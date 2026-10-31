# Environment receipts

A prepared directory is reusable only when Cambium can explain which source
state it belongs to. That explanation is an environment receipt.

## Receipt identity

For each active path rule, Cambium hashes (receipt format version 6):

```text
receipt format version
OS and architecture
output path and policy
complete input-pattern list
matching target-tree Git blob IDs and file modes
required / activation / safety flags
prepare and validate argv
source root when source-sensitive or no inputs matched
built-in versus custom-policy provenance
```

Inputs come from `git ls-tree` on the exact target commit or hidden speculative
root. Cambium does not read a feature branch's lockfile from the primary
checkout by accident.

Example:

```text
main/package-lock.json blob A       → receipt E0
feature/package-lock.json blob B    → receipt E1
```

A cached E0 `node_modules` tree cannot be silently treated as E1.

Git input identity is not installation provenance. Before capturing a live
dependency output, Cambium checks its declared live inputs against the target
tree and repeats the check around validation/copying. An installation may still
be stale even when the checkout is clean, so a copied payload without a
successful validation command is explicitly unverified.

Version 5 receipts are not reused by version 6. A previously cached absence can
be replaced when the source environment later becomes available.

An absence upgrade keeps the existing receipt readable while validation and
copying run. The completed payload is published as a new generation inside the
receipt, then an atomic metadata replacement makes it visible. Validation or
copy failures leave existing workspace and checkpoint references intact. An
interrupted publication can leave an unreferenced payload generation; pruning
the unused receipt removes these generations together.

Environment capture, checkpoint capture, and adoption hold the same cache
administration lock as workspace creation until their receipt IDs are saved in
workspace or checkpoint metadata. Pruning therefore cannot remove active
staging directories or newly published payloads before a durable reference
protects them.

## Payload and metadata

- `clone` and `seed` receipts may contain an immutable payload directory/file.
- `recreate`, `share`, `empty`, and `skip` receipts contain policy metadata but
  no cloned payload.
- Cache publication is atomic and guarded by a per-receipt lock.
- Presence and validation observations can improve a metadata receipt without
  changing its compatibility identity.

## Capture

`cambium env capture WORKSPACE` publishes matching live clone/seed outputs and
records current metadata-only paths. It defaults to the workspace's own `HEAD`
or current speculative root—not the primary checkout's `HEAD`.

Validation commands, when configured and explicitly trusted, run before a
payload is published.

They also run in every destination workspace, even on a cache hit. The check
therefore uses that workspace's actual paths, source, runtime, and installed
payload. A historical successful validation flag does not waive the check.

## Readiness observations

`environment_ready` and Git workspace `status` are separate. A Git worktree may
be fully created while its dependencies are missing or unverified.

- `environment_missing` names expected dependency paths that are absent.
- `environment_unverified` names copied/recreated dependency paths whose
  configured validation has not succeeded.
- `seed` outputs are advisory and remain subject to the compiler/build tool's
  own validation. Missing required shared paths are still missing.
- An applicable missing Node dependency tree is not considered ready merely
  because no installation existed when the first receipt was created.

`cambium env explain NAME` observes current presence and declared-input
compatibility without running policy validation commands. Its successful validation information
is historical; it cannot detect every later payload change.
`cambium env validate NAME` explicitly runs the current checks and persists the
result. It exits unsuccessfully when paths remain missing/unverified or a hook
fails. With `--json`, the explanation is still emitted when a validation hook
fails. `env capture` also reruns configured validation before accepting live
state.

All successful checks are point-in-time observations scoped to the configured
validator. Cambium is neither a package resolver nor a hermetic build system.

## Composite checkpoints

A speculative checkpoint contains:

```text
hidden Git source root
+
environment receipt IDs
+
logical checkpoint lineage
```

The environment bytes are not stored in Git. Root metadata pins their layer IDs
so cache pruning cannot remove an environment still needed by a named root or
active workspace.

Forking restores the dirty source tree and then applies the checkpoint's exact
receipt set.

## Exactness boundary

Receipts identify **candidate prepared state**, not arbitrary byte-for-byte
history of every ignored file. If `node_modules` is manually modified without
changing any declared input, the receipt identity is unchanged and an existing
canonical layer may be reused. Dependency experiments are expected to modify
tracked manifests/lockfiles or custom input markers.

Projects that need finer identity should add the relevant marker files to
`inputs`, use `source_sensitive = true`, and define an appropriate validation
command before making a readiness claim. A directory-existence check is not a
dependency-version or toolchain-compatibility check.
