# Cambium implementation plan

## Product boundary

Cambium is a local-first Git workspace engine:

- **Git** owns tracked source, commits, trees, refs, branches, merges, and the
  linked-worktree registry.
- **The native filesystem** owns ordinary workspace I/O and physical CoW.
- **Cambium** owns worktree construction, ignored-path policy, environment
  receipts, speculative roots, recovery, and orchestration metadata.

A mounted FSKit/FUSE backend is not part of the default architecture.

## v0.5 release scope

### Transparent Git worktrees

- [x] Build `git-cambium` so `git cambium ...` works naturally.
- [x] Add `git cambium add -b BRANCH PATH [COMMIT]`.
- [x] Keep `cambium create` for compatibility and managed default paths.
- [x] Register every execution workspace through Git's linked-worktree API.
- [x] Add `path`, `adopt`, and `reconcile` commands.
- [x] Treat Git's worktree registry as authoritative after moves/removals.

### Generic path-policy engine

- [x] Add `clone`, `seed`, `recreate`, `share`, `empty`, and `skip`.
- [x] Require concrete output paths; allow Git-style globbing for inputs.
- [x] Use Git's own tracked/ignored classification.
- [x] Leave unknown ignored paths unmanaged.
- [x] Reject tracked overlays, unignored outputs, negated-ignore children,
  path traversal, and unexpected symlinks.
- [x] Add project-defined `.cambium.toml` policies.
- [x] Resolve `.cambium.toml` from the exact target Git tree.
- [x] Require an explicit operational trust flag before repository policy
  commands may execute.

### Ecosystem defaults

- [x] JavaScript and TypeScript: npm, Yarn, pnpm, Bun-style outputs.
- [x] Python: uv/pip/Poetry/PDM-style inputs and recreate-only virtual envs.
- [x] Rust/Cargo build seeds.
- [x] Clojure, ClojureScript, Leiningen, Clojure CLI, shadow-cljs caches.
- [x] JVM/Maven/Gradle outputs.
- [x] Go, Ruby/PHP, Elixir, Dart, .NET, Swift, and C/C++ common outputs.
- [x] Keep all defaults as data passed through the generic engine.

### Branch-correct environment receipts

- [x] Fingerprint matching target-tree Git blob IDs, not primary checkout files.
- [x] Include platform, policy semantics, commands, and source identity when
  required.
- [x] Prevent incompatible branch environments from being reused.
- [x] Capture validated live clone/seed paths atomically.
- [x] Preserve metadata-only recreate/share/empty/skip receipts.
- [x] Protect layers referenced by speculative roots from cache pruning.

### Composite speculative state

- [x] Retain Git-native hidden Merkle roots for dirty source.
- [x] Attach environment receipt IDs to root metadata.
- [x] Fork source plus matching clone/seed/recreate policies coherently.
- [x] Keep the real branch and staging index unchanged during checkpointing.

### Reliability and validation

- [x] Retain operation journals, cross-process locks, rollback, and advanced
  branch protection.
- [x] Test real Git worktrees, moves, adoption, reconciliation, and deletion.
- [x] Test popular ecosystem defaults through filesystem fixtures.
- [x] Test branch-specific policy and lockfile differences.
- [x] Test policy-command trust, secrets, tracked paths, and ignore negation.
- [x] Run unit, race, system, stress, cross-build, and Git fsck validation.
- [ ] Publish the equal-semantics APFS matrix on target hardware.

## v0.6 candidates

- Three-way speculative-root merge through `git merge-tree`.
- Watcher/fsmonitor-assisted incremental checkpoints.
- MCP or JSON-RPC orchestration.
- Process groups, port allocation, resource accounting, and sandbox adapters.
- Optional remote transport for prepared environment layers.
- Shell completion and richer workspace review/landing commands.

## Custom filesystem gate

A native filesystem implementation begins only when measurements demonstrate
all of the following:

1. namespace materialization dominates startup after CoW, prepared indexes, and
   sparse checkout;
2. the agent working set is sparse enough that lazy access should win;
3. existing ArtifactFS/EdenFS-style options are unsuitable for the workload;
4. the advantage survives Git status, search, builds, indexing, and watchers;
5. the project can maintain filesystem conformance and crash-consistency tests.

Until then, APFS/reflinks provide block sharing without putting Cambium in the
live I/O path.
