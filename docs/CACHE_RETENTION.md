# Cache retention

Cambium retains immutable source baselines and environment snapshots so it can
reuse them and replay checkpoints. On filesystems without copy-on-write, an
environment snapshot is an additional copy alongside writable workspaces.

`cambium prune` removes unused cache entries. Choose an age threshold or a
byte budget for each invocation:

```bash
# Existing default: remove entries unused for at least seven days.
cambium prune

# Preview which unused entries would be evicted to retain at most 2 GiB.
cambium prune --max-unused-bytes 2GiB --dry-run --json

# Apply that budget, keeping the most recently used entries that fit.
cambium prune --max-unused-bytes 2GiB

# Remove all unused entries immediately, including incomplete staging entries.
cambium prune --max-unused-bytes 0

# Alternatively, select entries by minimum idle age.
cambium prune --older-than 24h
```

`--older-than` and `--max-unused-bytes` are mutually exclusive. Supplying a
budget replaces the default seven-day threshold. The budget accepts a
non-negative integer number of bytes, optionally followed by `B`, `KB`, `MB`,
`GB`, `TB`, `KiB`, `MiB`, `GiB`, or `TiB`, without case sensitivity. Decimal
units use powers of 1,000; binary units use powers of 1,024. Fractional,
negative, and overflowing sizes are rejected before cache access.

## What a budget protects and limits

The budget applies to **unused cache entries at the time pruning runs**.
Workspaces, durable operation journals, and retained speculative checkpoints
protect their referenced baselines or environment layers. Protected entries
are excluded from the budget, even when their bytes exceed the requested
limit. A zero budget does not remove these references or their payloads.

Pruning considers unused baselines and environment layers together. It evicts
the oldest entries until their combined retained size fits. Cambium uses the
existing `ready` marker timestamp as its recency signal; preparation and
supported cache-reuse paths update it. Reading a retained layer, including
checkpoint replay, does not necessarily update that timestamp. Incomplete
entries use their own modification time. Equal timestamps have a stable kind/ID
ordering. Eviction removes whole entries, so retained bytes can be below the
requested limit.

The cache administration lock covers reference collection, inventory, selection,
and deletion. Capture and publication share that lock, so pruning waits for an
in-progress payload and its durable reference. Inventory failures stop the
operation before deletion. A deletion error is reported; the budget is not
claimed to have been achieved after an error.

This command is an explicit cleanup operation. It does not install a daemon,
automatically prune on workspace removal, or enforce a continuous storage cap.
New unused snapshots can accumulate after it returns. `prune --older-than 1ns`
continues to provide immediate age-based cleanup of unreferenced entries.

## Byte accounting

The budget counts **logical regular-file bytes**, including cache metadata,
within each entry. Symlinks are not followed and do not contribute target
bytes. Directories and other non-regular files contribute zero. With a zero
budget, unprotected zero-byte entries are also removed.

Logical bytes are not physical allocated bytes. Filesystem metadata, sparse
files, compression, and copy-on-write sharing can change actual allocation.
The budget therefore bounds unused logical cache content, not total device
usage or the size of pinned snapshots. Physical-allocation claims require
separate filesystem measurements.

Text output labels the byte totals. JSON adds these fields:

| Field | Meaning |
|---|---|
| `byte_accounting` | `logical_regular_file_bytes` |
| `max_unused_bytes` | The requested byte budget; omitted for age pruning. |
| `protected_bytes` | Referenced cache content excluded from selection. |
| `eligible_bytes` | All unprotected cache content before pruning. In age mode, this includes entries younger than the threshold. |
| `retained_bytes` | Unprotected content the plan keeps. |
| `selected_bytes` | Unprotected content the plan selects for eviction. |
| `removed_bytes` | Content in entries actually removed successfully. Zero in a dry run. |
| `selected[].logical_bytes` | The logical size of each selected entry. |
| `removed[].logical_bytes` | The logical size of each removed entry. |

In a dry run, `retained_bytes` describes the proposed remainder; no files are
removed. A later invocation recomputes inventory and protections under the lock
and can select a different plan if cache use or references changed.

To release a checkpoint's pinned environment, explicitly remove the checkpoint
alias and allow root garbage collection after its live workspace references
are gone. Cache pruning then sees the unreferenced payload. Keeping checkpoint
replay available necessarily keeps its snapshot available too.
