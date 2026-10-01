# Ecosystem defaults

Cambium's built-ins are filesystem-policy data passed through one generic
engine. There is no Yarn lockfile parser, npm resolver, Cargo adapter, or uv
implementation in the core.

A built-in acts only when its output is ignored and either exists in a source
workspace, has a compatible cached layer, or is explicitly activated by input
files. Tracked outputs always remain with Git.

## JavaScript and TypeScript

Works with npm, Yarn, pnpm, Bun, and Node-backed ClojureScript tooling.

| Output | Default |
|---|---|
| `node_modules` | `clone` |
| `.yarn/cache` | `clone` when ignored; tracked zero-install caches stay with Git |
| `.yarn/unplugged` | `clone` |
| `.pnp.cjs`, `.pnp.loader.mjs` | `clone` only when ignored; tracked PnP maps stay with Git |
| `.yarn/install-state.gz`, `.yarn/sdks` | `seed` |
| `.pnpm-store` | `clone` only for project-local stores |
| `.next`, `.nuxt`, `.svelte-kit`, `.vite`, `.turbo` | `seed` |

Receipt inputs include package manifests, workspace manifests, npm/Yarn/pnpm/Bun
lockfiles, package-manager config, TypeScript configs, framework configs, and
common runtime marker files.

There is no `yarn.toml` required from the user.

## Python

Works with uv, pip, Poetry, PDM, Hatch-style projects, tox/nox, and standard
virtual environments.

| Output | Default |
|---|---|
| `.venv`, `venv` | `recreate` |
| `.tox`, `.nox`, `.pixi`, `.conda` | `recreate` |
| `.pytest_cache`, `.mypy_cache`, `.ruff_cache`, `.pyright` | `seed` |
| root `__pycache__` | `empty` |

Inputs include `pyproject.toml`, `uv.lock`, Poetry/PDM/Pipenv locks,
requirements files, setup metadata, and Python-version markers.

Cambium does not clone virtual environments by default because they commonly
contain absolute interpreter paths and shebangs. A project that wants automatic
recreation can add reviewed `prepare = ["uv", "sync", "--frozen"]` policy and
enable policy commands locally.

## Rust and Cargo

| Output | Default |
|---|---|
| `target` | `seed` |

Inputs include workspace Cargo manifests, `Cargo.lock`, Cargo config, and Rust
toolchain markers. Cargo's global registry/Git caches remain outside the
worktree and under Cargo's own management. `target` is disposable because Cargo
build metadata can contain source-path-sensitive state.

## Clojure and ClojureScript

Works with Clojure CLI, Leiningen, Babashka, shadow-cljs, Figwheel, and
Node-backed CLJS builds.

| Output | Default |
|---|---|
| `target` | `seed` |
| `.cpcache`, `.lsp`, `.clj-kondo/.cache` | `seed` |
| `.shadow-cljs`, `.figwheel-main` | `seed` |
| `node_modules` | generic Node `clone` rule |

Inputs include `deps.edn`, `project.clj`, `shadow-cljs.edn`, `bb.edn`, and
Figwheel/toolchain markers.

## JVM, Go, and additional defaults

| Ecosystem | Outputs |
|---|---|
| Maven/Gradle/Java/Kotlin | `target`, `build`, `.gradle`, `out` as seeds |
| Go | ignored `vendor` as clone; global module/build caches remain external |
| Ruby/PHP | ignored `vendor` as clone, `.bundle` as seed |
| Elixir | `deps` clone, `_build` seed |
| Dart/Flutter | `.dart_tool` seed |
| .NET | `obj` and `bin` seeds |
| Swift/CocoaPods | `.build` seed, ignored `Pods` clone |
| C/C++ | `vcpkg_installed` clone, generic build outputs as seeds |

## Custom ecosystems

Use `.cambium.toml` to describe paths and input files. The package manager
continues to run normally inside an ordinary Git worktree; only its filesystem
outputs need policy.
