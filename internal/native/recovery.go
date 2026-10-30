package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/operation"
	"github.com/nbardy/cambium/internal/state"
)

const worktreeOwnerFile = "cambium-owner"

// claimWorktree records identity inside this linked worktree's Git admin dir,
// never inside tracked source. Only explicit create/remove calls may establish
// ownership; recovery must verify an existing claim instead of inventing one.
// The caller holds the workspace and common Git administration locks.
func (b *Backend) claimWorktree(ctx context.Context, path, branch, workspaceID string) (string, error) {
	if workspaceID == "" {
		return "", errors.New("workspace identity is missing")
	}
	worktree, found, err := b.registeredWorktree(ctx, path)
	if err != nil {
		return "", err
	}
	if !found || worktree.Bare || worktree.Branch != branch {
		return "", fmt.Errorf("worktree registration at %s does not match branch %q", path, branch)
	}
	marker, err := b.ownerMarker(ctx, path)
	if err != nil {
		return "", err
	}
	owner, err := os.ReadFile(marker)
	if err == nil {
		if strings.TrimSpace(string(owner)) != workspaceID {
			return "", fmt.Errorf("worktree at %s belongs to another workspace; refusing cleanup", path)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := fsx.WriteFileAtomic(marker, []byte(workspaceID+"\n"), 0o600); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	return filepath.Dir(marker), nil
}

func (b *Backend) ownerMarker(ctx context.Context, path string) (string, error) {
	canonical, err := canonicalCreationPath(path)
	if err != nil {
		return "", err
	}
	if canonical != filepath.Clean(path) {
		return "", fmt.Errorf("workspace path %s now resolves to %s; refusing cleanup", path, canonical)
	}
	return b.Project.Repository.GitPath(ctx, path, worktreeOwnerFile)
}

func (b *Backend) registeredWorktree(ctx context.Context, path string) (gitx.Worktree, bool, error) {
	worktrees, err := b.Project.Repository.ListWorktrees(ctx)
	if err != nil {
		return gitx.Worktree{}, false, err
	}
	for _, worktree := range worktrees {
		if worktree.Path == filepath.Clean(path) {
			return worktree, true, nil
		}
	}
	return gitx.Worktree{}, false, nil
}

func (b *Backend) verifyWorktreeOwner(ctx context.Context, record operation.Record, worktree gitx.Worktree) error {
	if record.WorkspaceID == "" {
		return errors.New("operation predates ownership receipts; inspect the workspace manually")
	}
	if worktree.Bare || worktree.Branch != record.Branch {
		return fmt.Errorf("worktree registration belongs to branch %q, not recorded branch %q", worktree.Branch, record.Branch)
	}
	marker, err := b.ownerMarker(ctx, record.Path)
	if err != nil {
		return err
	}
	if record.WorktreeDir != "" && filepath.Dir(marker) != record.WorktreeDir {
		return errors.New("worktree Git administration directory changed; ownership is uncertain")
	}
	owner, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("cannot verify worktree ownership: %w", err)
	}
	if strings.TrimSpace(string(owner)) != record.WorkspaceID {
		return errors.New("worktree ownership changed; refusing to remove a replacement workspace")
	}
	return nil
}

func (b *Backend) recoverOperation(ctx context.Context, snapshot operation.Record, dryRun bool) (RecoveryAction, bool, error) {
	action := recoveryAction(snapshot)
	workspaceLock, err := lockfile.Acquire(filepath.Join(b.Project.LocksDir(), "workspace-"+slug(snapshot.Name)+".lock"))
	if errors.Is(err, lockfile.ErrBusy) {
		action.Action, action.Status = "skip", "active"
		action.Message = "workspace operation is still active; left untouched"
		return action, true, nil
	}
	if err != nil {
		return action, false, err
	}
	defer workspaceLock.Release()
	gitLock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
	if err != nil {
		return action, false, err
	}
	defer gitLock.Release()
	// A creator/remover may have finished after List returned, or another
	// recovery process may already have handled this operation.
	record, err := b.Journal.Load(snapshot.ID)
	if errors.Is(err, os.ErrNotExist) {
		return action, false, nil
	}
	if err != nil {
		return action, false, err
	}
	if record.Name != snapshot.Name || record.Path != snapshot.Path || record.Branch != snapshot.Branch || record.Kind != snapshot.Kind {
		return protected(action, "operation identity changed while acquiring its locks"), true, nil
	}
	switch record.Kind {
	case operation.KindCreate:
		action, err = b.recoverCreate(ctx, record, dryRun)
	case operation.KindRemove:
		action, err = b.recoverRemove(ctx, record, dryRun)
	default:
		action = protected(action, fmt.Sprintf("unknown operation kind %q", record.Kind))
	}
	return action, true, err
}

func recoveryAction(record operation.Record) RecoveryAction {
	return RecoveryAction{OperationID: record.ID, Kind: string(record.Kind), Name: record.Name}
}

func protected(action RecoveryAction, reason string) RecoveryAction {
	action.Action, action.Status, action.Message = "protect", "blocked", reason
	return action
}

func sameWorkspace(workspace model.Workspace, record operation.Record) bool {
	return record.WorkspaceID != "" && workspace.ID == record.WorkspaceID &&
		workspace.Path == record.Path && workspace.Branch == record.Branch
}

// recoverCreate is also used for ordinary creation rollback. The caller holds
// both locks. An operation's planned path is never sufficient deletion proof.
func (b *Backend) recoverCreate(ctx context.Context, record operation.Record, dryRun bool) (RecoveryAction, error) {
	action := recoveryAction(record)
	workspace, err := b.Registry.Load(record.Name)
	if err == nil {
		if !sameWorkspace(workspace, record) {
			return protected(action, "workspace metadata belongs to a different operation; left untouched"), nil
		}
		action.Action, action.Status = "finalize", "resolved"
		action.Message = "matching workspace metadata exists; removed stale operation record"
		if !dryRun {
			err = b.Journal.Delete(record.ID)
		}
		return action, err
	}
	if !errors.Is(err, state.ErrWorkspaceNotFound) {
		return action, err
	}
	branchExists, reason, err := b.checkRecoveryBranch(ctx, record)
	if err != nil {
		return action, err
	}
	if reason != "" {
		return protected(action, reason), nil
	}
	worktree, registered, err := b.registeredWorktree(ctx, record.Path)
	if err != nil {
		return action, err
	}
	if registered {
		if err := b.verifyWorktreeOwner(ctx, record, worktree); err != nil {
			return protected(action, err.Error()), nil
		}
		if reason := b.rollbackContentRisk(ctx, record.Path); reason != "" {
			return protected(action, reason), nil
		}
		if !dryRun {
			if err := b.Project.Repository.WorktreeRemove(ctx, record.Path, true); err != nil {
				return action, fmt.Errorf("recover worktree %s: %w", record.Path, err)
			}
		}
	} else {
		if _, err := os.Lstat(record.Path); err == nil {
			return protected(action, "path exists without the owned Git registration; refusing to remove it"), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return action, err
		}
		if branchExists && (record.WorkspaceID == "" || record.WorktreeDir == "") {
			return protected(action, "branch exists but creation never recorded worktree ownership; inspect it manually"), nil
		}
	}
	if !dryRun {
		if branchExists {
			if err := b.Project.Repository.DeleteBranchIfUnchanged(ctx, record.Branch, record.BaseCommit); err != nil {
				return protected(action, "workspace removed, but branch was retained: "+err.Error()), nil
			}
		}
		if err := b.Journal.Delete(record.ID); err != nil {
			return action, err
		}
	}
	action.Action, action.Status = "rollback", "resolved"
	action.Message = "removed owned incomplete worktree and unadvanced branch"
	return action, nil
}

func (b *Backend) checkRecoveryBranch(ctx context.Context, record operation.Record) (bool, string, error) {
	exists, err := b.Project.Repository.BranchExists(ctx, record.Branch)
	if err != nil || !exists {
		return exists, "", err
	}
	commit, err := b.Project.Repository.BranchCommit(ctx, record.Branch)
	if err != nil {
		return true, "", err
	}
	if commit != record.BaseCommit {
		return true, fmt.Sprintf("branch advanced to %s; refusing automatic deletion", commit), nil
	}
	return true, "", nil
}

func (b *Backend) rollbackContentRisk(ctx context.Context, path string) string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Sprintf("cannot inspect incomplete workspace: %v", err)
	}
	if len(entries) == 1 && entries[0].Name() == ".git" {
		// An interrupted --no-checkout registration has no source content to
		// lose, even though its absent index makes Git report staged deletions.
		return ""
	}
	clean, _, err := b.Project.Repository.IsClean(ctx, path)
	if err != nil {
		return fmt.Sprintf("cannot verify incomplete workspace is clean: %v", err)
	}
	if !clean {
		return "incomplete workspace contains uncommitted or untracked changes; left for inspection"
	}
	// Ignore rules do not make local data disposable. In particular, a .env
	// created after an interruption must not disappear during automatic repair.
	others, err := b.Project.Repository.RunAt(ctx, path, nil, "ls-files", "--others", "-z")
	if err != nil {
		return fmt.Sprintf("cannot inspect untracked workspace data: %v", err)
	}
	if others.Stdout != "" {
		return "incomplete workspace contains ignored or untracked data; left for inspection"
	}
	return ""
}

