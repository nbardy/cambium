# Cambium

**Cheap, ready-to-run Git worktrees for parallel coding agents.**

Running several agents on one repository means one worktree each. A plain
`git worktree add` writes a full copy of every tracked file, and then each
worktree needs its own `npm install` / `cargo build`. Ten agents on a large repo
can fill a laptop disk in days.

Cambium creates the same **real Git worktrees**, but on copy-on-write
filesystems (APFS, btrfs, XFS) they share disk with each other until a file
actually changes:

| Measured on an 802 MB, 16k-file repo (APFS) | Disk |
|---|---|
| `git worktree add` | ~810 MB each |
| Cambium, first worktree at a new commit | ~10 MB for a small diff |
| Cambium, another worktree at the same commit | ~10 MB |
| New worktree with a 609 MB `node_modules`, ready to run | ~25 MB |

After creation it is ordinary Git: branches, `git status`, editors, and package
managers all work normally, and the worktree survives uninstalling Cambium.

## Quick start

```bash
go install github.com/nbardy/cambium/cmd/cambium@latest
go install github.com/nbardy/cambium/cmd/git-cambium@latest

cd my-repo
cambium init                                  # optional; writes local settings
git cambium add -b agent/auth ../agent-auth main
cd ../agent-auth && npm test                  # deps are already there

cambium remove --delete-branch agent/auth     # when the agent is done
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
- **No CoW filesystem?** It falls back to a normal Git checkout.

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

## More

- **Checkpoints:** `cambium checkpoint` / `cambium fork` snapshot uncommitted
  work and branch from it without visible commits.
  See [structural sharing](docs/STRUCTURAL_SHARING.md).
- [Architecture](docs/ARCHITECTURE.md) ·
  [Ecosystem defaults](docs/ECOSYSTEM_DEFAULTS.md) ·
  [Security model](docs/SECURITY_MODEL.md) ·
  [Comparisons](COMPARISONS.md) · [Roadmap](docs/ROADMAP.md)

Develop with `make check test-race system-test`. Apache-2.0, Go, no runtime
dependencies.
