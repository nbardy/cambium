package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/model"
)

// RefusalReason names why a guarded command preserved its target. The values
// are part of the JSON contract read by orchestrators (oompa, Buddies).
type RefusalReason string

const (
	RefuseUnreadable       RefusalReason = "unreadable"
	RefuseInProgress       RefusalReason = "in-progress"
	RefuseBranchChanged    RefusalReason = "branch-changed"
	RefuseDirty            RefusalReason = "dirty"
	RefuseSalvageFailed    RefusalReason = "salvage-failed"
	RefuseUnmerged         RefusalReason = "unmerged"
	RefuseOutputConflict   RefusalReason = "output-conflict"
	RefuseOutputUnreadable RefusalReason = "output-unreadable"
	RefuseCheckedOut       RefusalReason = "checked-out"
	RefuseMissing          RefusalReason = "missing"
	RefuseTagConflict      RefusalReason = "tag-conflict"
	RefusePushUnverified   RefusalReason = "push-unverified"
	RefuseMoved            RefusalReason = "moved"
)

// Refusal is the typed "nothing was deleted" outcome. The CLI maps it to exit
// code 3 so callers can tell a deliberate preservation from a crash.
type Refusal struct {
	Reason RefusalReason
	Detail string
}

func (r *Refusal) Error() string { return fmt.Sprintf("refused (%s): %s", r.Reason, r.Detail) }

