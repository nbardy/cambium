# ADR 001: native workspaces first; no mounted filesystem

- Status: accepted
- Date: 2026-08-16

## Context

The product needs many isolated coding-agent workspaces on one local repository
without multiplying tracked source, dependencies, and build environments.
Candidate substrates included plain worktrees, APFS clones, simgit, cow,
ArtifactFS, EdenFS, and a custom persistent filesystem.

## Decision

Use real Git linked worktrees plus native APFS clone/reflink materialization.
Add generic Git-aware path policies, branch-correct environment receipts, and
lifecycle/recovery above that substrate. Use Git checkout when native CoW is unavailable.

Do not fork ArtifactFS or EdenFS. Do not build FSKit/FUSE in this phase. Do not
rewrite the Go control plane in Rust merely for language preference.

## Rationale

- Native files preserve maximum compatibility and performance after creation.
- Git already supplies the immutable content-addressed authority for tracked
  source and the integration model for agent branches.
- APFS/reflinks already supply physical structural sharing.
- The unmet need is ready-to-build environment policy and reliable agent
  lifecycle, not a new block-sharing mechanism.
- ArtifactFS optimizes lazy remote hydration; the initial target already has a
  complete local repository.
- A custom filesystem would add a large POSIX correctness obligation before a
  benchmark showed namespace materialization was the bottleneck.

## Consequences

Positive:

- no daemon, mount, macFUSE, or kernel extension;
- ordinary Git tooling and worktree registration;
- explicit tracked/ignored and secret/runtime-state boundaries;
- usable on day one while preserving an optional future virtual backend.

Negative:

- every native workspace still has its own directory/file metadata;
- per-file tree cloning may be slower than a controlled APFS root clone on very
  large monorepos;
- full sparse/lazy namespace benefits are unavailable.

These negatives are benchmark targets, not assumptions that justify a new
filesystem prematurely.
