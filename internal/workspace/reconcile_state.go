package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

type reconcileWorktree struct {
	gitx.Worktree
	GitDir  string
	Owner   string
	Primary bool
	Valid   bool
}

func (m *Manager) reconcileWorktrees(ctx context.Context) ([]reconcileWorktree, error) {
	values, err := m.Project.Repository.ListWorktrees(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]reconcileWorktree, 0, len(values))
	for i, value := range values {
		value.Path = reconcilePath(value.Path)
		// Git documents that worktree list always lists the main worktree first,
		// independently of which linked checkout invokes the command.
		item := reconcileWorktree{Worktree: value, Primary: i == 0}
		info, err := os.Stat(value.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect worktree %s: %w", value.Path, err)
		}
		if err == nil && info.IsDir() && !item.Primary && !value.Bare {
			item.GitDir, item.Owner, err = m.Native.ReadWorktreeOwner(ctx, value.Path)
			if err != nil && !errors.Is(err, native.ErrWorktreeOwnership) {
				return nil, err
			}
			item.Valid = item.GitDir != ""
		}
		result = append(result, item)
	}
	return result, nil
}

func matchingWorktree(value model.Workspace, worktrees []reconcileWorktree) *reconcileWorktree {
	if value.ID == "" {
		return nil
	}
	// Paths, branches, and even Git administration directories can be reused.
	// A matching receipt is required for new and legacy workspace metadata.
	var match *reconcileWorktree
	for i := range worktrees {
		item := &worktrees[i]
		if !item.Primary && item.Valid && item.Owner == value.ID {
			if match != nil {
				return nil // Duplicated receipts do not prove a unique identity.
			}
			match = item
		}
	}
	return match
}

func reconcileState(value model.Workspace, worktrees []reconcileWorktree) (model.Workspace, []ReconcileChange) {
	var changes []ReconcileChange
	item := matchingWorktree(value, worktrees)
	if item == nil {
		if value.Status != model.WorkspaceBroken {
			changes = append(changes, ReconcileChange{Name: value.Name, Action: "mark-broken", From: value.Path})
			value.Status = model.WorkspaceBroken
		}
		return value, changes
	}
	if value.Path != item.Path {
		changes = append(changes, ReconcileChange{Name: value.Name, Action: "move", From: value.Path, To: item.Path})
		value.Path = item.Path
	}
	if value.Branch != item.Branch {
		changes = append(changes, ReconcileChange{Name: value.Name, Action: "update-branch", From: value.Branch, To: item.Branch})
		value.Branch = item.Branch
	}
	if value.Status != model.WorkspaceReady {
		changes = append(changes, ReconcileChange{Name: value.Name, Action: "mark-ready", To: item.Path})
		value.Status = model.WorkspaceReady
	}
	value.WorktreeGitDir = item.GitDir
	value.LastUsedAt = time.Now().UTC()
	return value, changes
}

func unmanagedWorktrees(registered []model.Workspace, worktrees []reconcileWorktree) []string {
	managed := make(map[string]bool)
	for _, value := range registered {
		if item := matchingWorktree(value, worktrees); item != nil {
			managed[item.Path] = true
		}
		path := reconcilePath(value.Path)
		// A known missing/corrupt registration remains a broken managed
		// workspace; it is not a new worktree waiting to be adopted.
		for _, item := range worktrees {
			if !item.Valid && item.Path == path {
				managed[item.Path] = true
			}
		}
	}
	var unmanaged []string
	for _, item := range worktrees {
		if !item.Primary && !item.Bare && !managed[item.Path] {
			unmanaged = append(unmanaged, item.Path)
		}
	}
	return unmanaged
}

func reconcilePath(path string) string {
	if canonical, err := existingCanonicalPath(path); err == nil {
		return canonical
	}
	return filepath.Clean(path)
}
