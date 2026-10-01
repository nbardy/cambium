# Configuration

Cambium has two configuration surfaces with deliberately different trust and
versioning roles.

## `.cambium.json`: local operational settings

`cambium init` writes `.cambium.json`:

```json
{
  "version": 3,
  "branch_prefix": "cambium/",
  "materializer": "auto",
  "require_cow": false,
  "prepared_index": true,
  "require_ignored_layers": true,
  "allow_policy_commands": false,
  "layers": []
}
```

Fields:

- `materializer`: `auto`, `cow`, `git`, or diagnostic `copy`.
- `require_cow`: fail instead of falling back to normal Git checkout.
- `prepared_index`: seed worktree indexes from immutable baselines.
- `require_ignored_layers`: require managed outputs to be Git-ignored unless a
  rule explicitly opts out.
- `allow_policy_commands`: allow reviewed `prepare`/`validate` argv from policy
  rules to execute. The default is false.
- `layers`: legacy JSON path overrides; `.cambium.toml` is preferred for new
  project-specific policy.

This file is loaded from the invocation's primary checkout and controls local
operation. It is not used as a branch-varying environment receipt input.

## `.cambium.toml`: target-tree path policy

`.cambium.toml` is read from the **exact target Git commit or speculative root**.
A branch can therefore carry policy that matches its own build layout.

```toml
version = 1

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

Supported keys:

- `name`
- `path` (`pattern` is an alias, but output paths must be concrete)
- `policy` (`mode` is an alias)
- `inputs` (`fingerprint` is a legacy alias)
- `prepare` and `validate` as direct argv arrays
- `allow_unignored`
- `source_sensitive`
- `required`
- `activate_on_inputs`
- `priority`

The parser accepts a strict, documented TOML subset: double-quoted strings,
booleans, integers, and one- or multi-line string arrays inside `[[path]]`
tables. Unknown and duplicate keys fail closed.

## Precedence

For one exact output path:

```text
built-in default
    < legacy .cambium.json layer
    < target-tree .cambium.toml rule
```

Overlapping parent/child output rules are rejected because two policies cannot
safely own the same subtree.

## Commands and trust

Policy commands execute directly as argv in the target workspace; Cambium does
not invoke a shell unless the policy explicitly names a shell executable.

Tracked repository policy can execute code, so commands are disabled by
default. Enable them only after review:

```bash
cambium init --force --allow-policy-commands
```

or set:

```json
"allow_policy_commands": true
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
