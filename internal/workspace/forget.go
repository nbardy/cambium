package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/state"
)

// ForgetChange names one workspace record dropped by Forget.
type ForgetChange struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ForgetResult struct {
	Forgotten []ForgetChange `json:"forgotten"`
	DryRun    bool           `json:"dry_run"`
}

// WorktreeRemainsError means Forget kept a record because something of the
// workspace is still on disk or in Git. Nothing was changed.
type WorktreeRemainsError struct {
	Name   string
	Reason string
}

func (e *WorktreeRemainsError) Error() string {
	return fmt.Sprintf("workspace %q is not gone: %s", e.Name, e.Reason)
}

// Forget drops the metadata of workspaces whose worktree no longer exists.
//
// Remove cannot do this: it must prove ownership from a receipt inside the
// worktree before deleting anything, and a worktree deleted by another tool
// has no receipt left to read. Such a record stays "broken" and keeps its
// baseline and layers referenced, so prune never releases them. Forget deletes
// only the record. It never touches a directory, a Git registration or a
// branch, so the proof it needs is absence: see worktreeRemains.
//
// A named workspace that is not gone is an error and stops the call.
func (m *Manager) Forget(ctx context.Context, names []string, dryRun bool) (ForgetResult, error) {
	result := ForgetResult{DryRun: dryRun}
	for _, name := range names {
		change, err := m.forgetOne(ctx, name, dryRun)
		if err != nil {
			return result, err
		}
		result.Forgotten = append(result.Forgotten, change)
	}
	return result, nil
}

// ForgetMissing applies Forget to every registered workspace and skips the ones
// that are not gone.
func (m *Manager) ForgetMissing(ctx context.Context, dryRun bool) (ForgetResult, error) {
	result := ForgetResult{DryRun: dryRun}
	values, err := m.Registry.List()
	if err != nil {
		return result, err
	}
	for _, value := range values {
		change, err := m.forgetOne(ctx, value.Name, dryRun)
		var remains *WorktreeRemainsError
		// The list is a snapshot: a record removed since then is already gone.
		if errors.As(err, &remains) || errors.Is(err, state.ErrWorkspaceNotFound) {
			continue
		}
		if err != nil {
			return result, err
		}
		result.Forgotten = append(result.Forgotten, change)
	}
	return result, nil
}

func (m *Manager) forgetOne(ctx context.Context, name string, dryRun bool) (ForgetChange, error) {
	// Same workspace -> Git lock order as create and remove. The record and
	// Git's worktree list are both read under the locks, so a concurrent
	// create, adopt or reconcile cannot revive the workspace between the
	// check and the delete.
	lock, err := m.Native.LockWorkspace(ctx, name)
	if err != nil {
		return ForgetChange{}, err
	}
	defer lock.Release()
	gitLock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "git-admin.lock"))
	if err != nil {
		return ForgetChange{}, err
	}
	defer gitLock.Release()
	value, err := m.Registry.Load(name)
	if err != nil {
		return ForgetChange{}, err
	}
	records, err := m.Native.Journal.List()
	if err != nil {
		return ForgetChange{}, err
	}
	for _, record := range records {
		if record.Name == name {
			return ForgetChange{}, &WorktreeRemainsError{Name: name, Reason: "an interrupted operation is recorded for it; run `cambium recover` first"}
		}
	}
	worktrees, err := m.reconcileWorktrees(ctx)
	if err != nil {
		return ForgetChange{}, err
	}
	reason, err := worktreeRemains(value, worktrees)
	if err != nil {
		return ForgetChange{}, err
	}
	if reason != "" {
		return ForgetChange{}, &WorktreeRemainsError{Name: name, Reason: reason}
	}
	if !dryRun {
		if err := m.Registry.Delete(name); err != nil {
			return ForgetChange{}, err
		}
	}
	return ForgetChange{Name: value.Name, Path: value.Path}, nil
}

// worktreeRemains returns why a workspace is not gone, or "" when nothing of it
// is left: no directory at its path, no Git registration at its path, no
// worktree carrying its receipt (a moved worktree), and no worktree on its
// branch (a moved worktree from before receipts). A path on an unmounted volume
// also looks gone; its record can be restored with `cambium adopt`.
func worktreeRemains(value model.Workspace, worktrees []reconcileWorktree) (string, error) {
	if _, err := os.Lstat(value.Path); err == nil {
		return fmt.Sprintf("%s still exists; use `cambium remove`", value.Path), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect workspace path %s: %w", value.Path, err)
	}
	path := reconcilePath(value.Path)
	for _, item := range worktrees {
		switch {
		case item.Path == path:
			return fmt.Sprintf("Git still registers a worktree at %s; check it, then run `git worktree prune`", item.Path), nil
		case value.ID != "" && item.Valid && item.Owner == value.ID:
			return fmt.Sprintf("its worktree moved to %s; run `cambium reconcile`", item.Path), nil
		case value.Branch != "" && item.Branch == value.Branch:
			return fmt.Sprintf("its branch %q is checked out at %s", value.Branch, item.Path), nil
		}
	}
	return "", nil
}
