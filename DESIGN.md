# Cambium design

Cambium is a Git-native workspace engine for parallel coding agents.

The design keeps three authorities separate:

1. **Git** owns commits, trees, refs, tracked source, branches, and linked-worktree registration.
2. **APFS/reflinks** provide native copy-on-write sharing for materialized working files without a userspace filesystem in the hot I/O path.
3. **Cambium** owns optimized worktree construction, safe ignored-path policies, branch-correct environment receipts, speculative dirty-state roots, and lifecycle recovery.

The default architecture deliberately does **not** implement a custom filesystem. A CambiumFS/FSKit/FUSE backend is benchmark-gated for the future case where namespace materialization—not file data—is proven to dominate enormous sparse workspaces.

Detailed design:

- [Architecture](docs/ARCHITECTURE.md)
- [Implementation plan](docs/PLAN.md)
- [Path policies](docs/PATH_POLICIES.md)
- [Environment receipts](docs/ENVIRONMENT_RECEIPTS.md)
- [Structural sharing](docs/STRUCTURAL_SHARING.md)
- [Product decision](docs/PRODUCT_DECISION.md)
- [Comparisons](COMPARISONS.md)
