# Validation and performance evidence

## Correctness suite

The release gate is:

```bash
make check
make test-race
make system-test
git fsck --strict --no-reflogs
```

The tests use real temporary Git repositories and linked worktrees. Coverage
includes:

- tracked-file isolation, modes, symlinks, and Git status;
- APFS/reflink probing and ordinary-checkout fallback;
- prepared versus fresh indexes;
- clone, seed, recreate, share, empty, and skip policies;
- tracked/unignored/negated-ignore/symlink/path-traversal safety;
- branch-correct lockfile and `.cambium.toml` resolution;
- npm/Yarn/pnpm/Bun, Python, Cargo, Clojure/CLJS, Gradle, and Go fixtures through
  the generic path engine;
- policy-command trust boundaries;
- hidden source roots, exact subtree reuse, composite environment forks, and
  root/layer GC protection;
- concurrent creation, duplicate claims, interrupted operations, recovery, and
  cache pruning;
- ordinary Git worktree adoption, moves, removal, and reconciliation;
- SHA-1 and SHA-256 Git repositories.

## APFS proof

The repository includes a macOS CI job and `make benchmark-apfs`. The matrix
compares equal-semantics rows where possible:

```text
plain Git worktree
Git worktree + copied ignored environment
Cambium auto
Cambium CoW-required
simgit
cow
```

Measurements include physical free-space deltas, creation latency, first/warm
Git status, environment readiness, and cleanup. Logical `du` alone is not used
to claim CoW savings.

The comparison tools are optional. A missing or incompatible binary is reported
as skipped rather than replaced by an invented result.

## Current evidence boundary

Linux non-reflink hosts validate fallback and lifecycle behavior but cannot
prove APFS savings. Cambium must not claim it is faster than cow or simgit until
the published APFS result and machine details support that claim.

See [COMPARISONS.md](../COMPARISONS.md) and the APFS benchmark issue.

## v0.5.0 pre-publication verification

The release source was verified on Linux/amd64 with Go 1.23.2, Git 2.47.3,
and ext4 using:

```text
make check                 PASS
make test-race             PASS
make system-test           PASS
git diff --check           PASS
git fsck --strict          PASS
four-target cross-build    PASS
10x shuffled stress run    PASS
```

Aggregate in-process statement coverage was 55.6%. The generic environment
package measured 73.2%, configuration 75.8%, speculative roots 67.0%, native
workspaces 56.1%, and workspace orchestration 59.0%. Several thin executable
and subprocess wrappers are exercised by the system test but appear as zero in
in-process coverage.

A small non-reflink benchmark smoke test also passed and correctly reported
simgit/cow as unavailable rather than inventing comparison rows. These Linux
numbers are fallback evidence only; they are not evidence of APFS performance.
