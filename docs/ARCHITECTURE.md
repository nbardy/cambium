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

## Baselines: one per commit, paid for by the diff

A baseline is an immutable checkout of one commit under
`.git/cambium/baselines/<commit>/`; workspaces at that commit are CoW clones of
it and cost almost nothing. Agents usually start from a *new* commit, though,
so the baseline itself is where disk goes. A new baseline is built one of two
ways, recorded as `source` in its `metadata.json`:

| Source | When | Cost |
|---|---|---|
| `checkout` | no CoW, no ready baseline, or the nearest one differs in over half the tree | full tracked size |
| `derived:<parent>` | otherwise | only the changed paths |

Deriving clones the ready baseline with the fewest changed paths, refreshes a
fresh index against the clones (their new inodes would otherwise make
`read-tree -m` refuse every path), applies `read-tree -m -u <parent> <commit>`,
and verifies the result with `git diff-index --quiet <commit>`. Measured on a
16,050-file, 802 MB repository (2026-10-01): full checkout 810 MB, derived
baseline for a 14-file diff 10 MB.

Baseline construction and cache pruning share `cache-admin.lock`, so a parent
cannot be pruned while it is being cloned.

## Generic path-policy flow

```text
target Git tree
├─ tracked path                   → Git owns it
├─ ignored + matching policy      → Cambium may manage it
├─ ignored + no policy            → leave untouched
└─ untracked but not ignored      → refuse by default
```

Policy precedence is built-in, then the exact target-tree `.cambium.toml`
rule. Operational settings come from committed `.cambium.toml` `[settings]`,
overridden by the clone-local `.git/cambium/config.toml`.

Receipts hash target-tree input object IDs plus policy/platform semantics.
Clone/seed payloads publish atomically beneath `.git/cambium/layers`; metadata
receipts represent recreate/share/empty/skip policies without copying bytes.
Receipt format 6 checks live declared inputs before capturing a dependency
payload. Copies without successful validation are unverified warm candidates.
Validation runs again in each destination workspace; its result is a
point-in-time observation. See [environment receipts](ENVIRONMENT_RECEIPTS.md).

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

Recovery acquires the workspace lock before the common Git lock, skips live
operations, and rereads the journal after locking. A `cambium-owner` marker in
the linked worktree's Git administration directory binds ownership to the
workspace identity. Recovery verifies the registration, branch, marker, and
local data before removing anything. Unknown ownership, changed registrations,
older journals without ownership evidence, or local uncommitted/ignored work
produce a `blocked` action for inspection. Planned paths alone never authorize
recursive deletion. Branch deletion uses an expected-value ref update so an
advanced branch is retained.

User-relative revisions are resolved in the invoking worktree. Subsequent
common Git administration runs from the stable common Git directory, including
when the command removes its own invoking worktree.

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
