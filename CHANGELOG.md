# Changelog

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

### Changed

- Configuration schema is version 3.
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
