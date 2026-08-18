# Cambium

**Git-native, ready-to-build workspaces for parallel coding agents.**

Cambium creates **real linked Git worktrees**, but populates them more
intelligently:

- tracked source can share physical blocks through APFS clones or Linux
  reflinks;
- ignored dependency/build paths follow conservative `clone`, `seed`,
  `recreate`, `share`, `empty`, or `skip` policies;
- dirty source state can be checkpointed into hidden Git Merkle roots and
  forked without adding disposable commits to visible branch history;
- ordinary Git, editors, compilers, package managers, and file watchers operate
  on normal native files after creation.

Cambium is **not a replacement for Git** and is **not a mounted filesystem**.
Git owns branches, commits, tracked files, refs, and the worktree registry.
Cambium is an optimized constructor and lifecycle layer above those primitives.

## Quick start

```bash
go install github.com/nbardy/cambium/cmd/cambium@latest
go install github.com/nbardy/cambium/cmd/git-cambium@latest

cd my-repository
cambium init

git cambium add -b agent/auth ../agent-auth main
cd ../agent-auth

# Everything from here is ordinary Git and ordinary package-manager usage.
git status
npm test
```

The created workspace appears in `git worktree list` and remains usable even if
Cambium is later uninstalled.

## What happens at creation

```text
Git target commit
      │
      ├─ register linked worktree with --no-checkout
      ├─ populate tracked files from an immutable CoW baseline
      ├─ install a matching Git index
      ├─ apply safe ignored-path policies
      └─ verify ordinary Git status and record recovery metadata
```

On filesystems without native CoW support, `auto` falls back to Git's normal
checkout rather than pretending to save disk.

## Filesystem policies, not package-manager implementations

Cambium does not parse Yarn lockfiles, resolve Cargo dependencies, or implement
Python environments. Its generic engine combines:

```text
Git ownership (tracked / ignored / untracked)
+
known output path
+
input file identities from the exact target Git tree
+
filesystem policy
```

Built-in defaults cover common JavaScript/TypeScript, Python, Rust,
Clojure/ClojureScript, JVM, Go, Ruby/PHP, Elixir, Dart, .NET, Swift, and C/C++
paths. Unknown ignored paths remain untouched. Projects with custom tooling can
add `.cambium.toml`; there is no special `yarn.toml`, npm parser, or Cargo
adapter.

See [ecosystem defaults](docs/ECOSYSTEM_DEFAULTS.md),
[path policies](docs/PATH_POLICIES.md), and
[configuration](docs/CONFIGURATION.md).

## Speculative checkpoints

```bash
cambium checkpoint --as parser-base agent/parser
cambium fork parser-base agent/pratt
cambium fork parser-base agent/recursive
cambium root diff parser-base parser-later
```

Cambium captures visible dirty source through a private Git index, writes normal
Git tree/blob objects, and pins a deterministic hidden commit. The real branch
and staging area remain unchanged. Composite checkpoints also retain compatible
prepared-environment receipt IDs.

See [structural sharing](docs/STRUCTURAL_SHARING.md) and the
[Git-native root ADR](docs/ADR-002-git-native-speculative-roots.md).

## Where Cambium fits

| Need | Strongest baseline |
|---|---|
| Plain multiple branch checkouts | Git worktree |
| Narrow disk-efficient linked worktrees | simgit |
| Instant whole-directory Mac clones | cow |
| Huge remote repo with sparse reads | ArtifactFS / EdenFS-style systems |
| Linked worktrees + explicit environments + dirty-state forks | Cambium's focus |

Cambium is broader than simgit and more Git-native than cow's default clone
model, but those tools are currently more mature in their narrower strengths.
Cambium does not claim a performance win until the equal-semantics APFS matrix
is published. See [COMPARISONS.md](COMPARISONS.md).

## Documentation

- [Implementation plan and release gates](docs/PLAN.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Configuration](docs/CONFIGURATION.md)
- [Path policies and safety](docs/PATH_POLICIES.md)
- [Environment receipts](docs/ENVIRONMENT_RECEIPTS.md)
- [Ecosystem defaults](docs/ECOSYSTEM_DEFAULTS.md)
- [Security model](docs/SECURITY_MODEL.md)
- [Validation and benchmarks](docs/VALIDATION.md)
- [Roadmap](docs/ROADMAP.md)

## Development

```bash
make check
make test-race
make system-test
```

The project is Apache-2.0 licensed and uses Go 1.23 with no runtime library
dependencies.
