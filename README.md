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

Develop with `make check test-race system-test`. Apache-2.0, Go, no runtime
dependencies.
