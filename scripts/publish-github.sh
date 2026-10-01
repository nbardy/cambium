#!/usr/bin/env sh
set -eu

OWNER=${OWNER:-nbardy}
REPO=${REPO:-cambium}
VISIBILITY=${VISIBILITY:-public}
REMOTE_URL="https://github.com/$OWNER/$REPO.git"

command -v gh >/dev/null 2>&1 || {
  echo 'GitHub CLI (gh) is required.' >&2
  exit 1
}
gh auth status >/dev/null
gh auth setup-git >/dev/null

case "$VISIBILITY" in
  public|private|internal) ;;
  *) echo "VISIBILITY must be public, private, or internal" >&2; exit 1 ;;
esac

if gh repo view "$OWNER/$REPO" >/dev/null 2>&1; then
  if git remote get-url origin >/dev/null 2>&1; then
    git remote set-url origin "$REMOTE_URL"
  else
    git remote add origin "$REMOTE_URL"
  fi
else
  # Release bundles may carry a local `origin`; never push back into that file.
  if git remote get-url origin >/dev/null 2>&1; then
    git remote remove origin
  fi
  gh repo create "$OWNER/$REPO" "--$VISIBILITY" \
    --description "Git-native CoW worktrees, branch-correct environments, and speculative checkpoints for coding agents" \
    --source=. --remote=origin
fi

git push -u origin HEAD:main --follow-tags
printf '%s\n' "https://github.com/$OWNER/$REPO"
