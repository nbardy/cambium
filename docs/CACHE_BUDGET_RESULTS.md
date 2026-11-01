# Unused-cache retention budget — Linux, 2026-11-01

## Result and scope

The unused-cache byte budget retained the requested newest cache generation,
removed older unused generations, and preserved a checkpoint's environment for
successful replay after its original workspaces and primary environment were
removed. The run completed on November 1, 2026, at 13:38 Japan time
(04:38 UTC), using the published source in [PR #33](https://github.com/nbardy/cambium/pull/33).

In the selective scenario, a **16,780,114-byte unused budget** retained exactly
that many logical regular-file bytes and removed **12,586,272 logical bytes**.
The synchronized whole-volume observation recorded **12,627,968 bytes reclaimed**.
An additional **8,391,505 bytes** remained protected by a checkpoint, outside
the unused budget. A subsequent zero budget removed the remaining unused
generation while the checkpoint still replayed and passed its builds/tests.

This implements a user-controlled retention policy. It does not reduce the
extra immutable copy while a workspace or checkpoint still references it.
The existing `prune --older-than 1ns` command could already delete all unused
entries. The new option adds selective retention by size; it is not evidence
of faster copying or a new ability to reclaim otherwise undeletable cache.

The budget is applied when `prune` is invoked. It is not a background quota.
It counts regular-file lengths, including entry metadata, without following
symlinks. Referenced workspaces, pending operations, and retained checkpoint
snapshots are excluded. Whole entries are evicted, so retained bytes may fall
below the requested limit. Eviction uses recorded ready-marker timestamps
across both cache roots; those timestamps are updated by preparation/reuse,
not every possible reader. Logical bytes are not an exact physical-allocation
limit, particularly on filesystems with shared extents.

## Source, host, and workload

- Baseline Cambium source: `7c3d9a80b913f76ea014517a1661d17b0f0b7d40`.
- Measured integration source: `2c7e815eb21b99c5f425895feaec929224e14ec5`.
- Published PR source: [`2a16714b9e8baa68e076317308b66eb40d482f78`](https://github.com/nbardy/cambium/commit/2a16714b9e8baa68e076317308b66eb40d482f78).
  The published and measured trees both equal
  `00c8844abbab5c4321dfc0d2c4e57209e0863157`; GitHub commit metadata and local
  Git objects were independently checked. This records the published PR source,
  not a claim that the PR was already merged during measurement.
- Both binaries were built with Go 1.23.2 and `-trimpath -buildvcs=false`.
  Exact hashes and Go build information are in the provenance manifest.
- Linux x86_64, kernel 6.18.57, overlayfs; Intel Xeon Platinum 8573C,
  nine visible vCPUs, approximately 9.73 GiB reported memory.
- Git 2.51.1, Node 24.22.1, npm 11.9.0, Python 3.12.14.
  Cargo/Rust were unavailable and were not part of this run.
- One generated offline npm source commit,
  `dc6c86d44d8b34863a14dd268d4b48b10c972891`, tree
  `d4610274bd283acbabae8412fab0aafc7dda9a80`, was used by both workflow runs.

The fixture installs a real local npm dependency, builds a JavaScript module,
and runs Node tests. Source edits change the tested per-workspace result;
dependency edits install the second local dependency version and change the
expected result. A deterministic 16-MiB file under `node_modules/.cache` makes
retention observable. The build and trusted environment validator check its
length and complete content hash. This is a synthetic storage/correctness
fixture, not a maintained-project or monorepo performance result.

Each run contains **two workspaces per method** and a Git-worktree control that
copies the same prepared environment. The baseline run uses normal
`--older-than 168h` retention; the final run uses explicit
`--max-unused-bytes 0`. These are different requested policies. They do not
constitute an equal-policy speed comparison. Local build/test jobs were stopped
before measurement; a dedicated, otherwise idle physical host is not claimed.

## Actual workflow timings

Values are aggregate seconds for the two workspaces, except reference
preparation, which prepares one reference per method. This is one sequential
run per row; latency differences are descriptive observations only. All
initial, source-edit, and dependency-edit build/test phases passed.

| Method | Prepare reference | Create 2 | Initial build/test | Source edit/build/test | Dependency edit/build/test | Remove | Prune |
|---|---:|---:|---:|---:|---:|---:|---:|
| Before / Git + environment copy | 1.515 | 0.274 | 1.454 | 1.485 | 2.465 | 0.043 | 0.015 |
| Before / Cambium 168h age | 1.232 | 0.749 | 1.216 | 1.261 | 2.421 | 0.110 | 0.043 |
| After / Git + environment copy | 1.312 | 0.075 | 1.201 | 1.289 | 2.228 | 0.046 | 0.017 |
| After / Cambium zero budget | 1.188 | 0.642 | 1.267 | 1.312 | 2.110 | 0.076 | 0.049 |

## Physical allocation through the workflow

Values are MiB (1,048,576 bytes) added relative to each method's warmed
reference, observed from synchronized whole-volume free-space differences.
They include Cambium state and retained cache, plus same-volume log/report
writes and any unrelated activity. They are not sums of logical file sizes or
`st_blocks`.

| Method | After creation | After initial build/test | After source edit/build/test | After dependency edit/build/test | After removal | After prune |
|---|---:|---:|---:|---:|---:|---:|
| Before / Git + environment copy | 32.277 | 32.328 | 32.387 | 32.441 | 0.176 | 0.184 |
| Before / Cambium 168h age | 48.375 | 48.395 | 48.441 | 48.508 | 16.223 | 16.242 |
| After / Git + environment copy | 32.277 | 32.312 | 32.359 | 32.414 | 0.148 | 0.156 |
| After / Cambium zero budget | 48.359 | 48.395 | 48.441 | 48.508 | 16.223 | 0.211 |

Both Cambium runs used **48.508 MiB after dependency edits** and
**16.223 MiB after workspace removal**. The normal age policy left the fresh
cache retained; the explicit zero budget removed its two unreferenced entries,
accounting for **16,780,143 logical regular-file bytes**. Within the final run,
the prune phase observed **16,789,504 bytes reclaimed** from the volume.

After zero-budget pruning the remaining Cambium-state diagnostic contained
only the 165-byte local configuration file. The approximately 0.211-MiB
whole-volume residual includes Git changes, logs, reports, and observation
noise; it is not an unreclaimed environment-cache payload. The raw diagnostic
`logical_file_bytes` also counts symlink lengths, unlike the budget's explicit
regular-file-only accounting.

The Git control continues to use less space while the workspaces exist in
this non-CoW run. The extra pinned snapshot has not been eliminated.

## Selective retention and checkpoint replay

The second scenario created and built three generations with 8-, 12-, and
16-MiB payloads. It checkpointed the first generation and removed every
original workspace. The selected budget equals the complete newest unused
generation, including its metadata: **16,780,114 bytes**.

| Operation | Protected logical bytes | Eligible unused bytes before | Planned retained unused bytes | Actually removed logical bytes | Observed volume bytes reclaimed |
|---|---:|---:|---:|---:|---:|
| Selective dry-run | 8,391,505 | 29,366,386 | 16,780,114 | 0 | -8,192 |
| Selective prune | 8,391,505 | 29,366,386 | 16,780,114 | 12,586,272 | 12,627,968 |
| Zero budget, checkpoint pinned | 8,391,505 | 16,780,114 | 0 | 16,780,114 | 16,809,984 |
| After checkpoint release | 0 | 8,391,967 | 0 | 8,391,967 | 8,450,048 |

The dry-run selected the same entries as the subsequent prune and left the
recorded cache inventory and ready timestamps unchanged. Its negative volume observation
records 8 KiB of additional volume use during that phase; log/report writes
and unrelated filesystem activity can contribute. No cache was deleted. The actual
selective prune retained the newest generation and the pinned checkpoint,
while respecting the unused-byte limit.

A zero budget then removed the remaining unused generation and preserved the
checkpoint's two cache entries. The driver deleted the primary `node_modules`
directory; the primary source inputs already described generation three.
It forked the retained generation-one checkpoint and reran the build and tests.
The restored manifest and checked cache payload matched generation one.
After removing that replayed workspace, dropping the checkpoint alias, and
collecting the unreferenced root, a final zero-budget prune left both cache
roots empty.

The runtime also includes real-Git regression coverage for reference protection,
interrupted operations, dry-run, symlinks, global eviction ordering, and cache
publication coordination. See the [native budget tests](../internal/native/cache_budget_integration_test.go),
[workspace budget tests](../internal/workspace/cache_budget_integration_test.go),
and [CLI budget tests](../internal/cli/prune_budget_test.go).

## Raw evidence and reproduction

| Evidence | Report | Complete command logs |
|---|---|---|
| Driver, source setup, selective retention and replay | [cache-budget.json](benchmark-results/2026-11-01-linux/cache-budget.json) | [cache-budget-logs.tar.gz](benchmark-results/2026-11-01-linux/cache-budget-logs.tar.gz) |
| Baseline age-retention workflow and Git control | [cache-budget-before.json](benchmark-results/2026-11-01-linux/cache-budget-before.json) | [cache-budget-before-logs.tar.gz](benchmark-results/2026-11-01-linux/cache-budget-before-logs.tar.gz) |
| Final zero-budget workflow and Git control | [cache-budget-after.json](benchmark-results/2026-11-01-linux/cache-budget-after.json) | [cache-budget-after-logs.tar.gz](benchmark-results/2026-11-01-linux/cache-budget-after-logs.tar.gz) |

The [provenance manifest](benchmark-results/2026-11-01-linux/cache-budget-provenance.json)
records binary/script/report/archive hashes, source trees, build information,
and the fixture recipe. All **261 command log files** were checked against
their archives. Extract an archive beside its JSON report to restore the exact
relative log paths. Raw reports are unchanged from harness output.

Build both pinned Cambium revisions using the recorded Go toolchain and flags,
then use separate executable paths:

```bash
NPM_CONFIG_CACHE=/path/to/shared-writable-npm-cache \
python3 scripts/benchmark-cache-budget.py \
  --before-bin /path/to/baseline-cambium \
  --after-bin /path/to/final-cambium \
  --before-source-commit 7c3d9a80b913f76ea014517a1661d17b0f0b7d40 \
  --after-source-commit 2a16714b9e8baa68e076317308b66eb40d482f78 \
  --count 2 --payload-bytes 16777216 \
  --output dist/cache-budget.json
```

Choose a new output path; the driver refuses existing reports. `--scratch`
selects a parent filesystem for the source, both workflow runs, and the
selective-retention scenario. The driver removes its own successful scratch
repositories and retains failures with their evidence. Git commit timestamps
change when the disposable fixture is regenerated; both methods within a run
still use the exact same generated commit and tree.

This run does not establish APFS savings, concurrent throughput, large-project
performance, a physical-storage hard cap, or automatic cache maintenance.
The separately tracked APFS benchmark remains an evidence requirement.
