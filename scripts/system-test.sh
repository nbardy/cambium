#!/usr/bin/env sh
set -eu

CAMBIUM_BIN=${CAMBIUM_BIN:-cambium}
PATH=$(dirname "$CAMBIUM_BIN"):$PATH
export PATH
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/cambium-system.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT INT TERM
REPO="$ROOT/repo"
mkdir -p "$REPO"
cd "$REPO"

git init -q -b main
git config user.name 'Cambium System Test'
git config user.email 'system-test@example.invalid'
mkdir -p src node_modules/pkg
printf 'base\n' > src/value.txt
printf 'module\n' > node_modules/pkg/index.js
printf '{}\n' > package-lock.json
printf 'SECRET=do-not-copy\n' > .env
printf 'node_modules/\n.env\n' > .gitignore
git add .
git commit -q -m initial

"$CAMBIUM_BIN" init >/dev/null
grep -q '"version": 3' .cambium.json
grep -q '"allow_policy_commands": false' .cambium.json
"$CAMBIUM_BIN" prepare >/dev/null

# The Git-facing constructor still creates a normal registered linked worktree.
GIT_STYLE_PATH="$ROOT/git-style"
GIT_STYLE=$(git cambium add --materializer git --print-path -b git-style "$GIT_STYLE_PATH" main)
test "$GIT_STYLE" = "$GIT_STYLE_PATH"
git worktree list --porcelain | grep -q "worktree $GIT_STYLE_PATH"
"$CAMBIUM_BIN" remove --force --delete-branch git-style >/dev/null

WORKSPACE_JSON=$("$CAMBIUM_BIN" create --json system)
WORKSPACE=$(printf '%s' "$WORKSPACE_JSON" | sed -n 's/.*"path": "\([^"]*\)".*/\1/p')
test -n "$WORKSPACE"
test "$(cat "$WORKSPACE/src/value.txt")" = base
test "$(cat "$WORKSPACE/node_modules/pkg/index.js")" = module
test ! -e "$WORKSPACE/.env"
"$CAMBIUM_BIN" env explain system | grep -q '^clone[[:space:]]*node_modules'
printf 'agent\n' > "$WORKSPACE/src/value.txt"
test "$(cat src/value.txt)" = base
git -C "$WORKSPACE" status --porcelain | grep -q 'src/value.txt'
printf 'speculative\n' > "$WORKSPACE/speculative.txt"
ROOT_JSON=$("$CAMBIUM_BIN" checkpoint --as system-root --json system)
ROOT_ID=$(printf '%s' "$ROOT_JSON" | sed -n 's/.*"root": "\([^"]*\)".*/\1/p' | head -1)
test -n "$ROOT_ID"
FORK_JSON=$("$CAMBIUM_BIN" fork --materializer git --json system-root speculative-copy)
FORK_PATH=$(printf '%s' "$FORK_JSON" | sed -n 's/.*"path": "\([^"]*\)".*/\1/p')
test -n "$FORK_PATH"
test "$(cat "$FORK_PATH/src/value.txt")" = agent
test "$(cat "$FORK_PATH/speculative.txt")" = speculative
"$CAMBIUM_BIN" root show system-root | grep -q "root: $ROOT_ID"
"$CAMBIUM_BIN" remove --force --delete-branch speculative-copy >/dev/null

# Reusing an alias advances the workspace's immutable speculative lineage.
printf 'agent-v2\n' > "$WORKSPACE/src/value.txt"
ROOT2_JSON=$("$CAMBIUM_BIN" checkpoint --as system-root --json system)
ROOT2_ID=$(printf '%s' "$ROOT2_JSON" | sed -n 's/.*"root": "\([^"]*\)".*/\1/p' | head -1)
ROOT2_PARENT=$(printf '%s' "$ROOT2_JSON" | sed -n 's/.*"parent": "\([^"]*\)".*/\1/p')
test -n "$ROOT2_ID"
test "$ROOT2_ID" != "$ROOT_ID"
test "$ROOT2_PARENT" = "$ROOT_ID"
"$CAMBIUM_BIN" root show system-root | grep -q "root: $ROOT2_ID"
"$CAMBIUM_BIN" root diff "$ROOT_ID" "$ROOT2_ID" | grep -q 'src/value.txt'

# --detach starts a new logical root lineage instead of inheriting the
# workspace's current speculative root.
printf 'agent-v3\n' > "$WORKSPACE/src/value.txt"
DETACHED_JSON=$("$CAMBIUM_BIN" checkpoint --detach --as detached-root --json system)
DETACHED_ID=$(printf '%s' "$DETACHED_JSON" | sed -n 's/.*"root": "\([^"]*\)".*/\1/p' | head -1)
DETACHED_PARENT=$(printf '%s' "$DETACHED_JSON" | sed -n 's/.*"parent": "\([^"]*\)".*/\1/p')
test -n "$DETACHED_ID"
test "$DETACHED_ID" != "$ROOT2_ID"
test -z "$DETACHED_PARENT"

"$CAMBIUM_BIN" run system -- sh -c 'test "$CAMBIUM_WORKSPACE" = system && printf ok > command-result'
test "$(cat "$WORKSPACE/command-result")" = ok
"$CAMBIUM_BIN" remove --force --delete-branch system >/dev/null
test ! -e "$WORKSPACE"

# Exercise independent CLI processes racing through the same shared Git
# administration directory and prepared-layer cache.
pids=""
for index in 1 2 3 4; do
  "$CAMBIUM_BIN" create --json --materializer git "parallel-$index" >"$ROOT/parallel-$index.out" &
  pids="$pids $!"
done
for pid in $pids; do
  wait "$pid"
done
for index in 1 2 3 4; do
  PARALLEL_PATH=$(sed -n 's/.*"path": "\([^"]*\)".*/\1/p' "$ROOT/parallel-$index.out")
  test -n "$PARALLEL_PATH"
  test "$(cat "$PARALLEL_PATH/src/value.txt")" = base
  test "$(cat "$PARALLEL_PATH/node_modules/pkg/index.js")" = module
  "$CAMBIUM_BIN" remove --force --delete-branch "parallel-$index" >/dev/null
done

# Simulate a process dying after Git registered the linked worktree. The
# durable operation record must survive the command and recovery must remove
# only the unadvanced branch it owns.
if CAMBIUM_FAILPOINT=after-register "$CAMBIUM_BIN" create --materializer git interrupted >"$ROOT/interrupted.out" 2>"$ROOT/interrupted.err"; then
  echo 'failpoint create unexpectedly succeeded' >&2
  exit 1
fi
"$CAMBIUM_BIN" recover | grep -q 'resolved.*rollback.*interrupted'
if git show-ref --verify --quiet refs/heads/cambium/interrupted; then
  echo 'recovery retained an unadvanced interrupted branch' >&2
  exit 1
fi

"$CAMBIUM_BIN" recover --dry-run | grep -q 'no interrupted operations'
"$CAMBIUM_BIN" doctor >/dev/null

printf 'Cambium system test passed\n'
