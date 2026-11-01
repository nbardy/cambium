# Validation and performance evidence

## Correctness suite

Validation runs locally; the repository has no GitHub Actions workflows. Install
Git, Go 1.23+, Python 3.10+, and Node/npm before running the release gate:

```bash
make check
make test-race
make system-test
make integration-test
make workflow-test
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

The current regression suite also covers real offline npm installs with dirty
manifests and clean-but-stale installations, missing-cache upgrades, explicit
revalidation, linked-worktree revision context, recovery overlapping live
creation/removal, retained local work, and removal from the invoking worktree.
The npm regression requires a local Node/npm installation. A missing runtime
is a test skip, not a passed npm integration check. Binary releases are built
and published through the [manual release process](RELEASING.md).

## APFS proof

Run the synthetic allocation check locally on an APFS Mac:

```bash
make benchmark-apfs
```

This builds Cambium, writes `dist/benchmark-apfs.json`, and checks the recorded
APFS clone mode and physical allocation. The matrix compares equal-semantics
rows where possible:

```text
plain Git worktree
Git worktree + copied ignored environment
Cambium auto
Cambium CoW-required
simgit
cow
```

Measurements include physical free-space deltas, creation latency, first/warm
Git status, environment materialization, and cleanup. Logical `du` alone is not used
to claim CoW savings.

The comparison tools are optional. A missing or incompatible binary is reported
as skipped rather than replaced by an invented result.

Preserve the raw report with the Cambium source commit, toolchain versions, and
machine/filesystem details. Also run the real build/edit recipes below and
publish their reports and command logs manually. The synthetic check alone
does not establish post-build savings.

## Current evidence boundary

Linux non-reflink hosts validate fallback and lifecycle behavior but cannot
prove APFS savings. Cambium must not claim it is faster than cow or simgit until
the published APFS result and machine details support that claim.

See [COMPARISONS.md](../COMPARISONS.md) and the APFS benchmark issue.

## Real build workflows

See [WORKFLOW_BENCHMARKS.md](WORKFLOW_BENCHMARKS.md) for reproducible offline
npm/Cargo fixtures and pinned clsx/ripgrep recipes. The harness runs actual
builds/tests at initial, source-edit, and dependency-edit phases, then removes
worktrees and prunes caches. Reports retain command logs, revisions, toolchain
versions, physical free-space deltas, and residual state.

Synthetic benchmark schema 2 calls its file-existence counter
`environment_materialized`. It is not a build-readiness measurement. The older
schema-1 field `environment_ready` had the same synthetic meaning.

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
