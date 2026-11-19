# Configuration

Cambium has one configuration format, TOML, in two places with different
trust:

| File | Committed? | May contain | Read from |
|---|---|---|---|
| `.cambium.toml` | yes | `[settings]` and `[[path]]` rules | rules: the exact target commit; settings: the primary checkout |
| `.git/cambium/config.toml` | never | `[settings]` only | the clone (shared by all its worktrees) |

Settings resolve as **built-in defaults < committed `[settings]` < local
`[settings]`**. The local file holds only keys you set explicitly, so a team
default in `.cambium.toml` keeps applying unless you override that one key.

Neither file is required. With no files, Cambium uses its defaults and the
built-in path policies.

## `.cambium.toml`

```toml
version = 1

[settings]
branch_prefix = "agent/"
require_cow = true

[[path]]
name = "internal dependency tree"
path = ".company-deps"
policy = "clone"
inputs = ["company.lock", "company.toml"]
required = true

[[path]]
name = "generated local environment"
path = ".company-env"
policy = "recreate"
inputs = ["company.lock"]
prepare = ["company-pm", "sync", "--locked"]
validate = ["company-pm", "check"]
required = true

[[path]]
path = "target"
policy = "empty" # override the built-in seed policy
```

`[[path]]` rules are read from the **exact target Git commit or speculative
root**, so a branch carries the policy that matches its own build layout.

### Settings

| Key | Default | Meaning |
|---|---|---|
| `branch_prefix` | `"cambium/"` | prefix for generated workspace branches |
| `materializer` | `"auto"` | `auto`, `cow`, `git`, or diagnostic `copy` |
| `require_cow` | `false` | fail instead of falling back to a normal Git checkout |
| `prepared_index` | `true` | seed worktree indexes from immutable baselines |
| `require_ignored_layers` | `true` | managed outputs must be Git-ignored unless a rule opts out |
| `workspace_root` | unset | directory for `add NAME` with no PATH: `<root>/<name>`. `~/` and absolute paths; relative paths are relative to the primary checkout |
| `merge_target` | unset | ref (e.g. `origin/main`) that marks a workspace finished; `maintain` removes clean, idle, **ephemeral** workspaces whose HEAD is in it, after a best-effort fetch of that remote branch |
| `prune_merged_on_add` | `false` | needs `merge_target`; `add` and `create` first remove finished workspaces the same way (guarded and checkpointed; a failure only warns) |
| `submodules` | `false` | `add`/`create` clone each git submodule from the invoking checkout's local copy (no network, remote restored to `.gitmodules`); `remove` refuses a dirty or unpushed submodule (`submodule-unpushed`), then empties it so `git worktree remove` can proceed |
| `protect` | `false` | **repository only** (committed or clone-local); `maintain`, `prune` and `gc` refuse this repository (exit 3, `protected`) and the sweep on `add` skips it. An explicit `remove NAME` still runs its guards |
| `protected_repos` | `[]` | **per-user file only**; repositories, or directories holding repositories, protected the same way. Wrapper policy that is dynamic goes through `maintain --exclude PATH` instead |
| `scrub_env` | `false` | every child process (Git, policy commands) runs with an allow-listed environment (`PATH HOME USER LOGNAME LANG LC_* TZ TMPDIR GIT_AUTHOR/COMMITTER_*`), `core.hooksPath=/dev/null`, `core.fsmonitor=false` and no terminal prompts, so a repository hook never sees provider keys. Repository selectors (`GIT_DIR`, `GIT_WORK_TREE`, ...) inherited from a calling hook are always dropped, scrubbed or not |
| `repos_root` | `~/git` | **per-user file only**; where `status --all` and `maintain --all-repos` look for repositories with `.git/cambium` |
| `allow_policy_commands` | `false` | **local file only**; see [Commands and trust](#commands-and-trust) |

### Path rule keys

- `name`
- `path` (a concrete relative path; globs are rejected)
- `policy`: `clone`, `seed`, `cargo-target`, `recreate`, `share`, `empty`, or `skip`
- `seed` (only with `policy = "seed"`): `"cargo-deps"`, see [PATH_POLICIES.md](PATH_POLICIES.md#seed)
- `inputs` (globs allowed)
- `prepare` and `validate` as direct argv arrays
- `allow_unignored`
- `source_sensitive`
- `required`
- `activate_on_inputs`
- `priority`

The parser accepts a strict TOML subset: double-quoted strings, booleans,
integers, and one- or multi-line string arrays. Unknown tables, unknown keys,
and duplicate keys fail closed.

## `.git/cambium/config.toml`

`cambium init` writes this file with only the flags you pass:

```bash
cambium init --require-cow=false --allow-policy-commands
```

```toml
# Cambium settings for this clone only. Never committed.
# Overrides [settings] in the committed .cambium.toml.
version = 1

[settings]
require_cow = false
allow_policy_commands = true
```

It lives in Git's common directory, so every linked worktree of the clone
shares it and it can never be committed. `[[path]]` rules here are rejected:
what a commit builds with is decided by that commit.

## Per-user config

`~/.config/cambium/config.toml` (or `$XDG_CONFIG_HOME/cambium/`, or the file in
`$CAMBIUM_CONFIG`) takes `[settings]` only, at the lowest precedence:
defaults, user, committed `.cambium.toml`, clone-local config.

```toml
version = 1

[settings]
workspace_root = "~/git/_wt"
repos_root = "~/git"
```

## Path precedence

For one exact output path:

```text
built-in default
    < target-tree .cambium.toml rule
```

Overlapping parent/child output rules are rejected because two policies cannot
safely own the same subtree.

## Commands and trust

Policy commands execute directly as argv in the target workspace; Cambium does
not invoke a shell unless the policy explicitly names a shell executable.

Committed policy can execute code, and any branch can edit it, so commands are
disabled by default and `allow_policy_commands` is **rejected** in
`.cambium.toml`. Enable it in the local file only after reviewing the
repository policy:

```bash
cambium init --force --allow-policy-commands
```

Cambium is not a sandbox. Commands inherit the user's process environment and
credentials.

### Readiness checks

`validate` is a project-defined check of the actual installed state and any
relevant runtime/toolchain requirements. Without it, cloned dependency folders
remain unverified even when their declared input fingerprints match. A clean
Git checkout is insufficient evidence that the last install succeeded.

For example, a project that supplies its own dependency checker can configure:

```toml
[[path]]
path = "node_modules"
policy = "clone"
inputs = ["package.json", "package-lock.json", "scripts/check-dependencies.cjs"]
validate = ["node", "scripts/check-dependencies.cjs"]
required = true
```

The project must implement that checker to compare installed dependencies to
its lockfile and runtime needs. Merely checking that a directory exists does
not establish compatibility. Cambium runs the check again in each destination;
`cambium env validate NAME` explicitly repeats it after changes. `env explain`
only observes state and does not run policy commands.

## Unknown package managers

No Cambium source change is needed. Describe only their filesystem state:

```toml
version = 1

[[path]]
path = ".newpm/deps"
policy = "clone"
inputs = ["newpm.lock", "newpm.toml"]

[[path]]
path = ".newpm/runtime"
policy = "recreate"
inputs = ["newpm.lock"]
prepare = ["newpm", "sync", "--frozen"]
validate = ["newpm", "verify"]
```

See [PATH_POLICIES.md](PATH_POLICIES.md) for policy semantics.
