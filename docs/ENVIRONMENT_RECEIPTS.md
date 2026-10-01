# Environment receipts

A prepared directory is reusable only when Cambium can explain which source
state it belongs to. That explanation is an environment receipt.

## Receipt identity

For each active path rule, Cambium hashes:

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

Receipts represent **compatible prepared state**, not arbitrary byte-for-byte
history of every ignored file. If `node_modules` is manually modified without
changing any declared input, the receipt identity is unchanged and an existing
canonical layer may be reused. Dependency experiments are expected to modify
tracked manifests/lockfiles or custom input markers.

Projects that need finer identity should add the relevant marker files to
`inputs`, use `source_sensitive = true`, or define a validation command.
