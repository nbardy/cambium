package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/model"
)

// CheckAdoption rejects primary checkouts and existing workspace claims. The
// caller holds the named workspace, cache-admin, and Git administration locks
// through registry publication, so a create cannot reserve the same name after
// this check and pruning cannot remove freshly captured environments before
// they become referenced. The returned directory locates the current Git
// registration; an owner receipt, not this reusable pathname, proves identity.
func (b *Backend) CheckAdoption(ctx context.Context, name string, expected gitx.Worktree) (string, error) {
	if err := validateWorkspaceName(name); err != nil {
		return "", err
	}
	worktrees, err := b.Project.Repository.ListWorktrees(ctx)
	if err != nil {
		return "", err
	}
	found := false
	for _, value := range worktrees {
		path, pathErr := canonicalCreationPath(value.Path)
		if pathErr != nil || path != expected.Path {
			continue
		}
		if value.Bare || value.Branch != expected.Branch || value.HEAD != expected.HEAD {
			return "", fmt.Errorf("worktree registration changed while adopting %s; retry after the worktree is stable", expected.Path)
		}
		found = true
		break
	}
	if !found {
		return "", fmt.Errorf("%s is no longer registered as a Git linked worktree", expected.Path)
	}
	marker, err := b.ownerMarker(ctx, expected.Path)
	if err != nil {
		return "", err
	}
	gitDir, err := canonicalCreationPath(filepath.Dir(marker))
	if err != nil {
		return "", err
	}
	commonDir, err := canonicalCreationPath(b.Project.Repository.CommonGitDir)
	if err != nil {
		return "", err
	}
	if gitDir == commonDir {
		return "", errors.New("the primary worktree cannot be adopted as an agent workspace")
	}
	values, err := b.Registry.List()
	if err != nil {
		return "", err
	}
	for _, value := range values {
		if value.Name == name {
			return "", fmt.Errorf("workspace %q already exists", name)
		}
		path, _ := canonicalCreationPath(value.Path)
		claimedGitDir := ""
		if value.WorktreeGitDir != "" {
			claimedGitDir, _ = canonicalCreationPath(value.WorktreeGitDir)
		}
		if path == expected.Path || value.Branch == expected.Branch || claimedGitDir == gitDir {
			return "", fmt.Errorf("worktree %s is claimed by workspace %q; reconcile its verified ownership or inspect stale metadata before adopting", expected.Path, value.Name)
		}
	}
	records, err := b.Journal.List()
	if err != nil {
		return "", err
	}
	for _, record := range records {
		path, _ := canonicalCreationPath(record.Path)
		if record.Name == name || path == expected.Path || record.Branch == expected.Branch {
			return "", fmt.Errorf("worktree or name is reserved by pending operation %s; finish or recover it before adoption", record.ID)
		}
	}
	if _, err := os.Lstat(marker); err == nil {
		return "", fmt.Errorf("worktree %s has an existing ownership receipt; inspect or recover its original workspace before adoption", expected.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return gitDir, nil
}

// ClaimAdoption establishes ownership only after rechecking an explicitly
// requested adoption. Callers publish registry metadata before releasing their
// workspace/cache/Git locks. A failed publication may retain the receipt for
// inspection, but may never leave metadata without its ownership evidence.
func (b *Backend) ClaimAdoption(ctx context.Context, value model.Workspace) error {
	gitDir, err := b.CheckAdoption(ctx, value.Name, gitx.Worktree{Path: value.Path, Branch: value.Branch, HEAD: value.BaseCommit})
	if err != nil {
		return err
	}
	if gitDir != value.WorktreeGitDir {
		return fmt.Errorf("Git administration directory changed while adopting %s", value.Path)
	}
	claimedDir, err := b.claimWorktree(ctx, value.Path, value.Branch, value.ID)
	if err != nil {
		return err
	}
	if claimedDir != gitDir {
		return fmt.Errorf("Git administration directory changed while recording ownership of %s; inspect the retained receipt", value.Path)
	}
	return nil
}
