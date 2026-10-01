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

### `seed`

Materialize a disposable acceleration layer. It may be stale or path-sensitive;
the owning tool remains responsible for validation and rebuilding.

Good fits:

- Cargo/Maven/Lein `target`
- Gradle project cache
- frontend build caches
- compiler output directories

A seed never becomes a correctness authority.

### `recreate`

Remove any copied path and optionally run `prepare` plus `validate` in the new
workspace. Without a prepare command, an expected path is reported missing.

Good fits:

- Python `.venv` and `venv`
- tox/nox/Conda/Pixi environments
- other path-bound environments

Python virtual environments default to recreate because interpreters and
scripts commonly embed absolute paths.

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
