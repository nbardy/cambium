# AGENTS.md

Cambium is a local-first Git workspace engine. It is not a replacement SCM or
filesystem and must not quietly grow a second source authority beside Git.

## Invariants

1. Every execution workspace is a real Git linked worktree.
2. `auto` may use CoW or Git checkout, but never silently baseline-copy on a
   non-CoW filesystem.
3. Git owns every tracked path; environment policies may not overlay it.
4. Unknown ignored paths remain unmanaged.
5. Custom unignored paths require an explicit escape hatch.
6. `.cambium.toml` is resolved from the exact target Git tree.
7. Repository policy commands are disabled unless local operational trust is
   explicitly enabled.
8. Environment receipts use target-tree object IDs, not primary checkout bytes.
9. Shared Git administration must hold the appropriate cross-process lock and
   recheck name/branch/path invariants under it.
10. Cleanup removes only paths proven owned by the failing operation.
11. Recovery never deletes an advanced branch automatically.
12. Metadata writes and cache publication are atomic.
13. Speculative source roots use Git's object DAG; do not add another source
    store without a measured decision change.
14. Checkpointing must not modify the visible branch or real index.
15. Performance claims require physical-allocation and timing evidence; a
    missing comparison tool is reported as skipped.

## Package ownership

- `internal/native`: native data path, environment receipts, recovery.
- `internal/environment`: built-in generic path-policy data.
- `internal/config`: operational JSON and strict policy TOML.
- `internal/gitx`: all Git subprocess and target-tree behavior.
- `internal/speculative`: hidden source roots, aliases, diff, restore, GC.
- `internal/workspace`: composition, adoption, reconciliation, checkpoints.
- `internal/operation`: durable staged operations.
- `internal/bench`: comparative evidence.

## Required checks

```bash
make fmt
make check
make test-race
make system-test
git fsck --strict --no-reflogs
```

Create/remove/recovery or concurrency changes require real-repository
integration tests and race-enabled coverage.
