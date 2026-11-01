# Measured build workflows — Linux, 2026-10-30

Both pinned projects completed five worktrees per method, with successful
initial, source-edit, and dependency-edit builds/tests, followed by removal and
cache pruning. This run establishes the tested workflow behavior and the cost
of the ordinary-copy fallback on this host. It does not establish APFS savings.

Cambium used more storage than Git worktrees with copied environments here.
For ripgrep, the extra allocation stayed near 681 MiB until cache pruning;
for clsx, it was about 4.9 MiB. Both Cambium cache diagnostics contained zero
files after pruning. There is no speed or space advantage to claim from these
Linux results.

## Host and source

- Linux x86_64, kernel 6.18.55, overlayfs; Cambium reported `git-checkout`
  for every workspace. No native CoW result is present.
- Nine visible vCPUs, AMD EPYC 9V74 host model, about 9.73 GiB reported RAM.
- Go 1.23.2; Git 2.51.1; Node 18.20.8/npm 10.8.2; Rust/Cargo 1.99.1.
- `CARGO_BUILD_JOBS=4`; package/tool caches warmed before measurement.
- This change set was built on base commit
  `285a0c6c04a19842ade78f984c29cd56b429f0c2`. The binary still identifies
  itself as 0.5.0; the raw reports record its exact SHA-256,
  `e17e639ef5e5a4f2c04c5ac73830823bb0d63a7f763baa7540b288a179821f71`,
  and the benchmark harness SHA-256.

| Project | Source and verification scope |
|---|---|
| clsx | Upstream [`925494cf`](https://github.com/lukeed/clsx/commit/925494cf31bcd97d3337aacd34e659e80cae7fe2), plus the pinned benchmark lockfile, yielding `202794065c925e7f4dcab436150e8a24b360a697`. Builds distributables and runs all 32 upstream tests. Source changes add a comment; dependency changes upgrade uvu 0.5.4 to 0.5.6 and reinstall offline. |
| ripgrep | Upstream [`91f5d22e`](https://github.com/BurntSushi/ripgrep/commit/91f5d22ef48bde459d90a8c7dbeab416e97d0f2b). Builds `rg` and runs workspace library tests excluding optional `grep-pcre2`. Source changes add a comment; dependency changes bump the local `grep-searcher` version and rebuild/update the lockfile. |

The ripgrep recipe excludes CLI integration tests: upstream
`feature::f1414_no_require_git` failed on this review host before Cambium was
involved. It is therefore not a full upstream-suite result. The maintained
project edits exercise relocation and rebuild behavior with narrow changes,
not major implementation divergence. The separate offline fixtures do make
behavior-changing source and dependency edits and test the changed results.

## Timings

Times below are aggregate seconds for all five worktrees, not per-workspace
latencies. Each row is one sequential run. Reference preparation is shown
separately and is not included in creation time.

| Project / method | Prepare reference | Create 5 | Initial build/test | Source edit/build/test | Dependency edit/build/test | Remove | Prune |
|---|---:|---:|---:|---:|---:|---:|---:|
| clsx / Git + environment copy | 1.320 | 0.286 | 2.864 | 2.801 | 4.869 | 0.095 | 0.012 |
| clsx / Cambium auto | 1.421 | 0.648 | 2.895 | 2.996 | 4.915 | 0.192 | 0.044 |
| ripgrep / Git + environment copy | 11.519 | 3.018 | 36.238 | 10.845 | 26.178 | 0.884 | 0.015 |
| ripgrep / Cambium auto | 11.361 | 3.910 | 36.256 | 11.406 | 25.147 | 0.907 | 0.139 |

## Allocated space through the workflow

Values are MiB (1,048,576 bytes) added relative to each method's warmed
reference, measured from synchronized filesystem free-space differences.
They cover all five worktrees and Cambium state, including its retained
environment cache. They are not sums of logical file sizes or `st_blocks`.

| Project / method | After creation | After initial build/test | After source edit/build/test | After dependency edit/build/test | After removal | After prune |
|---|---:|---:|---:|---:|---:|---:|
| clsx / Git + environment copy | 24.93 | 25.10 | 25.21 | 25.33 | 0.34 | 0.34 |
| clsx / Cambium auto | 29.83 | 30.00 | 30.11 | 30.22 | 5.18 | 0.38 |
| ripgrep / Git + environment copy | 3425.46 | 4628.58 | 4480.77 | 5642.91 | 0.33 | 0.34 |
| ripgrep / Cambium auto | 4106.68 | 5309.83 | 5162.01 | 6324.17 | 681.49 | 0.38 |

Rust rebuilds increase the allocation substantially for both methods. The
initial clone/copy measurement alone would miss that cost. The additional
Cambium copy remains after worktree removal and is released by `cambium prune`.
The small post-prune whole-volume residuals include report/log growth and
measurement noise; the Cambium cache tree itself contains zero files.

## Readiness and passing tests are separate observations

The clsx creation records report `environment_ready: false` with
`environment_unverified: ["node_modules"]`, because this recipe does not
configure a trusted policy validator. The benchmark independently executes
and records successful builds/tests. Copying the directory no longer produces
an unsupported readiness claim.

Rust `target` is an advisory seed, so its readiness metadata does not assert
compiler or dependency correctness. The recorded Cargo commands perform that
verification. A successful project validator is itself a point-in-time check
scoped to what that validator tests.

## Raw evidence and reproduction

| Project | JSON report | Exact command logs |
|---|---|---|
| clsx | [clsx.json](benchmark-results/2026-10-30-linux/clsx.json) | [clsx-logs.tar.gz](benchmark-results/2026-10-30-linux/clsx-logs.tar.gz) |
| ripgrep | [ripgrep.json](benchmark-results/2026-10-30-linux/ripgrep.json) | [ripgrep-logs.tar.gz](benchmark-results/2026-10-30-linux/ripgrep-logs.tar.gz) |

Extract a log archive beside its JSON report to restore the exact relative
log paths recorded in the report. Reports retain the full recipe, source
commit/tree, timestamps, tool versions, command exit codes, and signed byte
observations. Successful temporary workspaces were cleaned up.

Use the pinned preparation helper and commands in
[WORKFLOW_BENCHMARKS.md](WORKFLOW_BENCHMARKS.md) to reproduce the workload.
clsx requires the documented Node 18 runtime for its upstream legacy test
loader. Use the same writable npm cache during preparation and measurement.

## Limits and outstanding evidence

- One trial per project/method, five sequential worktrees. These are descriptive
  observations, not a statistically established performance comparison.
- Methods share a warmed toolchain and package caches; all Git-control phases
  run before Cambium. The control explicitly copies the same prewarmed output
  paths: `node_modules` plus `dist`, or Cargo `target`.
- Volume measurements can include other writes and release timing. Reports
  preserve signed measurements; they do not substitute logical sizes when
  physical observations are inconvenient.
- clsx is a small library. Neither workload establishes monorepo-scale
  behavior, parallel throughput, or broad dependency-change performance.
- APFS post-build allocation and timings remain unmeasured in these artifacts.
  Run the [local Mac recipes](WORKFLOW_BENCHMARKS.md) with an explicit
  `cambium-cow` row and publish their raw reports and command logs to supply
  that evidence.
- Comparison tools such as simgit and cow are not part of these two-method
  workflow results. No relative claim about them follows from this run.

## Subsequent runtime optimization

A separate [controlled before/after measurement](ENVIRONMENT_OBSERVATION_PERFORMANCE.md)
covers the removal of unnecessary input scans for unverified or missing
environment paths. It does not change the fallback-copy storage costs above.