func (b *Backend) recoverRemove(ctx context.Context, record operation.Record, dryRun bool) (RecoveryAction, error) {
	action := recoveryAction(record)
	workspace, err := b.Registry.Load(record.Name)
	if err == nil && !sameWorkspace(workspace, record) {
		return protected(action, "workspace metadata was replaced; refusing to finish the old removal"), nil
	}
	if err != nil && !errors.Is(err, state.ErrWorkspaceNotFound) {
		return action, err
	}
	if record.WorkspaceID == "" || record.WorktreeDir == "" {
		return protected(action, "removal has no worktree ownership receipt; inspect it manually"), nil
	}
	branchExists, reason, err := b.checkRecoveryBranch(ctx, record)
	if err != nil {
		return action, err
	}
	if reason != "" {
		return protected(action, reason), nil
	}
	worktree, registered, err := b.registeredWorktree(ctx, record.Path)
	if err != nil {
		return action, err
	}
	if registered {
		if err := b.verifyWorktreeOwner(ctx, record, worktree); err != nil {
			return protected(action, err.Error()), nil
		}
		// A failed removal may have been followed by new edits. Recovery never
		// replays --force onto that new work; the user can remove it explicitly.
		if reason := b.rollbackContentRisk(ctx, record.Path); reason != "" {
			return protected(action, reason), nil
		}
		if !dryRun {
			if err := b.Project.Repository.WorktreeRemove(ctx, record.Path, record.Force); err != nil {
				return protected(action, err.Error()), nil
			}
		}
	} else if _, err := os.Lstat(record.Path); err == nil {
		return protected(action, "removed worktree path has been reused; refusing to touch it"), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return action, err
	}
	if !dryRun {
		if err := b.Registry.Delete(record.Name); err != nil {
			return action, err
		}
		if record.DeleteBranch && branchExists {
			if err := b.Project.Repository.DeleteBranchIfUnchanged(ctx, record.Branch, record.BaseCommit); err != nil {
				return protected(action, "workspace removed, but branch was retained: "+err.Error()), nil
			}
		}
		if err := b.Journal.Delete(record.ID); err != nil {
			return action, err
		}
	}
	action.Action, action.Status = "finish-remove", "resolved"
	action.Message = "completed owned interrupted removal"
	return action, nil
}
