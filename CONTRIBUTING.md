# Contributing

Cambium optimizes for trustworthy local-agent workspaces. Every change should
name the invariant or measured bottleneck it addresses.

Before opening a change:

```bash
make check
make test-race
make system-test
```

For performance work, attach a JSON report from `cambium benchmark` and state
the filesystem, hardware, OS, Git version, fixture, and cold/warm conditions.
Do not infer CoW savings from logical `du` alone.

Path-policy changes must include:

- tracked/ignored ownership tests;
- branch-correct target-tree input tests;
- a reason the path is `clone`, `seed`, `recreate`, `share`, `empty`, or
  `skip`;
- security analysis for symlinks, commands, secrets, and unignored children.

Do not vendor EdenFS, ArtifactFS, simgit, or cow. A mounted filesystem proposal
must first satisfy the gate in `docs/PRODUCT_DECISION.md`.
