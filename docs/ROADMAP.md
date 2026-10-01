# Roadmap

## v0.5: transparent worktrees and generic environments

- [x] `git cambium add` with real linked-worktree registration.
- [x] Branch-correct `.cambium.toml` policy resolution.
- [x] `clone`, `seed`, `recreate`, `share`, `empty`, and `skip`.
- [x] Git-owned tracked/ignored safety boundary.
- [x] Built-in generic defaults for major language ecosystems.
- [x] Project-defined custom path policies.
- [x] Explicit trust gate for policy commands.
- [x] Target-tree environment receipts.
- [x] Composite source/environment speculative checkpoints.
- [x] Worktree adoption and reconciliation.
- [x] Popular ecosystem, safety, race, recovery, and system fixtures.
- [ ] Publish the target-hardware APFS comparison matrix.

## Next: root operations and orchestration

- [ ] Three-way root merge through `git merge-tree` with explicit conflicts.
- [ ] Watcher/fsmonitor-assisted incremental checkpoints.
- [ ] MCP and/or JSON-RPC orchestration.
- [ ] Process-group cleanup, port allocation, resource accounting.
- [ ] Optional container/VM sandbox adapters.
- [ ] Shell completion and richer review/landing flows.

## Environment distribution

- [ ] Optional remote transport for immutable clone/seed layers.
- [ ] Signed/trusted receipt metadata for shared teams.
- [ ] Toolchain-fingerprint hooks that remain generic and explicit.
- [ ] Cache size budgets and per-ecosystem pruning guidance.

## Native performance evidence

- [ ] Publish small-file, large-file, and real-monorepo APFS results against
  Git, simgit, and cow.
- [ ] Measure prepared index on/off.
- [ ] Compare recursive per-file clone with controlled root cloning of a private
  immutable baseline.
- [ ] Add guarded real-repository benchmark mode.

## Optional filesystem and storage research

Do not build FSKit/FUSE merely because persistent namespaces are elegant. A
mounted backend or auxiliary prolly/chunk CAS must first beat the native/Git
path on a measured workload while preserving Git, build, search, watcher,
locking, crash, and portability behavior.
