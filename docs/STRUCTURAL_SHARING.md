# Structural sharing, Merkle trees, and speculative roots

## Structural sharing is not the same thing as Merkle hashing

Clojure's persistent collections are immutable values whose updates return new
roots while reusing unchanged internal nodes. Hash maps and sets use
HAMT-family tries; vectors use wide bit-partitioned vector tries plus a small
tail. Clojure transients temporarily allow controlled local mutation to make a
batch transformation faster before publishing another immutable value.

None of that requires cryptographic hashes. Within one process, two versions can
share an internal node by pointer.

A Merkle structure adds stable content identity: every immutable node is named
by a hash of its canonical contents and child IDs. This supplies:

- identity across processes and machines;
- deduplication in a content-addressed store;
- corruption/integrity checks;
- complete-subtree equality certificates;
- fast diffs that skip equal subtrees;
- transport and cache keys.

A data structure may be persistent without being Merkle-addressed, Merkle-
addressed without presenting a convenient persistent update API, or both.

## Git already provides the relevant persistent Merkle DAG

For committed source, Git is the structural-sharing system:

```text
commit A                       checkpoint root
└── tree A                     └── tree B
    ├── src tree A                 ├── src tree B
    │   ├── foo blob A             │   ├── foo blob B
    │   └── bar blob ──────────────┼───└── bar blob (shared)
    └── docs tree ─────────────────┴────── docs tree (shared)
```

Changing one file writes a new blob and new tree objects only along that file's
directory path. Unchanged blobs and directory subtrees retain exactly the same
object IDs.

Cambium snapshots dirty state with a private temporary Git index:

1. Copy the linked worktree's index to a private sibling file.
2. Stage visible files into that temporary index with `git add -A`.
3. Write the tree with `git write-tree`.
4. Create a deterministic hidden commit whose only parent is the base commit.
5. Pin the commit under hidden Cambium refs.

The real index and branch remain unchanged. Same base commit plus same visible
source tree yields the same speculative root commit ID.

## Why the custom persistent radix prototype was rejected

A prototype implemented:

- a canonical SHA-256 object store;
- a path-compressed persistent radix trie;
- path-copy updates;
- content-defined chunks and file manifests;
- custom named refs, mark/sweep GC, replay, and diff.

It did provide real structural sharing. But it also reimplemented facilities Git
already owns: blobs, directory trees, root identity, refs, locking, integrity
verification, diff traversal, object lifecycle, and future merge behavior.

A synthetic design comparison used 5,000 tracked paths and three dirty paths:

| Path | Median end-to-end checkpoint | Added checkpoint storage after 11 aliases |
|---|---:|---:|
| Custom radix/chunk prototype | ~70.3 ms | ~148 KiB |
| Git-native Cambium root | ~56.5 ms | ~88 KiB |

The Git tree-writing core alone measured about 10.4 ms. The complete Cambium
row also performs coherence checks, durable metadata, hidden-ref publication,
and CLI startup. The one-run Linux fixture is not a universal benchmark; it is
enough to show that the custom structure had not earned its complexity.

Git also brings capabilities the custom tree would otherwise need to rebuild:

- file modes and symlinks;
- gitlinks/submodule entries in tree objects;
- rename-aware tree diffs;
- `git fsck` integrity validation;
- refs and reflogs;
- packfiles and delta compression;
- partial clone/object transfer;
- mature merge-tree machinery;
- SHA-1 or SHA-256 repository formats.

## Newer persistent structures and where they fit

There is no universally newer-and-better persistent collection. The operation
mix determines the structure.

