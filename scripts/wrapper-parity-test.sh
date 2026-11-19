#!/usr/bin/env sh
# Real-command check of the behaviours that replaced the wrapper scripts:
# default workspace root, submodules, and a Cargo dependency seed built by a
# real cargo run. Needs git; the seed part also needs cargo and a cached
# registry crate (cfg-if), and is skipped with a message when they are absent.
set -eu

CAMBIUM_BIN=${CAMBIUM_BIN:-cambium}
PATH=$(dirname "$CAMBIUM_BIN"):$PATH
export PATH
ROOT=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/cambium-parity.XXXXXX")" && pwd -P)
trap 'rm -rf "$ROOT"' EXIT INT TERM
export CAMBIUM_CONFIG="$ROOT/user.toml"
export CAMBIUM_STATE_DIR="$ROOT/state"
export CAMBIUM_SEED_ROOT="$ROOT/seed"
export GIT_CONFIG_GLOBAL="$ROOT/gitconfig"
git config --global user.name parity
git config --global user.email parity@example.invalid
git config --global protocol.file.allow always
git config --global init.defaultBranch main

fail() { echo "FAIL: $*" >&2; exit 1; }

# ---- 1. default workspace root -------------------------------------------
REPO="$ROOT/repo"
git init -q "$REPO"
cd "$REPO"
printf 'x\n' > a.txt
git add . && git commit -q -m init
printf 'version = 1\n[settings]\nworkspace_root = "%s/wt"\n' "$ROOT" > "$CAMBIUM_CONFIG"
GOT=$(git cambium add --print-path -b feat-one -name one -ephemeral)
test "$GOT" = "$ROOT/wt/one" || fail "user workspace_root: got $GOT"
test -f "$ROOT/wt/one/a.txt" || fail "workspace not materialized"
# A repository's committed setting wins over the user's.
printf 'version = 1\n[settings]\nworkspace_root = "%s/repo-wt"\n' "$ROOT" > .cambium.toml
git add .cambium.toml && git commit -q -m cfg
GOT=$(git cambium add --print-path -b feat-two -name two -ephemeral)
test "$GOT" = "$ROOT/repo-wt/two" || fail "repo workspace_root: got $GOT"
# NAME lookup from an unrelated directory finds both through the registry.
cd "$ROOT"
test "$(cambium path one)" = "$ROOT/wt/one" || fail "path NAME from elsewhere"
cambium remove one >/dev/null
cambium remove two >/dev/null
test ! -e "$ROOT/wt/one" && test ! -e "$ROOT/repo-wt/two" || fail "remove left a workspace"
echo "ok default workspace root"

# ---- 2. submodules ---------------------------------------------------------
KIT="$ROOT/kit-origin"
git init -q "$KIT"
printf 'kit\n' > "$KIT/kit.txt"
git -C "$KIT" add . && git -C "$KIT" commit -q -m kit
SUPER="$ROOT/super"
git init -q "$SUPER"
cd "$SUPER"
git submodule -q add "$KIT" kit
printf 'version = 1\n[settings]\nsubmodules = true\nworkspace_root = "%s/wt"\n' "$ROOT" > .cambium.toml
git add . && git commit -q -m super
WS=$(git cambium add --print-path -b sub-ws -name sub -ephemeral)
test "$(cat "$WS/kit/kit.txt")" = kit || fail "submodule not populated"
test "$(git -C "$WS/kit" remote get-url origin)" = "$KIT" || fail "submodule remote not restored to .gitmodules"
test -z "$(git -C "$WS/kit" status --porcelain)" || fail "fresh submodule is dirty"
# Unpushed commit inside the submodule: remove must refuse and keep the work.
PINNED=$(git -C "$WS/kit" rev-parse HEAD)
printf 'more\n' > "$WS/kit/more.txt"
git -C "$WS/kit" add . && git -C "$WS/kit" commit -q -m unpushed
if cambium remove sub >/dev/null 2>"$ROOT/err"; then fail "remove succeeded with an unpushed submodule commit"; fi
grep -q "submodule-unpushed" "$ROOT/err" || fail "refusal does not name the submodule: $(cat "$ROOT/err")"
test -d "$WS/kit" || fail "refused remove still deleted the workspace"
git -C "$WS/kit" push -q origin HEAD:refs/heads/lane
git -C "$WS/kit" checkout -q "$PINNED" # pointer back to the committed one; the work is now on the remote
cambium remove sub >/dev/null
test ! -e "$WS" || fail "clean submodule workspace not removed"
echo "ok submodules"

# ---- 3. cargo-deps seed from a real cargo build ------------------------------
if ! command -v cargo >/dev/null 2>&1; then echo "skip cargo-deps seed: no cargo"; exit 0; fi
CRATE="$ROOT/crate"
mkdir -p "$CRATE/src"
cd "$CRATE"
git init -q .
cat > Cargo.toml <<'TOML'
[package]
name = "seedmember"
version = "0.1.0"
edition = "2021"
[dependencies]
cfg-if = "1"
TOML
printf 'fn main() { cfg_if::cfg_if! { if #[cfg(unix)] { println!("unix"); } else { println!("other"); } } }\n' > src/main.rs
printf 'target/\n' > .gitignore
printf 'version = 1\n[settings]\nworkspace_root = "%s/wt"\n[[path]]\npath = "target"\npolicy = "seed"\nseed = "cargo-deps"\n' "$ROOT" > .cambium.toml
if ! cargo generate-lockfile --offline >/dev/null 2>&1; then echo "skip cargo-deps seed: cfg-if is not in the local registry cache"; exit 0; fi
git add . && git commit -q -m crate
env -u CARGO_TARGET_DIR -u CARGO_BUILD_BUILD_DIR cargo build --offline -q
ls target/debug/.fingerprint | grep -q '^cfg-if-' || fail "build produced no cfg-if unit"
ls target/debug/.fingerprint | grep -q '^seedmember-' || fail "build produced no member unit"
cambium seed refresh >/dev/null
WS=$(git cambium add --print-path -b seeded -name seeded -ephemeral)
ls "$WS/target/debug/.fingerprint" | grep -q '^cfg-if-' || fail "dependency unit missing from the new workspace"
if ls "$WS/target/debug/.fingerprint" | grep -q '^seedmember-'; then fail "member unit travelled with the seed"; fi
# The seeded workspace builds offline and only compiles the member.
(cd "$WS" && env -u CARGO_TARGET_DIR -u CARGO_BUILD_BUILD_DIR cargo build --offline 2>&1 | grep -q 'Compiling seedmember') || fail "member was not compiled in the workspace"
(cd "$WS" && env -u CARGO_TARGET_DIR -u CARGO_BUILD_BUILD_DIR cargo build --offline 2>&1 | grep -q 'Compiling cfg-if' ) && fail "cfg-if was rebuilt despite the seed"
cambium remove --force seeded >/dev/null
echo "ok cargo-deps seed"
