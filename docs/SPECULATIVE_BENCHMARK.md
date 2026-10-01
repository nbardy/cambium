# Speculative-root design benchmark

- Date: 2026-08-17
- Host: Linux/amd64, local ext-family filesystem
- Fixture: 5,000 tracked paths, three dirty paths
- Purpose: choose the checkpoint representation, not publish a cross-platform SLA

## Compared designs

### Rejected custom store

The discarded prototype used a canonical SHA-256 object store, persistent
path-compressed radix nodes, path-copy updates, content-defined file chunks,
file manifests, named refs, mark/sweep GC, replay, and structural diff.

### Accepted Git-native store

The accepted implementation copies the worktree index to a private file,
stages visible state into it, calls `git write-tree`, creates a deterministic
single-parent hidden commit with `git commit-tree`, and pins roots/aliases with
hidden refs. It also performs coherence checks and durable metadata updates.

## Retained design-run results

| Path | Median end-to-end checkpoint | Added storage after 11 aliases |
|---|---:|---:|
| Custom radix/chunk prototype | ~70.3 ms | ~148 KiB |
| Git-native Cambium CLI | ~56.5 ms | ~88 KiB |
| Git tree-write core only | ~10.4 ms | ~28 KiB of Git objects in that run |

These are retained measurements from one design run. The custom prototype was
intentionally deleted after the decision, so this table is historical evidence,
not a continuously reproducible release benchmark. It should not be used as a
universal performance claim.

The release keeps a reproducible benchmark for the accepted path:

```bash
go test -run '^$' \
  -bench BenchmarkGitMerkleCheckpointOneChangeInFiveHundredTwelvePaths \
  -benchtime=10x -count=3 -benchmem \
  ./internal/speculative
```

A release validation run on the same date reported:

```text
31.29 ms/op   681,675 B/op   3,267 allocs/op
37.26 ms/op   667,448 B/op   3,258 allocs/op
34.43 ms/op   667,046 B/op   3,257 allocs/op
```

That benchmark includes Git subprocesses, three coherence observations,
deterministic commit creation, hidden-ref publication, and durable JSON
metadata. It is intentionally not reduced to the `write-tree` core.

## Decision rule

The custom structure would have been justified only if it materially improved a
named workload after accounting for all features it would need to reproduce:
Git file modes, symlinks, tree identity, refs, locking, `fsck`, packfiles,
transport, rename-aware diff, and merge machinery. It did not. Git therefore
remains the source checkpoint DAG. A separate chunk CAS may return only for
measured large non-Git environment artifacts.