| Structure | Strong fit | Cambium decision |
|---|---|---|
| HAMT | Immutable maps with arbitrary hashable keys | Clojure-style map substrate, not needed beside Git |
| CHAMP | More compact/cache-friendly HAMT variants | Useful for in-memory metadata maps at very large scale |
| Persistent radix/Patricia trie | String/path keys and prefix scans | Rejected as a second source store; still a valid generic design |
| Persistent B/B+ tree | Ordered pages, range scans, on-disk databases | Candidate for large control-plane indexes, not source content |
| RRB tree | Immutable sequences with efficient concat/slice | Useful for vectors/logs, not path namespaces |
| Adaptive radix tree | Dense byte-key tries with specialized node sizes | Potential in-memory index optimization |
| Prolly tree / content-defined B-tree | Ordered, history-independent, versioned maps with range scans and diff proportional to changed chunks | Strongest future custom-store candidate for huge non-Git layers or pathological flat source trees |
| Hash-priority treap / Merkle search tree | Canonical ordered maps and confluent set operations | Research candidate when deterministic merge is more important than Git interoperability |
| Merkle DAG | Durable content identity and subtree reuse | Already supplied by Git for source checkpoints |

The useful question is not “what is the newest persistent trie?” It is “which
existing authority already represents the state we need?” For source trees, the
current answer is Git.

That choice is not mathematically final. Git tree objects contain the complete
ordered entry list for a directory. One edit in an exceptionally large flat
directory therefore rewrites that directory tree, and the current checkpoint
path stages/scans visible source state. A prolly tree could provide
history-independent chunk boundaries, ordered scans, structural sharing, and
diffs proportional to changed chunks. Cambium will benchmark that design only
if real repositories show Git tree/index work dominating checkpoint latency;
until then it would be a second source authority with a large interoperability
bill.

## Speculative refs and root lifecycle

Cambium uses two hidden ref namespaces:

```text
refs/cambium/speculative/names/<hash-of-user-name>
refs/cambium/speculative/roots/<root-commit-id>
```

The first gives a mutable human-facing alias. The second pins an immutable root
commit independently of alias changes. Metadata records the original name, message, source workspace, parent roots,
and creation time. Each workspace also records its current root; checkpointing
again uses that root as the default logical parent, so aliases can move without
collapsing the immutable lineage. `--detach` explicitly starts a new root
lineage.

`cambium root gc` removes root refs that are no longer reachable from:

- named speculative refs;
- parent links of named refs;
- active workspace provenance;
- the configured age grace period.

It deliberately does not run an aggressive `git prune`. Once a hidden ref is
released, Git's normal GC policy decides when unreachable object bytes are
reclaimed.

## What this still is not

A speculative root is not a mounted persistent filesystem. It does not
implement `open(2)`, `mmap`, `fsync`, locks, inodes, FSEvents, or rename-after-
open semantics.

Forking still materializes a normal linked worktree. A mounted FSKit/FUSE
backend earns implementation only if measurements show native namespace
materialization remains the dominant cost after CoW, sparse checkout, prepared
indexes, and prepared environment layers.


## References

- [Clojure data structures](https://clojure.org/reference/data_structures) —
  immutable persistent collections and structural sharing.
- [Clojure functional programming](https://clojure.org/about/functional_programming)
  — array-mapped hash tries and persistence.
- [Clojure transients](https://clojure.org/reference/transients) — O(1)
  persistent/transient conversion with controlled local mutation.
- [Git `write-tree`](https://git-scm.com/docs/git-write-tree),
  [`commit-tree`](https://git-scm.com/docs/git-commit-tree), and
  [`update-ref`](https://git-scm.com/docs/git-update-ref) — the plumbing used
  by Cambium's checkpoints.
- Steindorfer and Vinju,
  [“Optimizing Hash-Array Mapped Tries for Fast and Lean Immutable JVM Collections”](https://doi.org/10.1145/2814270.2814312)
  — CHAMP.
- Bagwell and Rompf,
  [“RRB-Trees: Efficient Immutable Vectors”](https://infoscience.epfl.ch/record/169879)
  — relaxed radix-balanced persistent vectors.
- [Dolt storage-engine documentation](https://www.dolthub.com/docs/architecture/storage-engine/)
  — prolly trees as content-addressed, structurally shared ordered maps.
