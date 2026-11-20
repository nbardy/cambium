# Cambium

**Cheap, isolated Git worktrees for parallel coding agents.**

Running several agents on one repository means one worktree each. A plain
`git worktree add` writes a full copy of every tracked file, and then each
worktree needs its own `npm install` / `cargo build`. Ten agents on a large repo
can fill a laptop disk in days.

Cambium creates the same **real Git worktrees**, but on copy-on-write
filesystems (APFS, btrfs, XFS) they share disk with each other until a file
actually changes.

After creation it is ordinary Git: branches, `git status`, editors, and package
managers all work normally, and the worktree survives uninstalling Cambium.
Dependency folders can provide a warm start. Cambium distinguishes missing,
copied-but-unverified, and validated environments; it does not infer runnable
dependencies from the existence of a directory.

## Quick start

Requires Git 2.36 or newer. Cambium uses Git's
[NUL-delimited worktree listing](https://git-scm.com/docs/git-worktree/2.36.0)
to preserve paths containing newlines and other unusual characters.

```bash
go install github.com/nbardy/cambium/cmd/cambium@latest
go install github.com/nbardy/cambium/cmd/git-cambium@latest

cd my-repo
cambium init                                  # optional; writes local settings
git cambium add -b agent/auth --name agent-auth ../agent-auth main
cambium env explain agent-auth                # inspect missing/unverified paths
cd ../agent-auth                              # run your normal install/build checks

cd -                                         # return to the original checkout
cambium remove --delete-branch agent-auth     # when the agent is done
cambium prune                                 # drop old, unused caches
```

## Guarded removal, housekeeping and archiving

`remove` never loses work by default: a dirty tree or a commit that no remote
or other branch holds is refused (or, with `--salvage-ref`, saved at a ref and
reported). `prune`, `maintain` and the sweep on `add` use the same guard. Exit 3 + a typed reason on refusal:

```bash
# Reasons: unreadable, in-progress, branch-changed, in-use, in-use-unknown, primary, dirty,
# unpushed, submodule-unpushed, protected, unmerged, salvage-failed,
# output-conflict, output-unreadable.
# --require-merged: refuse unless HEAD is an ancestor of main
# --harvest: move git-ignored output/ files to the primary checkout first
#   (--harvest . --harvest-to ~/git/_rescued/DATE/NAME moves them elsewhere;
#   rebuildable dependency and compiler directories stay behind)
# --salvage-ref: snapshot a dirty tree (incl. untracked) and any unpushed
#   commits at a ref instead of refusing
# --force: the explicit opt-out (still never the primary checkout)
cambium remove --json --require-merged main --harvest output \
  --salvage-ref refs/salvage/agent agent-auth        # works from any directory

cambium status --all            # dirty, unpushed, merged, in-use, exclusive bytes
cambium prune --merged main     # remove clean workspaces already in main
cambium maintain --all-repos    # reconcile + forget --missing + prune caches
cambium maintain --all-repos --exclude ~/git/loft   # skip repositories you protect
```

A workspace is "in use" when `cambium run` holds its use marker or Cargo holds a
build lock. Run long-lived commands through `cambium run` so Cambium can track
them. If the records cannot be checked, including legacy PID-only markers,
guarded removal reports `in-use-unknown` and keeps the workspace.
With `workspace_root = "~/git/_wt"` in
`~/.config/cambium/config.toml`, `git cambium add -b NAME -name NAME` needs no
path.

Every removal first saves the workspace (untracked files included) as a hidden
checkpoint, `removed/<name>-<time>`, so even `--force` is recoverable:

```bash
cambium recover --removed                          # list
cambium recover --removed --path ~/git/_wt/back removed/agent-auth-20261119T101500Z agent-auth
```

`cambium run` writes a use marker named by its PID for the length of its child;
a dead PID's marker is ignored. Checks never kill, signal or wait.

## Dependency seeds for Cargo

A repository with a committed `Cargo.lock` gets `cargo-deps` seeding for its
ignored `target` directory on CoW filesystems. An explicit rule for `target`
takes precedence. Set `policy = "skip"` to leave the directory unmanaged.
To enable the seed explicitly, use this rule in `.cambium.toml`.

```toml
[[path]]
path = "target"
policy = "seed"
seed = "cargo-deps"
```

Run `cambium seed warm target` to build the committed Cargo workspace in a
temporary linked worktree and publish its registry and Git dependency artifacts
for both `cargo check` and `cargo build`. The cache is keyed by the lockfile,
compiler build, target triple and Cargo profile directory. Cargo checks
freshness in the receiving workspace, where workspace members and path
dependencies build locally.

`cambium remove` fills missing seeds from a workspace's existing build.
`cambium seed refresh [NAME]` publishes an existing build on demand. Add and
status record whether each seed was cloned, with a reason such as
`no_source_build`, `capture_failed` or `toolchain_mismatch` when it was cold.

```bash
# Replace a branch with a tag verified on the remote, then compare-and-delete it.
# Exit 3 + {"archived":false,"reason":...}: checked-out, missing, tag-conflict,
# push-unverified, moved. Rerunning after success returns {"already":true}.
cambium archive --json --remote origin agent/auth archive/agent-auth
```

## How it works

- **Tracked files** come from a per-commit baseline under `.git/cambium/`,
  cloned with copy-on-write. A new commit's baseline is cloned from the nearest
  existing one, so it costs only the files that changed.
- **Dependency and build folders** follow a policy per path: `node_modules` is
  cloned, Rust `target` is seeded as a warm start, `.venv` is recreated (venvs
  hold absolute paths). Only paths Git ignores *and* a rule names are touched;
  `.env`, logs, and databases never are. Defaults cover JS/TS, Python, Rust,
  JVM, Go, and more.
- **No CoW filesystem?** Tracked files use a normal Git checkout. Managed
  dependency folders use ordinary copies, including a cached copy; this can
  cost more space than plain worktrees on a non-CoW filesystem.

## Environment readiness

`status: ready` means the Git workspace was created successfully.
`environment_ready` describes the last observation of its declared environment
policies. A copied dependency directory without a successful validation hook is
reported in `environment_unverified`, and the environment is not marked ready.
Missing required dependencies appear in `environment_missing`.

Cambium refuses to snapshot a dependency folder against committed inputs when
the source checkout's live declared inputs differ. Even a clean checkout can
contain stale dependencies, so copying alone never establishes validation.

Projects can define a reviewed `validate` command in their path policy and opt
in locally. Validation runs in the destination workspace under its current
runtime; cached validation is not a substitute. `cambium env validate NAME`
explicitly reruns these checks. `env explain` does not run policy validation
commands.
Validation is a point-in-time observation, not a guarantee after later edits.
Build caches using `seed`, including Rust `target`, remain advisory: run the
owning compiler/build tool. See [environment receipts](docs/ENVIRONMENT_RECEIPTS.md).

## Configuration

Optional. Commit a `.cambium.toml` for project rules:

```toml
version = 1

[settings]
branch_prefix = "agent/"

[[path]]
path = ".venv"
policy = "recreate"
inputs = ["uv.lock"]
prepare = ["uv", "sync", "--frozen"]
```

Commands like `prepare` run only after you opt in locally
(`cambium init --allow-policy-commands`, stored in `.git/cambium/config.toml`),
because any branch can edit the committed file.
See [configuration](docs/CONFIGURATION.md) and
[path policies](docs/PATH_POLICIES.md).

## Use it from coding agents

`integrations/` has adapters so agents get Cambium worktrees automatically:

- **Claude Code:** `integrations/claude-worktree-hook` for the `WorktreeCreate` /
  `WorktreeRemove` hooks (needs `jq`). Worktrees go under
  `~/.claude/worktrees` (`CLAUDE_WORKTREE_ROOT` overrides).
- **Codex:** `export CODEX_WORKTREE_COMMAND=$PWD/integrations/codex-worktree-command`
  (needs a Codex build with `CODEX_WORKTREE_COMMAND` support).

Both put the agent's worktree on its own branch, mark it ephemeral, and keep any
commits the agent made when the worktree is removed. `make integration-test`
exercises both against real git.

## More

- **Cache retention:** `cambium prune --max-unused-bytes 2GiB` bounds retained
  unused cache content while preserving live workspaces and checkpoints. Add
  `--dry-run --json` to inspect the plan; use `0` to remove all unused entries.
  See [cache retention](docs/CACHE_RETENTION.md) for logical-byte accounting.
- **Checkpoints:** `cambium checkpoint` / `cambium fork` snapshot uncommitted
  work and branch from it without visible commits.
  See [structural sharing](docs/STRUCTURAL_SHARING.md).
- [Architecture](docs/ARCHITECTURE.md) ·
  [Ecosystem defaults](docs/ECOSYSTEM_DEFAULTS.md) ·
  [Security model](docs/SECURITY_MODEL.md) ·
  [Comparisons](COMPARISONS.md) · [Roadmap](docs/ROADMAP.md)

## Measure your workflow

The synthetic benchmark measures file materialization, not successful builds.
The [workflow benchmark](docs/WORKFLOW_BENCHMARKS.md) runs real npm/Cargo
commands, changes source and dependencies, reruns verification, and measures
cleanup and retained caches. It compares equal preparation and verification
steps against Git worktrees with copied environments. Raw reports include the
machine, toolchains, source revisions, and physical free-space observations.

Historical APFS measurements are described in the architecture document; they
do not establish post-build savings for another repository. Published results
and their limitations belong with the benchmark recipe and raw report.

Develop with `make check test-race system-test`. Validation and benchmarks run
locally; this repository has no GitHub Actions workflows. See the full
[validation gate](docs/VALIDATION.md) and [manual release process](docs/RELEASING.md).

Apache-2.0, Go, no runtime dependencies.
