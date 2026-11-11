# Changelog

## 0.7.1 — cargo-target seeds grow with their source

### Fixed

- A `cargo-target` seed was captured once per receipt and never again, so
  third-party crates built afterwards (another profile, or a dependency version
  built after its lockfile bump was committed) compiled again in every new
  workspace. The seed is now captured again when the invoking workspace holds a
  third-party unit it lacks.
- A source whose declared inputs no longer match the receipt could mark a
  present cached payload absent. It now leaves the payload in place.

## 0.7.0 — share third-party Cargo build output between workspaces

### Added

- `policy = "cargo-target"` for a Cargo `target` directory. The layer keeps
  Cargo's compile cache and removes the freshness records of every package
  `Cargo.lock` lists without a `source`, plus incremental state, final
  binaries and anything else stored under `target`. A new workspace reuses
  third-party crates and rebuilds its own. A plain `seed` of `target` reuses
  local crates too, which runs code compiled in another checkout with that
  checkout's paths compiled in. The seed is not captured while a build holds
  Cargo's build lock. Measured on a 432-package workspace: 0 third-party
  crates recompiled, 66 seconds, 0.26 GiB of new disk for a 7.4 GiB build
  directory.

## 0.6.1 — forget workspaces whose worktree is gone

### Added

- `cambium forget [--dry-run] [--json] <NAME... | --missing>`: drops the
  record of a workspace whose worktree no longer exists. When another tool
  deletes a worktree, `remove`, `remove --force` and `gc` all refuse the
  record ("worktree ownership cannot be verified") because the receipt they
  verify was inside the deleted worktree. The record stayed `broken` and kept
  its baseline and layers referenced, so `prune` never released them (found
  2026-11-11: 14 such records in one repository held six baselines). `forget`
  deletes only the record, and only when the path does not exist, Git
  registers no worktree there, no worktree carries the workspace's receipt and
  no worktree is on its branch. It never deletes a directory, a Git
  registration or a branch.

### Fixed

- `integrations/claude-worktree-hook` read `.path` from the WorktreeRemove
  input; Claude Code sends `worktree_path`. Every removal exited 0 having
  removed nothing. It now removes the workspace, keeps one with uncommitted
  work (non-zero exit), and forgets the record when the directory is already
  gone.

## 0.6.0 — guarded removal and verified archiving

### Added

- `cambium remove --safe [--json] [--require-merged REF] [--harvest DIR]
  [--salvage-ref REF]`: a guard ladder (unreadable, in-progress,
  branch-changed, dirty, unmerged, salvage-failed, output-conflict,
  output-unreadable) that refuses with exit 3 and a typed JSON reason instead
  of deleting. `--salvage-ref` snapshots tracked + untracked files through a
  private index; `--harvest` moves git-ignored output to the primary checkout
  (ported from oompa after swarm 455987ea lost paid GPU run output,
  2026-10-24). `--safe` and `--force` are mutually exclusive.
- `cambium archive [--json] [--remote origin] BRANCH TAG`: tags the branch
  tip, pushes the tag, verifies it with `ls-remote`, then deletes the branch
  with compare-and-delete. Fails closed; idempotent on rerun.

## Unreleased (folded into 0.6.0) — workspace correctness and build evidence

### Fixed

- Resolve `HEAD` and other worktree-relative revisions in the invoking worktree
  instead of the common Git directory.
- Recovery skips active operations, rechecks state under the workspace and Git
  locks, and requires ownership evidence before destructive cleanup.
- Refuse dependency snapshots whose live declared inputs differ from the
  target receipt. Unvalidated copied dependencies no longer report ready.
- Upgrade a cached missing environment after dependencies become available.

### Added

- `environment_unverified` observations and explicit `cambium env validate`.
- Real-repository regressions for revision context, concurrent recovery, and
  npm dependency mismatch.
- A reproducible npm/Cargo workflow benchmark with source/dependency edits,
  verification, physical allocation observations, and cleanup/cache accounting.

### Changed

- Environment receipt format is version 6; older cache entries are not reused.
- Synthetic benchmark report schema is version 2. Its former
  `environment_ready` counter is now `environment_materialized`, because it
  checks copied fixture files rather than package-manager or build readiness.
- Readiness is explicitly a point-in-time result of declared policies and
  validators. Git workspace status and environment readiness are separate.

## 0.5.0 — transparent worktrees and generic environment receipts

### Added

- `git-cambium` and `git cambium add -b BRANCH PATH [COMMIT]`.
- Worktree `path`, `adopt`, and `reconcile` commands.
- Strict `.cambium.toml` path policy with target-tree resolution.
- `clone`, `seed`, `recreate`, `share`, `empty`, and `skip` policies.
- Branch-correct environment receipts based on Git blob identities.
- Generic built-in defaults for major JavaScript/TypeScript, Python, Rust,
  Clojure/ClojureScript, JVM, Go, and additional ecosystems.
- `cambium env explain` and `cambium env capture`.
- Composite speculative roots that retain environment receipt IDs.
- Explicit `allow_policy_commands` trust boundary.
- Git worktree adoption/reconciliation and moved-path recovery.
- Expanded ecosystem, policy, race, security, and system tests.
- Lean README, detailed comparison, configuration, policy, receipt, defaults,
  plan, and validation documents.

- Derived baselines: with CoW available, a new commit's baseline is cloned
  from the nearest ready baseline and only changed paths are rewritten
  (802 MB repository: 810 MB full checkout vs 10 MB for a 14-file diff).
  Each baseline records its `source` (`checkout` or `derived:<parent>`).

### Changed

- One configuration format: committed `.cambium.toml` (`[settings]` and
  `[[path]]`) plus clone-local `.git/cambium/config.toml` (`[settings]` only).
  `.cambium.json` and the `pattern`/`mode`/`fingerprint` aliases are gone.
- `allow_policy_commands` is rejected in committed config.
- Environment cache format is version 5.
- `.venv` and other Python environments default to `recreate`, never clone.
- Git's worktree registry is authoritative over Cambium's secondary metadata.
- Unknown ignored paths remain unmanaged instead of being copied implicitly.
- Built-in ecosystem support is filesystem-policy data, not package-manager
  parsing or dependency resolution.

### Fixed

- Target branches no longer inherit environments from incompatible lockfiles.
- Target branches resolve their own `.cambium.toml` instead of the primary
  checkout's policy.
- Cached built-in provenance cannot weaken a later custom-rule safety check.
- Metadata-only receipts can record later presence/validation observations.
- Recreate/share receipts no longer fail merely because they lack payload
  directories.
- Environment layers pinned by speculative roots survive cache pruning.
- Environment capture defaults to the workspace's own HEAD.

## 0.3.0 — Git-native speculative roots

- Dirty source checkpoints through a private Git index.
- Deterministic hidden root commits and aliases.
- Native root diff, fork, lineage, and GC.
- Rejected duplicate custom source CAS in favor of Git's Merkle DAG.

## 0.2.0 — native workspace control plane

- Real linked worktrees, CoW/fallback materializers, prepared layers, operation
  journals, locks, recovery, cache pruning, and comparative benchmarks.
