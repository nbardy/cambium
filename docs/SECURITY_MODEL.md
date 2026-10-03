# Security model

Cambium is a workspace-isolation and lifecycle tool, not a security sandbox.

## Isolated

- tracked working files in separate linked worktrees;
- ignored paths materialized by `clone`, `seed`, `recreate`, or `empty`;
- per-worktree Git branch/index state;
- workspace paths and lifecycle metadata.

## Shared

- Git objects and repository-level refs, including hidden Cambium refs;
- paths explicitly configured as `share`;
- global package/download caches outside the repository;
- user credentials, environment variables, network, processes, and kernel.

## Path-policy boundary

- Tracked paths are never overlaid.
- Unknown ignored paths are not inferred.
- Unignored outputs are refused by default.
- Negated-ignore children prevent a parent directory from being silently
  absorbed.
- Absolute, escaping, overlapping, `.git`, NUL, and unexpected-symlink paths
  are rejected.
- Built-ins stand down on tracked/unignored conflicts; custom rules fail.

`share` means concurrent writes are visible to every workspace and the primary
checkout. Use it only for caches that explicitly support multi-process access.

## Policy commands

Tracked `.cambium.toml` may contain direct `prepare` and `validate` argv.
Executing repository configuration is code execution, so commands are disabled
by default. `allow_policy_commands` is rejected in committed `.cambium.toml`
(any branch can edit it) and may be enabled only in the clone-local
`.git/cambium/config.toml`, after review.

Cambium invokes argv directly. It does not add a shell, but a policy may
explicitly invoke one. Commands inherit user credentials and environment and
are not sandboxed.

## Secrets and runtime state

Cambium does not automatically infer `.env`, credential stores, live databases,
WAL files, sockets, PID files, logs, or arbitrary temporary directories.
Untrusted code should run in a VM, container, restricted account, or other real
security boundary.

## Recovery safety

Automatic recovery does not delete a branch that advanced beyond the recorded
base. Workspace paths are canonicalized through existing symlink ancestors
before registration or cleanup.

## Speculative roots

Hidden roots are validated for deterministic shape and Git object integrity.
Any process that can write the common Git directory can still alter repository
objects or refs. Checkpoint capture uses Git's normal clean filters in a private
index, so configured filters may execute exactly as they would during a commit.
