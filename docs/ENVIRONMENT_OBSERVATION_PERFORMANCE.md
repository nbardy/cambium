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

## Merged build measured on 2026-10-31

The merged build reduced median creation time by **25.8%**, from **12.140950 s
to 9.002797 s**, in a fresh comparison with the original pre-optimization
baseline. This measurement includes the subsequent ownership checks and
workspace lifecycle fixes. The complete samples and Git process traces are in
the [final JSON report](benchmark-results/2026-10-31-linux/environment-observation-final.json);
exact source trees, build commands, versions, and binary hashes are in the
[provenance manifest](benchmark-results/2026-10-31-linux/environment-observation-final-provenance.json).

The baseline is `2937f5bf70d392d38498897a986f1f78c2d29f18`. The merged source is
[`3bb586bcbbb84c542f2e677469d88a031e13485d`](https://github.com/nbardy/cambium/commit/3bb586bcbbb84c542f2e677469d88a031e13485d).
Timing used the clean integration candidate
`c8bd732e414f6f6d72398c9130a005f7dac4c08d` while publication completed. Both commits
have tree `c9a22da1daeabdab3b9270d63d2a199682e86a1c`; a separate clean build of
the merged commit reproduced the measured binary's SHA-256 exactly:
`f28f64e9fd52ec674ca3e3c991204c109ac3a38309ab8fa44b1cdadcdc268ca4`.
The rebuilt baseline also exactly matched the original archived baseline binary.

The unchanged harness ran on Linux x86_64, kernel 6.18.57, with Git 2.51.1,
Go 1.23.2, and Python 3.12.14. Both binaries report Cambium 0.5.0 and were built
with `-trimpath -buildvcs=false`, so the recorded commit and binary hashes
identify the compared builds. Local build and test jobs finished before the
measurement window.

The disposable fixture contains 10,000 tracked 1-KiB source files, eight present
clone paths without validators, and one required missing clone path with a
configured validator. Seven paired observations alternate execution order.
Both binaries share a warmed environment cache; separate untimed runs count Git
processes using Trace2.

| Operation | Median baseline | Median merged build | Git subprocesses baseline → merged |
|---|---:|---:|---:|
| `env explain` | 5.707145 s | 0.024370 s | 31 → 5 |
| `env validate` | 5.556609 s | 0.032743 s | 31 → 7 |
| `create` | 12.140950 s | 9.002797 s | 61 → 37 |

The final observation counts include ownership verification. Validation also
checks ownership before publishing its metadata. Both commands produced the
same complete readiness JSON as the baseline; explicit validation still exited
with status 1, and every creation reported the expected missing and unverified
paths.

Creation times varied: baseline samples ranged from **11.568 to 14.297 s**, and
merged-build samples from **7.029 to 14.963 s**. Six of seven merged-build
samples were faster than their paired baseline; one was slower. The 25.8%
figure compares the two seven-sample medians and is specific to this workload
and run. The older measurements below used a different session and kernel, so
their absolute times should not be compared directly with this run.

All fourteen measured creations had a synchronized whole-volume used-space
increase of **42,393,600 bytes (40.43 MiB)**. All twenty-eight measured
observation commands had zero volume delta. These are signed free-space
observations, which can include unrelated filesystem activity; they establish
no disk-space saving. This change continues to leave payload copying and
retention costs unchanged.

## Original optimization measurement: 2026-10-30

This archived comparison predates the later ownership and lifecycle fixes.
Its 41.8% creation result describes the optimization build measured that day;
the merged-build result above is the applicable final verification.

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

Use Go 1.23.2 and build the pinned baseline and merged revision in separate
checkouts:

```bash
git worktree add --detach /tmp/cambium-observation-before \
  2937f5bf70d392d38498897a986f1f78c2d29f18
git worktree add --detach /tmp/cambium-observation-final \
  3bb586bcbbb84c542f2e677469d88a031e13485d
go -C /tmp/cambium-observation-before build -trimpath -buildvcs=false \
  -o /tmp/cambium-observation-before-bin ./cmd/cambium
go -C /tmp/cambium-observation-final build -trimpath -buildvcs=false \
  -o /tmp/cambium-observation-final-bin ./cmd/cambium
python3 /tmp/cambium-observation-final/scripts/benchmark-unverified-environments.py \
  --before-bin /tmp/cambium-observation-before-bin \
  --after-bin /tmp/cambium-observation-final-bin \
  --files 10000 --layers 8 --repetitions 7 \
  --output dist/environment-observation-final.json
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
