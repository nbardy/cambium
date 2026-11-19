#!/usr/bin/env sh
# End-to-end test of integrations/ against real git and a real cambium binary.
set -eu

CAMBIUM_BIN=${CAMBIUM_BIN:-cambium}
HERE=$(cd "$(dirname "$0")/.." && pwd -P)
PATH=$(dirname "$CAMBIUM_BIN"):$PATH
export PATH
# Resolve symlinks (macOS /var -> /private/var): Cambium reports canonical paths.
ROOT=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/cambium-integration.XXXXXX")" && pwd -P)
trap 'chmod -R u+w "$ROOT" 2>/dev/null; rm -rf "$ROOT"' EXIT INT TERM
# Never read or write the developer's own config, registry or workspace root.
export CAMBIUM_CONFIG="$ROOT/no-user-config.toml"
export CAMBIUM_STATE_DIR="$ROOT/state"

fail() { echo "FAIL: $*" >&2; exit 1; }
worktrees() { git -C "$REPO" worktree list --porcelain | grep -c '^worktree '; }

REPO="$ROOT/project"
mkdir -p "$REPO/src" "$REPO/node_modules/pkg"
cd "$REPO"
git init -q -b main
git config user.name 'Integration Test'
git config user.email 'integration@example.invalid'
printf 'node_modules/\n' > .gitignore
printf 'one\n' > src/value.txt
git add . && git commit -q -m one
printf 'two\n' > src/value.txt
git commit -q -am two
printf 'module\n' > node_modules/pkg/index.js
HEAD_SHA=$(git rev-parse HEAD)

# --- Codex: CODEX_WORKTREE_COMMAND contract -------------------------------
CODEX_ROOT="$ROOT/codex-home/worktrees/a1b2/project"
mkdir -p "$(dirname "$CODEX_ROOT")"
"$HERE/integrations/codex-worktree-command" create "$REPO" "$CODEX_ROOT" "$HEAD_SHA"
test "$(git -C "$CODEX_ROOT" rev-parse HEAD)" = "$HEAD_SHA" || fail "codex: wrong HEAD"
test -z "$(git -C "$CODEX_ROOT" status --porcelain --untracked-files=no)" || fail "codex: tracked files not clean"
test "$(cat "$CODEX_ROOT/src/value.txt")" = two || fail "codex: wrong content"
test -f "$CODEX_ROOT/node_modules/pkg/index.js" || fail "codex: node_modules was not cloned"
test "$(worktrees)" = 2 || fail "codex: worktree not registered"

"$HERE/integrations/codex-worktree-command" remove "$REPO" "$CODEX_ROOT"
test ! -e "$CODEX_ROOT" || fail "codex: worktree not removed"
test "$(worktrees)" = 1 || fail "codex: worktree still registered"
test -z "$(git -C "$REPO" branch --list 'codex/*')" || fail "codex: merged branch not deleted"

# A commit made in the worktree that nothing else holds is unpushed work: the adapter's remove
# refuses (so Codex keeps the worktree) until the commit is held elsewhere; then the branch is deleted.
"$HERE/integrations/codex-worktree-command" create "$REPO" "$CODEX_ROOT" "$HEAD_SHA"
git -C "$CODEX_ROOT" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m agent-work
if "$HERE/integrations/codex-worktree-command" remove "$REPO" "$CODEX_ROOT" 2>/dev/null; then
  fail "codex: removed a worktree holding an unpushed commit"
fi
test -d "$CODEX_ROOT" || fail "codex: refused removal still deleted the worktree"
git -C "$REPO" merge -q --ff-only codex/a1b2
"$HERE/integrations/codex-worktree-command" remove "$REPO" "$CODEX_ROOT"
test ! -e "$CODEX_ROOT" || fail "codex: worktree not removed once its commit is merged"
test "$(git -C "$REPO" log -1 --format=%s)" = agent-work || fail "codex: agent commit lost"
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD) # main moved with the merge

# --- Claude Code: WorktreeCreate / WorktreeRemove hooks -------------------
HOOK="$HERE/integrations/claude-worktree-hook"
CLAUDE_WORKTREE_ROOT="$ROOT/claude-worktrees"
export CLAUDE_WORKTREE_ROOT
OUT=$(printf '{"hook_event_name":"WorktreeCreate","session_id":"s","cwd":"%s","name":"fix auth/bug"}' "$REPO" | "$HOOK" create 2>/dev/null)
test "$(printf '%s\n' "$OUT" | wc -l | tr -d ' ')" = 1 || fail "claude: stdout must be exactly one line, got: $OUT"
case "$OUT" in
  "$CLAUDE_WORKTREE_ROOT"/project-*/fix-auth-bug) ;;
  *) fail "claude: unexpected path: $OUT" ;;
esac
test "$(git -C "$OUT" rev-parse HEAD)" = "$HEAD_SHA" || fail "claude: wrong HEAD"
test -f "$OUT/node_modules/pkg/index.js" || fail "claude: node_modules was not cloned"

printf '{"hook_event_name":"WorktreeRemove","session_id":"s","path":"%s"}' "$OUT" | "$HOOK" remove 2>/dev/null
test ! -e "$OUT" || fail "claude: worktree not removed"
test "$(worktrees)" = 1 || fail "claude: worktree still registered"
# Removing an already-removed worktree is a no-op, not an error.
printf '{"hook_event_name":"WorktreeRemove","session_id":"s","path":"%s"}' "$OUT" | "$HOOK" remove 2>/dev/null

echo "Cambium integration test passed"