func refuse(reason RefusalReason, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

type SafeRemoveOptions struct {
	DeleteBranch  bool
	RequireMerged string // empty: no merge requirement
	HarvestDir    string // empty: no harvest
	SalvageRef    string // empty: dirty workspaces refuse
}

type HarvestSummary struct {
	Moved        int `json:"moved"`
	Deduplicated int `json:"deduplicated"`
}

type SafeRemoveResult struct {
	Removed    bool           `json:"removed"`
	SalvageRef string         `json:"salvageRef"`
	Harvested  HarvestSummary `json:"harvested"`
}

var inProgressMarkers = []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD"}

// SafeRemove removes a workspace only after a guard ladder proves nothing of
// value is lost. Read-only guards run first, in contract order, so a refusal
// never leaves a salvage ref or a half-harvest behind; side-effecting steps
// (salvage, harvest) run last, still under the workspace and Git locks.
func (m *Manager) SafeRemove(ctx context.Context, name string, options SafeRemoveOptions) (SafeRemoveResult, error) {
	result := SafeRemoveResult{Removed: true}
	repo := m.Project.Repository
	err := m.Native.RemoveIf(ctx, name, true, options.DeleteBranch, func(current model.Workspace) error {
		clean, status, err := repo.IsClean(ctx, current.Path)
		if err != nil {
			return refuse(RefuseUnreadable, "git status failed in %s: %v", current.Path, err)
		}
		if err := checkNoOperationInProgress(ctx, repo, current.Path); err != nil {
			return err
		}
		head, err := checkBranchUnchanged(ctx, repo, current)
		if err != nil {
			return err
		}
		if !clean && options.SalvageRef == "" {
			return refuse(RefuseDirty, "workspace %q has uncommitted or untracked changes:\n%s", name, status)
		}
		if options.RequireMerged != "" {
			if err := checkMerged(ctx, repo, head, options.RequireMerged); err != nil {
				return err
			}
		}
		if !clean {
			if err := salvage(ctx, repo, current.Path, head, options.SalvageRef); err != nil {
				return err
			}
			result.SalvageRef = options.SalvageRef
		}
		if options.HarvestDir != "" {
			summary, err := harvestIgnored(ctx, repo, current.Path, options.HarvestDir)
			if err != nil {
				return err
			}
			result.Harvested = summary
		}
		return nil
	})
	if err != nil {
		return SafeRemoveResult{}, err
	}
	return result, nil
}

func checkNoOperationInProgress(ctx context.Context, repo gitx.Repository, worktree string) error {
	for _, marker := range inProgressMarkers {
		path, err := repo.GitPath(ctx, worktree, marker)
		if err != nil {
			return refuse(RefuseUnreadable, "resolve %s: %v", marker, err)
		}
		if _, err := os.Lstat(path); err == nil {
			return refuse(RefuseInProgress, "%s present at %s", marker, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return refuse(RefuseUnreadable, "stat %s: %v", path, err)
		}
	}
	return nil
}

// checkBranchUnchanged returns the workspace HEAD commit. A workspace with a
// recorded branch must still have that branch checked out; a detached
// workspace (no recorded branch) is judged by its HEAD alone.
func checkBranchUnchanged(ctx context.Context, repo gitx.Repository, current model.Workspace) (string, error) {
	head, err := repo.ResolveCommitAt(ctx, current.Path, "HEAD")
	if err != nil {
		return "", refuse(RefuseUnreadable, "resolve workspace HEAD: %v", err)
	}
	if current.Branch == "" {
		return head, nil
	}
	symbolic, err := repo.RunAt(ctx, current.Path, nil, "symbolic-ref", "-q", "HEAD")
	actual := strings.TrimSpace(symbolic.Stdout)
	if err != nil || actual != "refs/heads/"+current.Branch {
		return "", refuse(RefuseBranchChanged, "workspace HEAD is %q, recorded branch is %q", actual, current.Branch)
	}
	return head, nil
}

func checkMerged(ctx context.Context, repo gitx.Repository, head, target string) error {
	targetCommit, err := repo.ResolveCommit(ctx, target)
	if err != nil {
		return err
	}
	result, err := repo.RunCommon(ctx, nil, "merge-base", "--is-ancestor", head, targetCommit)
	if err == nil {
		return nil
	}
	if result.ExitCode == 1 {
		return refuse(RefuseUnmerged, "workspace HEAD %s is not an ancestor of %s (%s)", head, target, targetCommit)
	}
	return err
}

// salvage snapshots the full visible tree (tracked + untracked, not ignored)
// as a commit on top of head at ref, using a private index exactly like
// speculative checkpoints so the workspace's own staging area is untouched.
// An existing ref is reused only if it already holds the identical tree, so a
// rerun is idempotent and an unrelated salvage is never overwritten.
func salvage(ctx context.Context, repo gitx.Repository, worktree, head, ref string) error {
	tree, err := snapshotTree(ctx, repo, worktree)
	if err != nil {
		return refuse(RefuseSalvageFailed, "snapshot tree: %v", err)
	}
	existing, err := repo.RunCommon(ctx, nil, "rev-parse", "--verify", "-q", ref+"^{tree}")
	if err == nil {
		existingTree := strings.TrimSpace(existing.Stdout)
		if existingTree == tree {
			return nil
		}
		return refuse(RefuseSalvageFailed, "%s already exists with a different tree (%s)", ref, existingTree)
	}
	commit, err := repo.RunCommon(ctx, nil, "commit-tree", tree, "-p", head, "-m", "cambium remove --safe salvage of "+worktree)
	if err != nil {
		return refuse(RefuseSalvageFailed, "commit-tree: %v", err)
	}
	// Empty old value: create only, never clobber a ref created concurrently.
	if _, err := repo.RunCommon(ctx, nil, "update-ref", ref, strings.TrimSpace(commit.Stdout), ""); err != nil {
		return refuse(RefuseSalvageFailed, "update-ref %s: %v", ref, err)
	}
	return nil
}

func snapshotTree(ctx context.Context, repo gitx.Repository, worktree string) (string, error) {
	indexPath, err := repo.GitPath(ctx, worktree, "index")
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(indexPath), "cambium-salvage-index-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return "", err
	}
	_ = os.Remove(temporaryPath) // git refuses an empty pre-existing index file
	defer os.Remove(temporaryPath)
	defer os.Remove(temporaryPath + ".lock")
	env := map[string]string{"GIT_INDEX_FILE": temporaryPath}
	if _, statErr := os.Stat(indexPath); errors.Is(statErr, os.ErrNotExist) {
		if _, err := repo.RunAt(ctx, worktree, env, "read-tree", "HEAD"); err != nil {
			return "", err
		}
	} else if err := fsx.CopyFileAtomic(indexPath, temporaryPath, 0o600); err != nil {
		return "", err
	}
	if _, err := repo.RunAt(ctx, worktree, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	result, err := repo.RunAt(ctx, worktree, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// harvestIgnored moves git-ignored files under dir in the workspace to the
// same relative path in the primary checkout before the workspace is deleted.
// Why: `git status` omits ignored files, so a workspace whose only new files
// were ignored run output looked clean and removal deleted them. Swarm
// 455987ea lost the raw output of a paid GPU run this way (2026-10-24; ported
// from oompa agentnet/cambium.clj harvest-ignored-output!). All conflicts are
// found before anything moves: a differing destination refuses the whole
// harvest. Identical destinations deduplicate (the source is deleted).
func harvestIgnored(ctx context.Context, repo gitx.Repository, worktree, dir string) (HarvestSummary, error) {
	listed, err := repo.RunAt(ctx, worktree, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--", dir)
	if err != nil {
		return HarvestSummary{}, refuse(RefuseOutputUnreadable, "list ignored files under %s: %v", dir, err)
	}
	primary := filepath.Dir(repo.CommonGitDir)
	type pair struct{ relative, from, to string }
	var moves, duplicates []pair
	var conflicts []string
	for _, relative := range strings.Split(listed.Stdout, "\x00") {
		if relative == "" {
			continue
		}
		item := pair{relative, filepath.Join(worktree, relative), filepath.Join(primary, relative)}
		same, exists, err := sameEntry(item.from, item.to)
		if err != nil {
			return HarvestSummary{}, refuse(RefuseOutputUnreadable, "compare %s: %v", relative, err)
		}
		switch {
		case !exists:
			moves = append(moves, item)
		case same:
			duplicates = append(duplicates, item)
		default:
			conflicts = append(conflicts, relative)
		}
	}
	if len(conflicts) > 0 {
		return HarvestSummary{}, refuse(RefuseOutputConflict, "primary checkout %s holds different bytes at: %s", primary, strings.Join(conflicts, ", "))
	}
	summary := HarvestSummary{}
	for _, item := range moves {
		if err := os.MkdirAll(filepath.Dir(item.to), 0o755); err != nil {
			return summary, refuse(RefuseOutputUnreadable, "create %s: %v (moved %d before failing)", filepath.Dir(item.to), err, summary.Moved)
		}
		// Rename keeps symlinks as links. Workspaces live on the primary's
		// volume (CoW), so a cross-device rename is a refusal, not a copy.
		if err := os.Rename(item.from, item.to); err != nil {
			return summary, refuse(RefuseOutputUnreadable, "move %s: %v (moved %d before failing)", item.relative, err, summary.Moved)
		}
		summary.Moved++
	}
	for _, item := range duplicates {
		if err := os.Remove(item.from); err != nil {
			return summary, refuse(RefuseOutputUnreadable, "remove duplicate %s: %v", item.relative, err)
		}
		summary.Deduplicated++
	}
	return summary, nil
}

// sameEntry compares a regular file or symlink with its destination.
func sameEntry(from, to string) (same, exists bool, err error) {
	toInfo, err := os.Lstat(to)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}
	fromInfo, err := os.Lstat(from)
	if err != nil {
		return false, true, err
	}
	if fromInfo.Mode().Type() != toInfo.Mode().Type() {
		return false, true, nil
	}
	if fromInfo.Mode()&os.ModeSymlink != 0 {
		a, errA := os.Readlink(from)
		b, errB := os.Readlink(to)
		return errA == nil && errB == nil && a == b, true, errors.Join(errA, errB)
	}
	if fromInfo.Size() != toInfo.Size() {
		return false, true, nil
	}
	a, err := os.ReadFile(from)
	if err != nil {
		return false, true, err
	}
	b, err := os.ReadFile(to)
	if err != nil {
		return false, true, err
	}
	return bytes.Equal(a, b), true, nil
}
