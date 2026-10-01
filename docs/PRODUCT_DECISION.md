# Product decision: Git-native worktrees, generic environments, hidden roots

## Decision

Cambium is a local-first agent workspace engine with three cooperating layers:

1. **Execution:** real Git linked worktrees and native APFS/reflink
   materialization with normal Git fallback.
2. **Environment:** generic ignored-path policies and branch-correct receipts.
3. **Speculation:** deterministic hidden Git roots plus retained environment
   receipt IDs.

Do not build or fork a mounted filesystem in the current product. Do not own a
second content-addressed source store while Git represents the same state.

## Why

- Git already supplies immutable source objects, refs, diff, merge, transport,
  integrity checking, and linked-worktree semantics.
- APFS/reflinks already supply physical block sharing for ordinary files.
- Native files preserve compiler, editor, watcher, search, and Git behavior.
- The missing product layer is safe ignored-state semantics and recoverable
  agent lifecycle—not another block-sharing implementation.
- ArtifactFS/EdenFS-style virtualization is compelling for huge sparse/remote
  repositories but adds a daemon/filesystem surface to local hot repositories.

## Retained ideas

- one shared Git repository;
- immutable tracked baselines and prepared indexes;
- filesystem CoW with honest fallback;
- Clojure-like persistent speculation through Git's Merkle DAG;
- explicit environment compatibility receipts;
- durable operations, recovery, locking, pruning, and benchmarks.

## Rejected defaults

### Whole live-directory cloning

Useful and fast, but too indiscriminate as Cambium's semantic default. Secrets,
live databases, sockets, PID files, and unrelated ignored state should not be
copied merely because they exist.

### Custom source Merkle/radix/prolly store

A prototype had real structural sharing but duplicated Git's mature authority
and measured worse on the retained source-checkpoint fixture.

### ArtifactFS or EdenFS fork

Their lazy mounted model addresses remote/sparse and extreme-monorepo workloads.
Cambium's initial workload is an already-local repository with frequent native
Git/search/build operations.

### Rust rewrite

Changing the control-plane language does not alter Git subprocess,
materialization, or filesystem costs. Rust becomes justified only for a future
in-process data plane whose measurements earn it.

## Current versus ideal

| Capability | v0.5 | Ideal end state |
|---|---|---|
| Workspaces | Real linked worktrees | Same, with adaptive measured materialization |
| Environments | Generic local receipts/layers | Optional trusted remote layer transport |
| Defaults | Major ecosystems via path data | Community-maintained policy library |
| Dirty state | Hidden deterministic Git roots | Incremental scheduling and root merge |
| Lifecycle | Locks, journals, recovery, reconcile | Full agent runtime and sandbox adapters |
| Evidence | System/race/fallback tests | Published APFS and real-monorepo dashboard |
| Mounted FS | None | Optional only after the benchmark gate |

## Mounted-filesystem gate

Implementation begins only if:

1. namespace materialization still dominates after CoW, prepared indexes, and
   sparse checkout;
2. the working set is sparse enough for lazy access to win;
3. existing virtual filesystems cannot satisfy the workload;
4. the advantage survives Git status, search, builds, indexing, and watchers;
5. the project can sustain filesystem conformance and crash-consistency work.
