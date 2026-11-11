# Path policies

Cambium manages filesystem paths, not package managers. A path is eligible only
when a rule names it and Git confirms Cambium may own it.

## Ownership boundary

| Git/path state | Cambium behavior |
|---|---|
| Tracked | Git owns it; environment overlay is refused |
| Ignored + matching rule | Apply the rule |
| Ignored + no rule | Leave untouched/unmaterialized |
| Untracked but not ignored | Refuse by default |
| Secret/runtime path with no rule | Leave untouched |

Cambium asks Git through `check-ignore` and `ls-files`; it does not reimplement
`.gitignore`. Nested ignores, negation, `.git/info/exclude`, and global excludes
therefore follow Git's behavior.

## Policies

### `clone`

Publish a compatible ignored output into Cambium's immutable layer cache, then
copy-on-write clone it into each workspace. On unsupported filesystems a normal
copy is used unless CoW is required.

Good fits:

- `node_modules`
- ignored Yarn cache/unplugged trees
- `vendor` or `deps` when relocatable
- CocoaPods or vcpkg dependency trees

A workspace write is private even though initial physical blocks may be shared.
An unvalidated clone is a warm candidate, not a ready dependency environment.
Without a successful project-defined `validate` command it is reported as
unverified. Validation is repeated in each destination workspace, including
when the payload came from a cache. The source's live declared inputs must
match the target receipt before it can be captured.

### `seed`

Materialize a disposable acceleration layer. It may be stale or path-sensitive;
the owning tool remains responsible for validation and rebuilding.

Good fits:

- Cargo/Maven/Lein `target`
- Gradle project cache
- frontend build caches
- compiler output directories

A seed never becomes a correctness authority.

### `cargo-target`

Seed a Cargo `target` directory with third-party build output only.

A plain `seed` of `target` is unsound across checkouts. Cargo finds every unit
fresh, so the new workspace runs code compiled from the other checkout's
sources, with that checkout's `CARGO_MANIFEST_DIR` compiled in. Registry and
git packages do not have this problem: their sources are immutable and live
under `CARGO_HOME`, outside any checkout.

`cargo-target` therefore keeps Cargo's compile cache (`.fingerprint`, `deps`
and `build` in each profile directory, for the host and for every target
triple) and removes:

- the freshness record of every package that `Cargo.lock` lists without a
  `source`: workspace members and path dependencies. Cargo rebuilds these in
  the workspace and reuses the rest;
- `incremental`, final binaries, `examples`, and everything a project stores
  beside Cargo's own directories (evidence, timing reports, test archives).

`Cargo.lock` is read from beside the target directory. It is read twice: from
the source workspace when the seed is captured, and from the new workspace when
the seed is installed, so a package that became a path dependency since the
capture is rebuilt. With no readable `Cargo.lock` every record is removed and
the workspace builds cold. An unrecognized directory layout is emptied for the
same reason. Cambium never tells Cargo that something is fresh; it only
withholds records, so the worst outcome is a rebuild.

A target directory is not captured while a build holds Cargo's build lock. The
layer is published as absent and the next capture or workspace creation
upgrades it.

```toml
[[path]]
path = "target"
policy = "cargo-target"
inputs = ["Cargo.lock", "rust-toolchain.toml"]
```

Choose `inputs` for how often the seed should refresh. With `Cargo.lock` as an
input, a dependency change starts a new seed, captured from the invoking
workspace the next time one is created. With only the toolchain file, one seed
is reused across dependency changes and Cargo rebuilds what changed. Without
`inputs` the receipt is tied to one commit and other commits get no seed.

Limits:

- Builds must use the same Cargo configuration to share units. Different
  `RUSTFLAGS`, profiles or feature selections produce different unit hashes and
  compile again (measured: a build without a project's `--cfg` flag reused
  nothing).
- A third-party crate that reads a workspace-relative environment variable at
  compile time is still reused. Cargo does not track that either.

Measured on a 432-package workspace (374 third-party, 58 local), 2026-11-12: a
new workspace recompiled 0 third-party crates and 56 local ones in 66 seconds,
and its 7.4 GiB build directory used 0.26 GiB of its own disk.

### `recreate`

Remove any copied path and optionally run `prepare` plus `validate` in the new
workspace. Without a prepare command, an expected path is reported missing.

Good fits:

- Python `.venv` and `venv`
- tox/nox/Conda/Pixi environments
- other path-bound environments

Python virtual environments default to recreate because interpreters and
scripts commonly embed absolute paths.

Preparation and validation are separate: producing a directory alone does not
verify its contents. Configure a validator when the project needs a readiness
claim, and enable policy commands locally after reviewing them.

### `share`

Create a symlink to the primary checkout's path. Concurrent mutations are
visible to every workspace. Use only for caches explicitly designed for
multi-process sharing.

### `empty`

Ensure a fresh empty directory exists. This is suitable for temporary or
regenerated state that should not carry across workspaces.

### `skip`

Do nothing. This is useful to disable a built-in for one repository or to make
an intentional exclusion obvious.

## Safety rules

- Output paths must be concrete relative paths; input patterns may use `**`,
  `*`, `?`, and character classes.
- `.git`, absolute paths, `..` escapes, overlapping outputs, and NUL bytes are
  rejected.
- A managed output containing untracked files excluded by a negated ignore rule
  is rejected rather than silently absorbed.
- Unexpected symlink outputs are rejected; `share` is the explicit symlink
  policy.
- Built-in policies silently stand down when a same-named path is tracked or
  unignored. Custom rules fail loudly.
- `allow_unignored = true` is an explicit escape hatch and should be rare.

## Secrets and live runtime state

Cambium does not automatically manage:

```text
.env
credentials
*.pid
*.sock
live databases
SQLite WAL files
logs or arbitrary temporary directories
```

Unknown ignored paths are not cloned merely because they appear in
`.gitignore`.
