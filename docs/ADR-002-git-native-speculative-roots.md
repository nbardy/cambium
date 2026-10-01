# ADR-002: Represent speculative dirty state with hidden Git Merkle roots

- Status: accepted
- Date: 2026-08-17

## Context

Parallel coding agents frequently need to branch from dirty, uncommitted state.
Forcing every checkpoint into visible branch history is noisy. The v0.1 full-
tree Merkle experiment duplicated Git and was disconnected from live
worktrees. A later prototype narrowed that idea to a custom persistent radix
overlay, but still duplicated Git's object/ref/diff/GC machinery.

Git already stores source as an immutable Merkle DAG and supports private
indexes, tree creation, hidden refs, integrity checks, and tree diff/merge.

## Decision

A Cambium speculative root is a deterministic hidden Git commit:

```text
commit tree = visible checkpoint tree
commit parent = workspace base commit
commit message/identity/time = fixed Cambium v1 values
```

The checkpoint tree is produced from a temporary copy of the linked worktree's
index. Cambium stages visible state into that temporary index, writes the tree,
checks snapshot coherence, and leaves the real index untouched.

Human-facing aliases and immutable root pins live under hidden
`refs/cambium/speculative/...` namespaces. Metadata outside the Git object DAG
stores alias names, descriptions, source workspace, parent-root lineage, and
creation time.

Forking creates a normal linked worktree at the base commit and restores paths
from the root commit into the worktree only, leaving its branch HEAD at the
base. The result is normal dirty source state.

## Consequences

### Positive

- Uses Git's existing structural sharing and content-addressed storage.
- Same base and visible tree produce the same deterministic root ID.
- Does not modify visible branches or the user's staging area.
- Reuses Git file modes, symlink objects, refs, fsck, packs, transport, diff,
  and future merge-tree behavior.
- No filesystem daemon in the live I/O path.
- Less code, lower checkpoint latency, and lower metadata overhead than the
  rejected custom radix prototype in the design fixture.

### Negative

- A checkpoint represents visible files, not staged-versus-unstaged intent.
- Git attributes and clean filters apply when the temporary index stages data.
- Large changed binary blobs are not immediately content-defined-chunked;
  pack delta compression happens under Git's storage policy.
- Checkpointing is explicit rather than continuously synchronized.
- Materializing an executable native workspace remains proportional to the
  affected namespace and underlying worktree creation.
- Git directory tree objects are not a HAMT/prolly tree: a change in a very
  large flat directory rewrites that directory's tree object, and checkpoint
  capture currently scans/stages visible source state.

## Rejected alternatives

- **Full duplicate Merkle namespace:** duplicates Git authority.
- **Custom persistent radix/chunk CAS:** valid computer science, but measured
  worse on the source checkpoint workload and duplicates mature Git features.
- **Rewrite ArtifactFS in Rust:** changes implementation language without
  changing its mounted/lazy-hydration tradeoff.
- **Prolly-tree source store now:** potentially attractive for huge flat
  namespaces and change-proportional diff, but still a second source authority
  before Git checkpointing has failed a real workload.
- **Build FSKit/FUSE first:** incurs filesystem-semantics work before a measured
  need.
- **Visible checkpoint commits:** simpler, but pollutes ordinary branch history
  and makes disposable speculation harder to distinguish.
