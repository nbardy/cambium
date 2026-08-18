# Cambium and the existing workspace baselines

Cambium is not intended to beat every tool at every workload. Its design target
is a specific combination:

```text
real linked Git worktrees
+
native copy-on-write tracked files
+
branch-correct ignored environments
+
speculative dirty-state checkpoints
+
crash-safe agent lifecycle
```

This document separates **implemented semantics** from **performance claims**.
The latter require the published benchmark matrix on the target filesystem.

## Summary

| Tool | Git model | Working-file strategy | Ignored environment | Dirty-state forks | Live I/O |
|---|---|---|---|---|---|
| Git worktree | One shared repository | Normal checkout | Absent | No | Native |
| simgit | One shared repository | Immutable CoW baseline | Not its main scope | No | Native on macOS/reflink Linux; overlay fallback on other Linux filesystems |
| cow | Independent repo clone by default | Whole-directory APFS/reflink clone | Whole live tree plus special handling | No | Native |
| ArtifactFS | Git-backed mounted view | Lazy blob hydration | Writable overlay | Mount overlay | FUSE daemon |
| EdenFS/Sapling | Virtual source filesystem | Lazy materialization | Overlay/materialized state | SCM-specific | Virtual filesystem |
| **Cambium** | **One shared repository** | **CoW baseline or Git fallback** | **Explicit receipts and path policies** | **Hidden Git roots** | **Native** |

## Plain Git worktrees

Git supplies the semantic foundation Cambium deliberately preserves:

- one common object database and repository-level refs;
- separate `HEAD`, index, and administrative state per linked worktree;
- ordinary branch, diff, merge, rebase, push, and cleanup behavior.

A normal `git worktree add` checks out tracked files into the new path and does
not copy ignored state such as `node_modules`, `.venv`, or `target`.

### Git is better when

- disk and dependency setup are unimportant;
- the simplest, most established behavior is preferred;
- no speculative environment/checkpoint layer is needed.

### Cambium adds

- custom materialization through `git worktree add --no-checkout`;
- native CoW source sharing when supported;
- prepared ignored paths;
- durable create/remove recovery;
- hidden dirty-state roots.

Every Cambium execution workspace remains a normal Git worktree.

## simgit

[simgit](https://github.com/abendrothj/simgit) is the closest baseline for
Cambium's tracked-source path. It creates real linked worktrees from an
immutable baseline using APFS clones or Linux reflinks. On Linux filesystems
without reflinks it can use `fuse-overlayfs`. Its public benchmark demonstrates
large physical-disk savings for multiple APFS worktrees, while also reporting a
higher cold-creation cost than normal worktrees.

### simgit is better today at

- a narrow and understandable CoW-worktree promise;
- published APFS evidence;
- Linux non-reflink disk sharing through an overlay fallback;
- lower conceptual and implementation surface.

### Cambium differs

Cambium adds branch-correct ignored-environment receipts, user-defined path
policies, composite source/environment checkpoints, checkpoint lineage,
adoption/reconciliation, and a larger crash-recovery/control plane.

Until Cambium's APFS matrix is published, simgit is the safer proven choice for
"make tracked linked worktrees use less disk."

## cow

[cow](https://github.com/joeinnes/cow) optimizes a different primitive: clone a
ready development directory extremely quickly with APFS `clonefile`. Its default
pasture includes source, `.git`, dependencies, caches, and other live files,
then applies cleanup and package-manager-oriented handling. It also has mature
CLI ergonomics, migration, statistics, and MCP support.

### cow is better today at

- instant whole-directory Mac environments;
- polished package-manager and shell ergonomics;
- existing workspace migration;
- MCP integration;
- a mature local workflow around APFS directory clones.

### Cambium differs

- Cambium's default is a real linked worktree in one Git repository, not an
  independent cloned repository.
- Cambium does not indiscriminately copy `.env`, databases, sockets, PID files,
  or arbitrary ignored state.
- Environment compatibility derives from target-tree input object IDs and
  explicit policies rather than only from whatever happens to be present in a
  source directory.
- Cambium can checkpoint and fork dirty source through hidden Git roots.

The tradeoff is extra receipt/lifecycle machinery and, currently, less polished
Mac UX than cow.

## ArtifactFS

[ArtifactFS](https://github.com/cloudflare/artifact-fs) is a Git-backed FUSE
filesystem. It presents a repository quickly, hydrates blobs on demand, and
stores writes in an overlay. It is valuable when a repository is remote or very
large and an agent will read only a small portion of it.

### ArtifactFS is better when

- initial clone/download latency dominates;
- the working set is sparse;
- containers or remote sandboxes need a tree before all blobs arrive;
- a FUSE daemon is acceptable.

### Cambium is better suited when

- the repository is already local;
- agents repeatedly run Git status, search, builds, indexing, and file watches;
- ordinary native-file compatibility is the priority;
- no mount or daemon should remain in the I/O path.

ArtifactFS is an optional alternative for a different workload, not a library
Cambium needs to fork.

## EdenFS and Sapling

[EdenFS/Sapling](https://github.com/facebook/sapling) is the industrial
reference for virtualized working copies at monorepo scale. It can avoid eager
namespace/content materialization and is backed by years of production work,
but it is a much larger SCM/filesystem system than Cambium's local Git-focused
scope.

Cambium borrows the lesson that lazy namespaces can win at extreme scale, but
it does not inherit the implementation or operational burden before a real
benchmark proves native CoW worktrees insufficient.

## Full clones and containers

A full Git clone provides strong repository independence but duplicates more
Git data and requires explicit synchronization. Containers add process and
runtime isolation but are heavier than needed when the only requirement is a
separate working directory.

Cambium is not a security sandbox. It can be used *inside* a container or VM
when untrusted execution needs a real isolation boundary.

## What is uniquely useful about Cambium

Cambium's differentiator is not APFS alone. APFS already supplies the physical
copy-on-write primitive. The combination is:

1. Git remains the source/history authority.
2. Cambium registers standard linked worktrees.
3. APFS/reflinks share materialized tracked bytes.
4. Generic path policies handle only declared ignored state.
5. Target Git blobs produce branch-correct environment receipts.
6. Hidden Git roots make dirty source states persistent and forkable.
7. Operation journals and locks make agent lifecycle recoverable.

No other baseline above currently advertises that exact combination.

## Evidence status

Implemented and tested:

- real worktree registration;
- Git-checkout fallback;
- path-policy safety and branch-correct receipts;
- environment isolation/recreation;
- dirty checkpoint/fork behavior;
- concurrency and interrupted-operation recovery.

Still awaiting target-hardware evidence:

- equal-semantics APFS timing and physical-allocation comparison against Git,
  simgit, and cow;
- real monorepo measurements;
- prepared-index and root-clone tradeoffs.

See [docs/VALIDATION.md](docs/VALIDATION.md) and the open APFS benchmark issue.
