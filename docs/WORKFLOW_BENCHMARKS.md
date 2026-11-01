# Real build workflow benchmarks

This harness answers a narrower, useful question: what does an isolated
workspace cost through actual builds, source edits, dependency edits, and
cleanup? It records command success separately from Cambium's environment
readiness metadata. It does not equate a copied directory with a passing build.

## Offline smoke workflows

Requirements: Git, Python 3.10+, Go 1.23+, Node/npm, and Cargo/Rust.

```bash
make workflow-smoke
```

This builds Cambium, runs the harness safety tests, and exercises two worktrees
per method for each fixture. Reports and command logs are written under
`dist/`.

The npm fixture installs a real local dependency, builds a JavaScript module,
and runs Node tests. Source edits change each workspace's tested output;
dependency edits install v2 of the local dependency and update expectations.
Its reviewed validation policy checks the actual installed version and value.
The Cargo fixture compiles a Rust crate with a local path dependency, runs its
tests, changes source, then changes the dependency's version and implementation
and reruns the compiler/tests. These are reproducible correctness smoke tests,
not evidence about a large monorepo's performance.

## Pinned maintained projects

`scripts/prepare-workflow-project.py` creates a new disposable source checkout,
downloads the relevant dependencies before measurement, and writes a recipe.
It refuses existing destination and recipe paths. It never resets an existing
working repository.

| Project | Pinned upstream | Work performed | Runtime / scope |
|---|---|---|---|
| clsx | `925494cf31bcd97d3337aacd34e659e80cae7fe2` | Build distributables, run all 32 upstream tests, edit source, change uvu 0.5.4 to 0.5.6, rebuild/test | Node 18.20.8; the upstream suite uses the legacy `esm` loader. A checked-in benchmark lockfile makes a deterministic derived source commit. Small library, not a monorepo. |
| ripgrep | `91f5d22ef48bde459d90a8c7dbeab416e97d0f2b` | Build `rg`, run workspace library tests, edit source, bump the local `grep-searcher` dependency version, rebuild/test | Rust >=1.96; excludes optional PCRE2 and CLI integration tests. The upstream `f1414_no_require_git` integration test failed in the review host before Cambium was involved, so this recipe explicitly measures the library-test suite. |

For clsx, select Node 18 before these commands. Choose new paths for each run:

```bash
export NPM_CONFIG_CACHE="${TMPDIR:-/tmp}/cambium-npm-cache"
python3 scripts/prepare-workflow-project.py \
  --project clsx \
  --destination /tmp/cambium-clsx-source-new \
  --recipe /tmp/cambium-clsx-recipe-new.json

make build
python3 scripts/workflow-benchmark.py \
  --recipe /tmp/cambium-clsx-recipe-new.json \
  --count 5 --cambium-bin "$PWD/bin/cambium" \
  --output dist/workflow-clsx.json
```

Use `--project ripgrep` and separate paths for the Rust recipe. Set
`CARGO_BUILD_JOBS=4` to keep the local workload reproducible. Both recipes list
all prewarmed outputs copied by the control: `node_modules` and `dist` for clsx,
and `target` for ripgrep.
Keep the same writable `NPM_CONFIG_CACHE` in preparation and measurement so
offline dependency changes can read the warmed registry metadata and packages.

On an APFS Mac, require and record the CoW path as well:

```bash
python3 scripts/workflow-benchmark.py \
  --recipe /tmp/cambium-ripgrep-recipe-new.json \
  --count 5 --methods git-env-copy,cambium-auto,cambium-cow \
  --cambium-bin "$PWD/bin/cambium" \
  --output dist/workflow-ripgrep-apfs.json
```

`cambium-cow` fails explicitly if native sharing is unavailable. It is not
silently replaced with a copying result. Run these recipes locally on Linux
and on an APFS Mac. Preserve the raw JSON report and per-command logs, including
on failure; the repository has no GitHub Actions workflows to run or upload
them. Publish the successful reports and logs with the source commit,
toolchain versions, and machine/filesystem details before making APFS claims.

## Benchmark your own repository

Pass `--recipe recipe.json`. The source checkout is only read: each method uses
its own private clone at the recorded commit. Recipe commands execute project
code with your toolchain/environment; they are explicit argv arrays rather
than interpolated shell commands. A recipe is trusted executable input, not a
sandbox.

