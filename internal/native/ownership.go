package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/model"
)

// ErrWorktreeOwnership means a registration cannot be attributed to the
// recorded workspace. Callers may report it as broken without treating a
// cancellation or an unrelated Git/filesystem failure the same way.
var ErrWorktreeOwnership = errors.New("worktree ownership cannot be verified")

func readWorktreeOwner(marker string) (string, error) {
	info, err := os.Lstat(marker)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: ownership receipt is not a regular file", ErrWorktreeOwnership)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	owner := strings.TrimSpace(string(data))
	if owner == "" {
		return "", fmt.Errorf("%w: ownership receipt is empty", ErrWorktreeOwnership)
	}
	return owner, nil
}

// ReadWorktreeOwner reads an existing receipt from a linked worktree in this
// repository. The Git directory is only a locator: Git can reuse that pathname
// after removing a worktree. The receipt's workspace ID identifies its owner.
// No receipt is created, including for legacy metadata.
func (b *Backend) ReadWorktreeOwner(ctx context.Context, path string) (gitDir, owner string, err error) {
	marker, err := b.ownerMarker(ctx, path)
	if err != nil {
		return "", "", err
	}
	gitDir = filepath.Dir(marker)
	owner, err = readWorktreeOwner(marker)
	if errors.Is(err, os.ErrNotExist) {
		return gitDir, "", fmt.Errorf("%w: missing ownership receipt at %s; inspect and migrate legacy workspace metadata manually", ErrWorktreeOwnership, marker)
	}
	return gitDir, owner, err
}

// VerifyWorkspaceOwnership checks the current registration and existing owner
// receipt without changing either. Destructive callers hold the named
// workspace and Git administration locks through the subsequent mutation.
func (b *Backend) VerifyWorkspaceOwnership(ctx context.Context, value model.Workspace) (string, error) {
	if value.ID == "" {
		return "", fmt.Errorf("%w: workspace %q has no identity; inspect its metadata manually", ErrWorktreeOwnership, value.Name)
	}
	worktree, found, err := b.registeredWorktree(ctx, value.Path)
	if err != nil {
		return "", err
	}
	if !found || worktree.Bare || worktree.Branch != value.Branch {
		return "", fmt.Errorf("%w: registration at %s does not match recorded branch %q", ErrWorktreeOwnership, value.Path, value.Branch)
	}
	info, err := os.Stat(value.Path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return "", fmt.Errorf("%w: worktree directory %s is missing", ErrWorktreeOwnership, value.Path)
	}
	if err != nil {
		return "", err
	}
	gitDir, owner, err := b.ReadWorktreeOwner(ctx, value.Path)
	if err != nil {
		return "", err
	}
	if owner != value.ID {
		return "", fmt.Errorf("%w: worktree at %s belongs to another workspace; refusing to claim a replacement", ErrWorktreeOwnership, value.Path)
	}
	return gitDir, nil
}
