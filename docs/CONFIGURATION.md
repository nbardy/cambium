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
| `allow_policy_commands` | `false` | **local file only**; see [Commands and trust](#commands-and-trust) |

### Path rule keys

- `name`
- `path` (a concrete relative path; globs are rejected)
- `policy`: `clone`, `seed`, `recreate`, `share`, `empty`, or `skip`
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
