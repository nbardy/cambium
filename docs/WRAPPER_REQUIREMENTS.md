# Wrapper requirements, against Cambium 0.10.0

The 99 behaviours the Keel, loft, cleanup_tools, wave_sim relay, oompa and Buddies wrappers
implemented around Cambium. Each is either COVERED (the command or setting that now does
it) or DROPPED (why Cambium does not own it). Nothing is open. The 0.7.1 statuses this list
once held are replaced; the original text is in Git history.

Items are grouped as before: A Keel lanes (1-18), B-C Keel seed (19-35), D loft lanes (36-48),
E-F cleanup_tools (49-74), G-H relay (75-87), I other callers (88-92), J owner-wanted features
(93-99).

| # | Status | Cambium 0.10.0 |
|---|---|---|
| 1 | COVERED | `workspace_root` (user or repo setting); `git cambium add -b B -name N -ephemeral` with no PATH creates `<root>/N`. Keel sets `workspace_root = "~/git/lanes/room-runners"` in its `.cambium.toml`. |
| 2 | DROPPED | Keel's character set is Keel policy. Cambium rejects the names it cannot place (empty, `.`, `..`) and slugs the path; a wrapper that wants a stricter alphabet checks it before calling. |
| 3 | COVERED | `add` refuses an existing path (`workspace path already exists`) and an existing name or branch. |
| 4 | COVERED | `add` with no COMMIT resolves HEAD in the invoking checkout, not the primary (checked with a linked workspace ahead of the primary: the new workspace carries its commit). |
| 5 | COVERED | `add -b BRANCH -name NAME -ephemeral`. |
| 6 | COVERED | `-ephemeral` marks it; the primary checkout is never reported ephemeral, removable or adoptable (0.8). |
| 7 | COVERED | `submodules = true`: `add`/`create` clone each submodule from the invoking checkout's local copy with the remote restored to the `.gitmodules` URL. Real-command test: `scripts/wrapper-parity-test.sh`, section 2. |
| 8 | DROPPED | Reading Keel's pinned toolchain is Keel's. Cambium's cargo seed keys on `rustc -vV` run inside the workspace, so `rust-toolchain.toml` already applies. |
| 9 | COVERED | `[[path]] path = "target" policy = "seed" seed = "cargo-deps"` and `cambium seed refresh`; real cargo test: `scripts/wrapper-parity-test.sh`, section 3. |
| 10 | DROPPED | Output formatting of a wrapper. `add` prints the path (`--print-path` prints only it); timing is the caller's `time`. |
| 11 | COVERED | `cambium path NAME` from any directory, through the user registry then `repos_root`; an unknown NAME reports where it looked and the fix. |
| 12 | DROPPED | Containment under a lane root guarded against the primary checkout being treated as a lane. Cambium never removes, adopts or reports the primary checkout as a workspace, and `remove NAME` acts only on registered workspaces, so a path outside the root cannot be named. |
| 13 | COVERED | `remove NAME` is guarded by default: dirty (tracked, untracked, gitlink) is refused, exit 3, reason `dirty`. `--force` is the only opt-out. |
| 14 | COVERED | `submodule-unpushed` refusal: a submodule with uncommitted work or commits no remote holds. |
| 15 | DROPPED | Keel's `sweep.sh lane-end` clears Keel's shared compiler state; it is Keel housekeeping, not workspace removal. Keel keeps it as its own script, called before `cambium remove`. A cargo seed never includes member units. |
| 16 | COVERED | `remove` proves the submodule copies hold nothing unique, then empties them so `git worktree remove` proceeds; no private store is left to delete by hand. |
| 17 | COVERED | `cambium remove NAME`, from any directory. |
| 18 | COVERED | `cambium remove --dry-run NAME`: every guard, the same exit 3 and reason, nothing changed. |
| 19 | DROPPED | Building the gate workload is Keel's build step. Cambium publishes whatever the caller built: `cambium seed refresh`. |
| 20 | DROPPED | Environment of Keel's own build. `seed refresh` reads the workspace's `target` directly and ignores `CARGO_TARGET_DIR`. |
| 21 | COVERED | `cambium seed refresh [NAME]` publishes every profile present under `target` (debug, wasm and others). |
| 22 | COVERED | cargo-deps whitelist: a unit travels only when `.fingerprint/<package>-<hash>` names a package that `Cargo.lock` lists with a `source`; member units never travel (test: `TestDepSeedCarriesDependencyUnitsOnly`, and the real-cargo check). |
| 23 | COVERED | Registry and local package sets come from `Cargo.lock` (`scanCargoLock`), which is offline and locked by construction. |
| 24 | COVERED | A package name that is both local and registry stops the refresh. |
| 25 | COVERED | The layer clone is the native APFS clone or reflink materializer. |
| 26 | COVERED | `seed refresh` holds the profile's `.cargo-lock` flock while cloning and refuses while a build runs. |
| 27 | COVERED | `build/` and `deps/` entries follow the selected unit's name and hash. |
| 28 | COVERED | One seed per `rustc` release under `$CAMBIUM_SEED_ROOT` (default `~/Library/Caches/cambium-seed`), published by an atomic symlink rename. |
| 29 | COVERED | The two newest snapshots of a toolchain are kept, older ones retired on refresh. |
| 30 | COVERED | `.rustc_info.json` and `CACHEDIR.TAG` are copied. |
| 31 | COVERED | `rustc -vV` run inside the workspace. |
| 32 | COVERED | `cambium-seed.json` records origin, full rustc identity and per-profile counts. |
| 33 | COVERED | Apply clones into an absent path only; otherwise the workspace records `dependency seed: <reason>` (`inspect`), e.g. `no seed for rustc X`. |
| 34 | COVERED | An existing destination is never overwritten (`already exists; a seed only starts an absent path`). |
| 35 | COVERED | A non-link at the seed link path stops the refresh. |
| 36 | COVERED | `workspace_root` in `~/.config/cambium/config.toml` or `.cambium.toml`; repo beats user. Set to `~/git/_wt` in the owner's user config. Tested in `scripts/wrapper-parity-test.sh`, section 1. |
| 37 | COVERED | As item 4. |
| 38 | COVERED | `git cambium add -b BRANCH -name NAME -ephemeral [PATH] [BASE]`. |
| 39 | COVERED | `[[path]]` rules in `.cambium.toml` (`clone`, `empty`, `seed`, ...). |
| 40 | COVERED | `merge_target` + `prune_merged_on_add = true`: `add` first removes finished, idle (24h grace), clean, ephemeral workspaces. Protected repositories are skipped. |
| 41 | COVERED | The sweep and `maintain` run a best-effort `git fetch` of the `merge_target` remote branch first. |
| 42 | COVERED | `cambium path NAME` from anywhere; the error is typed when unknown. |
| 43 | DROPPED | As item 12. |
| 44 | COVERED | Dirty or untracked content is refused by `remove`, `prune --merged` and `maintain`. |
| 45 | COVERED | `cambium remove --require-merged origin/main NAME`; `--safe` is the default and still accepted. |
| 46 | COVERED | `--require-merged` is the check; wrappers do not repeat it. |
| 47 | COVERED | Plain `remove NAME` is guarded (dirty, unpushed, in-use, mid-operation, branch-moved) and checkpoints first. |
| 48 | DROPPED | A wrapper's per-removal output. `prune --merged` and `maintain` print `removed` and `kept <reason>` per workspace, and `--json` types both. |
| 49 | COVERED | `cambium maintain --all-repos` (registry, then `repos_root`). |
| 50 | COVERED | Protection in cambium: `protect = true` per repository, `protected_repos` in user config, and `maintain --all-repos --exclude PATH` per run. cleanup_tools' `daily.sh` asks `disk_policy.py protected REPO` for each repository and passes `--exclude`. Test: `TestProtectedRepositoriesAreNeverSwept`, `TestMaintainExcludeLeavesNamedRepositoriesAlone`. |
| 51 | COVERED | `maintain` reconciles. |
| 52 | COVERED | `maintain` forgets records whose worktree is gone (`forget --missing`). |
| 53 | COVERED | `maintain --older-than`, `prune --older-than`. |
| 54 | DROPPED | A wrapper's per-repository message. `maintain` reports per-repository `error` and continues; the exit code is non-zero if any failed. |
| 55 | COVERED | `gc` stays opt-in and refuses protected repositories and unpushed work; `maintain` removes only merged, clean, idle, ephemeral workspaces and checkpoints before every removal. |
| 56 | COVERED | In-use markers (live cwd, `cambium run`, held Cargo lock) block removal, prune and maintain; idle age for workspaces with no commits comes from `--older-than`. |
| 57 | COVERED | Cambium workspaces are found by the registry and `repos_root` (`list`, `status --all`); the Claude Code and Codex adapters in `integrations/` create Cambium workspaces. Plain `git worktree`s Cambium never created are not Cambium's to find: `disk.py` keeps scanning for them, and its plain-Git fallback in `remove_linked` is for exactly those, commented as such. |
| 58 | COVERED | In-use detection: a live process with its cwd inside, a `cambium run` marker, or a held Cargo lock. (`disk.py` still runs its `lsof` check for directories Cambium does not own.) |
| 59 | DROPPED | Cambium takes no `git worktree lock`; a lock held on a plain worktree is `disk.py`'s plain-Git case. |
| 60 | COVERED | `prune --merged --older-than 24h` and `maintain --older-than`; `status --all` shows activity. |
| 61 | COVERED | `cambium status --all`: dirty, unpushed, merged, in-use, exclusive bytes. |
| 62 | COVERED | `protect = true`, `protected_repos`, `maintain --exclude` (item 50). |
| 63 | COVERED | `cambium list --json`, or simply `cambium remove NAME` from anywhere. |
| 64 | COVERED | `remove --salvage-ref refs/...` snapshots tracked and untracked work and keeps unpushed commits at the ref, prints it, and checkpoints. Branch naming is the caller's choice of REF. |
| 65 | COVERED | `remove --harvest . --harvest-to ~/git/_rescued/DATE/NAME`; relative `--harvest DIR` still targets the primary checkout. Test: `TestHarvestToRescueDirectory`. |
| 66 | COVERED | `--harvest-to` leaves behind `target/*/incremental`, `.fingerprint`, `deps`, `build` and dependency directories; only outputs travel. |
| 67 | COVERED | Not needed: guarded `remove` salvages and checkpoints itself. `--force` stays the explicit opt-out. |
| 68 | DROPPED | Dropped for Cambium workspaces on purpose (the owner's rule: no bypass). `disk.py remove_linked` keeps a plain-Git path only for worktrees Cambium has no record of, and says so in a comment. |
| 69 | COVERED | `remove --json` returns `salvageRef` and the harvested counts. |
| 70 | DROPPED | An orphan (its Git link already pruned) has no repository to ask, so Cambium cannot know it. `cambium forget --missing` drops the record of a vanished worktree; moving a stranded directory into `_rescued` is a plain file move that stays in `disk.py` (`remove_orphan`). |
| 71 | DROPPED | Scan hygiene of `disk.py`'s own directory walk. |
| 72 | DROPPED | Submodule `.git` pointers are a property of `disk.py`'s scan; Cambium reads `.gitmodules`. |
| 73 | DROPPED | Which output directories are worth keeping is cleanup policy; `--harvest-to` implements the keep/leave split for Cambium workspaces (item 66). |
| 74 | DROPPED | Idle-age removal of dependency directories (`node_modules`, `target` of idle trees) is disk-pressure policy that also covers non-Git projects, so it stays in `disk.py clean regen`. Cambium removes whole finished workspaces (`prune --merged`, `maintain`) and prunes unreferenced caches by age or bytes. |
| 75 | DROPPED | Which root relay trees live in is relay's call. With `workspace_root = "~/git/_wt"` relay passes no sibling `.worktrees` path; wave_sim PR #155 puts relay trees under `~/git/_wt`. |
| 76 | COVERED | `cambium recover --removed [--path P] REF NAME` restores a removed workspace; `adopt NAME PATH` registers one that exists. |
| 77 | COVERED | `git cambium add -b B -ephemeral PATH COMMIT`; detaching is a `git switch --detach` in the new tree. |
| 78 | DROPPED | Relay's own check that a review turn left the tree unmodified; Cambium's `status --all` shows it, but the assertion is relay's. |
| 79 | COVERED | `cambium remove NAME` from anywhere. |
| 80 | COVERED | `cambium remove NAME` works from any directory, so the printed hint works; with PR #155 relay runs `prune --merged` itself. |
| 81 | DROPPED | Relay's source identity (commit and tree hash of tracked and untracked files) is relay's reproducibility record. `cambium checkpoint` takes hidden roots when a caller wants them. |
| 82 | DROPPED | Not a call; nothing to implement. |
| 83 | DROPPED | Relay's backend selector. `simgit` is retired; `git` is an explicit debug path outside Cambium. |
| 84 | COVERED | `git cambium add -require-cow -ephemeral -name N -b B PATH BASE`. |
| 85 | COVERED | Cambium fails closed; it never falls back to plain Git (`require_cow`, typed errors). The relay's refusal is relay's. |
| 86 | DROPPED | A debug path that does not use Cambium. |
| 87 | COVERED | Plain `cambium remove NAME` is guarded, so the wrapper needs no `--safe`. |
| 88 | COVERED | `add`, `list --json`, `remove --json` refusal reasons (`dirty`, `unpushed`, `in-use`, ...), `--require-merged`, `--harvest`. Owner-only process ownership and task holds stay in oompa. |
| 89 | COVERED | `add --json`, `remove --json --salvage-ref`, and `scrub_env = true` (allow-listed environment, `core.hooksPath=/dev/null`, fsmonitor off, no prompts; test `TestScrubEnvDisablesHooksAndSecrets`). Buddies still scrubs its own process environment, its own children's choice. |
| 90 | DROPPED | A comment, not a call. |
| 91 | DROPPED | Consumers of the `.cambium.toml` format; the format is unchanged. |
| 92 | COVERED | `cambium status --all --json` and `list --json` expose the state `disk_accounting.py` read from `workspace-meta`. |
| 93 | COVERED | `prune --merged REF`, alone, honouring dry-run exactly (`TestPruneMergedDryRunMatchesRealRun`). |
| 94 | COVERED | `maintain [--all-repos] [--merged REF] [--exclude PATH]`. |
| 95 | COVERED | `workspace_root`. |
| 96 | COVERED | `status --all`. |
| 97 | COVERED | In-use markers (item 58). |
| 98 | COVERED | Every `remove` first writes `removed/<name>-<time>`; `recover --removed` restores it. |
| 99 | COVERED | `seed = "cargo-deps"` policy (items 22-35). |

## Bugs the old list recorded

1. `add` default HEAD: resolved in the invoking checkout. Fixed.
2. Primary checkout reported ephemeral: never. Fixed (0.8).
3. `remove NAME` / `path NAME` outside the repository: registry, then `repos_root`. Fixed (0.10).
4. Plain `remove` deleting a dirty workspace: guarded by default. Fixed (0.8); unpushed commits too (0.10).
5. `-older-than` spelling: both spellings work; docs use `--older-than`.
6. "environment unverified" noise on `add`: removed from the summary (0.8).
7. Nothing protected a workspace in use: in-use markers (0.8).

## Hard rules

- Nothing uncommitted or unpushed is removed silently: `remove`, `prune --merged`, `maintain`, `gc`
  and the sweep on `add` refuse or, with `--salvage-ref`, save it at a ref and say so.
- No `rm` sweeps: removal is `git worktree remove` after the guards and a checkpoint.
- Owner root rule: `workspace_root = "~/git/_wt"`.
