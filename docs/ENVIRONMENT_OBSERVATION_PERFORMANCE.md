# Avoiding unnecessary environment input scans

## Change and preserved behavior

A missing environment path or a path with no configured validator cannot be
reported as validated. Previously, applying and observing these paths still
created a private Git index, loaded the target tree, compared the working tree,
and enumerated untracked files. The result was then discarded and the path was
marked missing or unverified.

The implementation now decides these cases before running the input scan.
Checks used to associate source payloads with receipts remain in place during
capture and publication. A configured validator still runs in the destination,
and its post-validation input check still rejects inputs changed by the hook.
The new regression exercises that behavior for both clone and recreate policies
and for explicit environment validation.

This change reduces CPU, filesystem inspection, and subprocess overhead. It does
not alter payload copying, cache retention, receipt identity, or the meaning of
readiness.

## Controlled before/after measurement

Measured on 2026-10-30 with Linux x86_64, kernel 6.18.55, Git 2.51.1 and Go
1.23.2. Both binaries were built with `-trimpath -buildvcs=false`. The baseline
is commit `2937f5bf70d392d38498897a986f1f78c2d29f18`; the after binary contains
this optimization. Exact binary SHA-256 values and raw samples are in the
[JSON report](benchmark-results/2026-10-30-linux/environment-observation.json).

The disposable repository has 10,000 tracked 1-KiB source files, eight present
clone paths without validators, and one required missing clone path with a
configured validator. Both binaries use the same warmed environment cache.
Seven paired observations alternate before/after execution order. Git Trace2
process counts are measured separately from elapsed timings.

| Operation | Median before | Median after | Git subprocesses before → after |
|---|---:|---:|---:|
| `env explain` | 2.779092 s | 0.008732 s | 31 → 4 |
| `env validate` | 2.731970 s | 0.009142 s | 31 → 4 |
| `create` | 5.935398 s | 3.456818 s | 61 → 37 |

Workspace creation took **41.8% less elapsed time** for this fixture, from
5.935 to 3.457 seconds. The much larger observation improvement applies to these
unvalidated/missing paths: their unnecessary repository scans are eliminated.
It is not a general speed multiplier for Cambium or for successful validators.

The complete `env explain` and `env validate` JSON results were identical before
and after. Both versions kept the environment unready; explicit validation
still exited with status 1. Every created workspace also reported the expected
missing and unverified paths.

Physical free-space differences were synchronized and recorded for each sample.
Both binaries added exactly **42,393,600 bytes (40.43 MiB)** during each measured
creation. Observation commands added zero bytes in the sampled volume deltas.
This optimization does not claim a disk-space reduction.

## Reproduction

Build the baseline in a separate checkout so the current source stays intact:

```bash
git worktree add --detach /tmp/cambium-observation-before \
  2937f5bf70d392d38498897a986f1f78c2d29f18
go -C /tmp/cambium-observation-before build -trimpath -buildvcs=false \
  -o /tmp/cambium-observation-before-bin ./cmd/cambium
go build -trimpath -buildvcs=false \
  -o /tmp/cambium-observation-after-bin ./cmd/cambium
python3 scripts/benchmark-unverified-environments.py \
  --before-bin /tmp/cambium-observation-before-bin \
  --after-bin /tmp/cambium-observation-after-bin \
  --files 10000 --layers 8 --repetitions 7 \
  --output dist/environment-observation.json
```

Choose unused paths; the script refuses an existing output file and only
creates/removes its own temporary fixture. It removes ambient Git repository
overrides so fixture commands cannot target the caller's repository. Creation
warmup, Trace2 counting, and worktree removal are outside the timing samples.

## Scope

This is a controlled workload designed to expose repeated input scans. It is
not a large dependency-installation benchmark, an APFS measurement, a concurrency
measurement, or a claim about source capture. Projects with successful configured
validators retain their input checks and should not expect the observation
speedup measured here. Source capture still scans inputs even when it cannot
make a readiness claim, because receipt identity requires that check.
