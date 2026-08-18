# Cambium architecture

## Authority map

| Concern | Authority |
|---|---|
| Commits, branches, tracked source, refs, worktree registration | Git |
| Dirty source checkpoints | Hidden Git commits/trees/blobs |
| Physical sharing and live file I/O | Native filesystem |
| Ignored-path behavior | Generic Cambium policies |
| Environment compatibility/cache | Cambium receipts and immutable layers |
| Workspace lifecycle/recovery | Cambium registry, locks, and operation journal |
| Process/network/security isolation | External sandbox, if any |

Cambium never requires a second source-tree database.

## Native worktree creation

```text
resolve target Git commit
        │
resolve target-tree .cambium.toml and input blob IDs
        │
plan materializer for actual destination filesystem
        │
        ├─ APFS/reflink available → immutable baseline + native CoW
        └─ unavailable            → ordinary Git checkout
        │
git worktree add --no-checkout registers a real worktree
        │
populate tracked files + install matching index
        │
apply ignored-path receipts and policies
        │
verify Git status + publish workspace metadata
```

Once created, the workspace is made of ordinary native files. Cambium is not in
the live I/O path.

## Generic path-policy flow

```text
target Git tree
├─ tracked path                   → Git owns it
├─ ignored + matching policy      → Cambium may manage it
├─ ignored + no policy            → leave untouched
└─ untracked but not ignored      → refuse by default
```

Policy precedence is built-in, then legacy local JSON override, then the exact
target-tree `.cambium.toml` rule.

Receipts hash target-tree input object IDs plus policy/platform semantics.
Clone/seed payloads publish atomically beneath `.git/cambium/layers`; metadata
receipts represent recreate/share/empty/skip policies without copying bytes.

## Speculative checkpoint path

```text
native dirty worktree
       │
       ├─ copy real index to private sibling
       ├─ git add -A with GIT_INDEX_FILE=temp
       ├─ verify capture coherence
       └─ git write-tree
               │
               ▼
deterministic hidden commit
  parent = ordinary base commit
  tree   = visible checkpoint tree
               │
       ┌───────┴─────────────┐
       │                     │
hidden Git refs      environment receipt IDs
```

Unchanged Git blobs and subtrees retain their existing object IDs. The visible
branch and staging area remain untouched.

## Speculative fork

```text
resolve hidden root and receipt set
       │
create normal linked worktree at root base
       │
restore root tree into worktree only (branch remains at base)
       │
replace default environments with checkpoint receipts
       │
record dirty native workspace
```

## Filesystem boundary

APFS/reflinks already provide block-level copy-on-write. A custom CambiumFS
would only be justified if repeated directory-entry materialization remains the
measured bottleneck for enormous sparse repositories. See
[PRODUCT_DECISION.md](PRODUCT_DECISION.md).

## Concurrency and recovery

Cambium uses cross-process `O_EXCL` locks for Git administration, workspace
names, layer receipts, cache pruning, reconciliation, and speculative refs. Git
provides atomic object/ref writes. Durable create/remove journals allow
conservative rollback while protecting a branch that advanced after an
interruption.

## Package map

```text
cmd/cambium             standalone CLI
cmd/git-cambium         Git-discovered subcommand
internal/cli            commands and output
internal/workspace      lifecycle, environment, checkpoint/fork composition
internal/native         materialization, receipts, layers, recovery, pruning
internal/environment    built-in generic path-policy data
internal/config         operational JSON and strict policy TOML
internal/speculative    private-index roots, refs, diff, restore, GC
internal/gitx           Git process boundary and target-tree queries
internal/operation      durable operation journal
internal/lockfile       cross-process locking/dead-owner recovery
internal/state          atomic workspace registry
internal/bench          cross-tool workspace benchmark
```