```json
{
  "version": 1,
  "name": "my-rust-project",
  "source": "/absolute/path/to/project",
  "ref": "an-exact-commit-id",
  "environment_paths": ["target"],
  "prepare": [["cargo", "build", "--locked"]],
  "verify": [["cargo", "build", "--locked"], ["cargo", "test", "--locked"]],
  "source_edits": [
    {"path": "src/lib.rs", "old": "exact original text", "new": "replacement for workspace {index}"}
  ],
  "dependency_edits": [
    {"path": "Cargo.toml", "old": "exact dependency declaration", "new": "updated declaration"}
  ],
  "dependency_prepare": [["cargo", "build"]],
  "allow_policy_commands": false,
  "provenance": {"description": "Explain the edits, relevant toolchain and test scope."}
}
```

Edits must target tracked regular files, match the exact expected occurrence
count (default `1`), and remain inside the disposable workspace. Only the
literal `{index}` is expanded, leaving other source-code braces untouched.
Prepare and initial verification must leave tracked source clean; commit any
generated lockfile in the benchmark source before measuring. Dependency
preparation may update lockfiles after the declared dependency edits.

Include every prewarmed environment output in `environment_paths`, or explicitly
skip additional outputs in a committed Cambium policy. Otherwise the Git
control and Cambium may be doing different work. Policy commands stay disabled
unless the recipe explicitly opts in after review.

## Phases and measurements

Each method starts with its own built and tested reference at the same commit.
It then creates all worktrees, verifies the initial builds/tests, makes
per-workspace source edits and verifies again, changes dependencies and verifies
again, removes the dirty worktrees, and prunes caches.

The default cleanup uses `prune --older-than 1ns` so unreferenced cache entries
are reclaimed immediately. Use `--prune-older-than 168h` to measure normal
default retention, or `--prune-max-unused-bytes BYTES` to measure an explicit
unused-cache budget. These options are mutually exclusive; the budget accepts
nonnegative integer logical regular-file bytes, including zero. Reports retain
the chosen policy. Comparing different retention policies does not establish a
copying-speed improvement or imply that age-based pruning cannot reclaim the
same unused cache.

`scripts/benchmark-cache-budget.py` runs the same offline npm source through
Git-plus-environment-copy controls and before/after Cambium binaries, then checks
multiple unused cache generations and checkpoint replay with a pinned cache.
It adds a documented deterministic warm cache payload to the fixture so physical
retention is observable. The payload is checked by actual builds and the trusted
environment validator. This is a synthetic storage/correctness workload, not a
substitute for the maintained-project or APFS matrix.

```bash
python3 scripts/benchmark-cache-budget.py \
  --before-bin /path/to/baseline-cambium \
  --after-bin /path/to/final-cambium \
  --before-source-commit FULL_BASELINE_COMMIT \
  --after-source-commit FULL_FINAL_COMMIT \
  --output dist/cache-budget.json
```

The driver refuses existing report paths and records exact binary and script
hashes, source commit/tree, all commands, raw workflow reports, signed physical
allocation observations, and selective-prune results. Its replay check removes
the original workspaces and the primary environment before forking the pinned
checkpoint and rerunning the build/tests. It leaves referenced snapshots outside
the budget and makes no claim that an unused-cache limit removes a pinned copy.

Reports contain:

- Exact recipe, upstream provenance, resolved commit/tree, Cambium binary and
  harness hashes, OS/filesystem, CPU/memory, and toolchain versions.
- Per-phase elapsed time, per-workspace creation timing, and actual build/test
  command results. Creation p50/p95 are descriptive values for that small run.
- Signed filesystem free-space deltas relative to both method start and the
  warmed reference, measured after filesystem synchronization.
- Source/environment/cache tree diagnostics, retained state after worktree
  removal, and state after pruning. Logical bytes and `st_blocks` totals are
  never used as proof of physical CoW savings.
- Exact commands and separate output logs. JSON is checkpointed after commands
  and phases. Failure exits nonzero and retains scratch data and evidence;
  successful scratch data is removed unless `--keep` is passed.

Measurements include Cambium's baselines and layers. Package-manager/toolchain
global caches are shared and warmed before workspace phases. Their configured
locations are recorded. Same-volume cache writes and unrelated filesystem
activity can affect free-space deltas; negative observations are preserved.
Run methods sequentially on an otherwise quiet machine. This benchmark does
not claim concurrent throughput, repeated-trial statistical significance, or
universal speed/space superiority.

On a filesystem without CoW, cached environment copies can make Cambium consume
more space before pruning. On APFS, new compiler outputs still allocate blocks:
post-build measurements matter as much as the initial clones.

## Published evidence

See [benchmark results](BENCHMARK_RESULTS.md) for the checked-in Linux runs and
their scope. APFS results require a successful macOS run and its raw artifact;
Linux fallback numbers cannot establish APFS savings.
