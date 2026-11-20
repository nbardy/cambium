package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/inuse"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

func holderList(holders []inuse.Holder) string {
	parts := make([]string, len(holders))
	for i, holder := range holders {
		parts[i] = holder.String()
	}
	return strings.Join(parts, "; ")
}

// RefusalReason names why a guarded command preserved its target. The values
// are part of the JSON contract read by orchestrators (oompa, Buddies).
type RefusalReason string

const (
	RefuseUnreadable        RefusalReason = "unreadable"
	RefuseInProgress        RefusalReason = "in-progress"
	RefuseBranchChanged     RefusalReason = "branch-changed"
	RefuseDirty             RefusalReason = "dirty"
	RefuseSalvageFailed     RefusalReason = "salvage-failed"
	RefuseUnmerged          RefusalReason = "unmerged"
	RefuseOutputConflict    RefusalReason = "output-conflict"
	RefuseOutputUnreadable  RefusalReason = "output-unreadable"
	RefuseCheckedOut        RefusalReason = "checked-out"
	RefuseMissing           RefusalReason = "missing"
	RefuseTagConflict       RefusalReason = "tag-conflict"
	RefusePushUnverified    RefusalReason = "push-unverified"
	RefuseMoved             RefusalReason = "moved"
	RefuseInUse             RefusalReason = "in-use"
	RefuseInUseUnknown      RefusalReason = "in-use-unknown"
	RefusePrimary           RefusalReason = "primary"
	RefuseSubmoduleUnpushed RefusalReason = "submodule-unpushed"
	RefuseUnpushed          RefusalReason = "unpushed"
	RefuseProtected         RefusalReason = "protected"
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
	// HarvestTo, with HarvestDir, moves the ignored files to HarvestTo/<relative path>
	// instead of into the primary checkout, and leaves behind what a rescue
	// copy does not need: regenerable dependency directories and Cargo
	// compiler state. HarvestDir "." means the whole workspace.
	HarvestTo  string
	SalvageRef string // empty: dirty workspaces and unpushed commits refuse
	// DryRun runs every read-only guard and reports the same refusal the real
	// removal would, but changes nothing (no checkpoint, salvage or harvest).
	DryRun bool
	// Recheck runs first under the workspace lock; the caller uses it to
	// re-verify identity and eligibility on the record actually removed.
	Recheck func(model.Workspace) error
}

type HarvestSummary struct {
	Moved        int `json:"moved"`
	Deduplicated int `json:"deduplicated"`
}

type SafeRemoveResult struct {
	Removed    bool           `json:"removed"`
	SalvageRef string         `json:"salvageRef"`
	Harvested  HarvestSummary `json:"harvested"`
	// Checkpoint is the hidden ref taken before removal ("" when there was
	// nothing to save); `cambium recover --removed` restores from it.
	Checkpoint string `json:"checkpoint,omitempty"`
}

var inProgressMarkers = []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD"}

// SafeRemove removes a workspace only after a guard ladder proves nothing of
// value is lost. Read-only guards run first, in contract order, so a refusal
// never leaves a salvage ref or a half-harvest behind; side-effecting steps
// (salvage, harvest) run last, still under the workspace and Git locks.
func (m *Manager) SafeRemove(ctx context.Context, name string, options SafeRemoveOptions) (SafeRemoveResult, error) {
	if options.DryRun {
		return m.checkRemovable(ctx, name, options)
	}
	checkpoint, err := m.SaveBeforeRemove(ctx, name)
	if err != nil {
		return SafeRemoveResult{}, err
	}
	result, err := m.safeRemove(ctx, name, options)
	if err != nil {
		// A refusal deletes nothing, so the checkpoint would only hold cache bytes.
		m.DiscardIfSurvived(ctx, name, checkpoint)
		return result, err
	}
	result.Checkpoint = checkpoint
	return result, nil
}

// removalPlan is what the read-only guards learned about a workspace.
type removalPlan struct {
	head     string
	clean    bool
	status   string
	unpushed int
	salvage  bool // a salvage ref will be written: dirty tree and/or unpushed commits
}

// guardRemoval is the guard ladder shared by the real removal and its dry run,
// so `prune --merged --dry-run` can never promise what the real run refuses.
// It changes nothing.
func (m *Manager) guardRemoval(ctx context.Context, name string, current model.Workspace, options SafeRemoveOptions) (removalPlan, error) {
	var plan removalPlan
	repo := m.Project.Repository
	if options.Recheck != nil {
		if err := options.Recheck(current); err != nil {
			return plan, err
		}
	}
	if native.SamePath(current.Path, m.Project.PrimaryRoot()) {
		return plan, refuse(RefusePrimary, "%s is the primary checkout, not a removable workspace", current.Path)
	}
	// Guarded removal has no force: a workspace in use is never removed.
	holders, err := m.InUse(current)
	if err != nil {
		return plan, refuse(RefuseInUseUnknown, "%s: %v", current.Path, err)
	}
	if len(holders) > 0 {
		return plan, refuse(RefuseInUse, "workspace %q is in use: %s", name, holderList(holders))
	}
	plan.clean, plan.status, err = repo.IsClean(ctx, current.Path)
	if err != nil {
		return plan, refuse(RefuseUnreadable, "git status failed in %s: %v", current.Path, err)
	}
	if err := checkNoOperationInProgress(ctx, repo, current.Path); err != nil {
		return plan, err
	}
	plan.head, err = checkBranchUnchanged(ctx, repo, current)
	if err != nil {
		return plan, err
	}
	if err := m.checkSubmodules(ctx, current.Path); err != nil {
		return plan, err
	}
	if !plan.clean && options.SalvageRef == "" {
		return plan, refuse(RefuseDirty, "workspace %q has uncommitted or untracked changes:\n%s", name, plan.status)
	}
	merged := false
	if options.RequireMerged != "" {
		if err := checkMerged(ctx, repo, plan.head, options.RequireMerged); err != nil {
			return plan, err
		}
		merged = true
	}
	// A commit already in REQUIRE-MERGED is held by that ref; any other commit
	// that no remote and no other branch holds is lost with the workspace's
	// branch (or stranded on it), so removal refuses or salvages it.
	if !merged {
		plan.unpushed, err = unpushedCommits(ctx, repo, current.Path, current.Branch)
		if err != nil {
			return plan, refuse(RefuseUnreadable, "count unpushed commits in %s: %v", current.Path, err)
		}
		if plan.unpushed > 0 && options.SalvageRef == "" {
			return plan, refuse(RefuseUnpushed, "workspace %q has %d unpushed commit(s) (HEAD %s) that no remote or other branch holds: push or merge them, or pass --salvage-ref refs/... to keep them", name, plan.unpushed, shortSHA(plan.head))
		}
	}
	plan.salvage = !plan.clean || plan.unpushed > 0
	return plan, nil
}

// checkRemovable is the dry run: the guards, then "would remove".
func (m *Manager) checkRemovable(ctx context.Context, name string, options SafeRemoveOptions) (SafeRemoveResult, error) {
	current, err := m.Registry.Load(name)
	if err != nil {
		return SafeRemoveResult{}, err
	}
	plan, err := m.guardRemoval(ctx, name, current, options)
	if err != nil {
		return SafeRemoveResult{}, err
	}
	result := SafeRemoveResult{Removed: false}
	if plan.salvage {
		result.SalvageRef = options.SalvageRef
	}
	return result, nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (m *Manager) safeRemove(ctx context.Context, name string, options SafeRemoveOptions) (SafeRemoveResult, error) {
	result := SafeRemoveResult{Removed: true}
	repo := m.Project.Repository
	err := m.Native.RemoveIf(ctx, name, true, options.DeleteBranch, func(current model.Workspace) error {
		plan, err := m.guardRemoval(ctx, name, current, options)
		if err != nil {
			return err
		}
		// Side effects last, after every guard has passed.
		if plan.salvage {
			if err := salvage(ctx, repo, current.Path, plan.head, options.SalvageRef, plan.clean); err != nil {
				return err
			}
			result.SalvageRef = options.SalvageRef
		}
		if options.HarvestDir != "" {
			summary, err := m.harvest(ctx, current.Path, options)
			if err != nil {
				return err
			}
			result.Harvested = summary
		}
		// Last, after every guard: drop the submodule copies the checks just proved empty of unique work.
		return m.dropSubmodules(ctx, current.Path)
	})
	if err != nil {
		return SafeRemoveResult{}, err
	}
	return result, nil
}

// unpushedCommits counts commits reachable from the workspace HEAD that no
// remote-tracking ref and no other local branch or tag holds: the work that
// removing this workspace (and, with --delete-branch, its branch) would strand
// or lose. A repository with no remote is judged by the other-ref test alone.
func unpushedCommits(ctx context.Context, repo gitx.Repository, worktree, branch string) (int, error) {
	args := []string{"rev-list", "--count", "HEAD", "--not"}
	if branch != "" {
		// Git matches --exclude relative to the namespace the next option walks
		// (--branches), so the pattern is the bare branch name, not refs/heads/NAME.
		// A full ref name matches nothing and the workspace's own branch would
		// count as "another branch holding the commits".
		args = append(args, "--exclude="+branch)
	}
	args = append(args, "--branches", "--tags", "--remotes")
	result, err := repo.RunAt(ctx, worktree, nil, args...)
	if err != nil {
		return 0, err
	}
	return atoi(result.Stdout), nil
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
	if actual == "" {
		// Detached HEAD (a review tree pinned to a commit): no other branch took the
		// workspace over, and the unpushed-commit guard judges the commit HEAD names.
		// Branch deletion stays safe because it only deletes a branch still at its base.
		return head, nil
	}
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
func salvage(ctx context.Context, repo gitx.Repository, worktree, head, ref string, clean bool) error {
	if clean {
		// Only unpushed commits to keep: the ref is the HEAD commit itself.
		existing, err := repo.RunCommon(ctx, nil, "rev-parse", "--verify", "-q", ref+"^{commit}")
		if err == nil {
			if strings.TrimSpace(existing.Stdout) == head {
				return nil
			}
			return refuse(RefuseSalvageFailed, "%s already exists at a different commit (%s)", ref, strings.TrimSpace(existing.Stdout))
		}
		if _, err := repo.RunCommon(ctx, nil, "update-ref", ref, head, ""); err != nil {
			return refuse(RefuseSalvageFailed, "update-ref %s: %v", ref, err)
		}
		return nil
	}
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
	commit, err := repo.RunCommon(ctx, nil, "commit-tree", tree, "-p", head, "-m", "cambium remove salvage of "+worktree)
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
func harvestIgnored(ctx context.Context, repo gitx.Repository, worktree, dir, dest string, skip func(string) bool) (HarvestSummary, error) {
	listed, err := repo.RunAt(ctx, worktree, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--", dir)
	if err != nil {
		return HarvestSummary{}, refuse(RefuseOutputUnreadable, "list ignored files under %s: %v", dir, err)
	}
	primary := filepath.Dir(repo.CommonGitDir)
	if dest != "" {
		primary = dest
	}
	type pair struct{ relative, from, to string }
	var moves, duplicates []pair
	var conflicts []string
	for _, relative := range strings.Split(listed.Stdout, "\x00") {
		if relative == "" || (skip != nil && skip(relative)) {
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

// harvest runs harvestIgnored for one removal. Into the primary checkout the
// rule is exact: every ignored file under DIR. Into a rescue directory
// (HarvestTo) what a rescue copy would only carry as dead weight stays behind.
func (m *Manager) harvest(ctx context.Context, worktree string, options SafeRemoveOptions) (HarvestSummary, error) {
	dir, dest := options.HarvestDir, options.HarvestTo
	if dest == "" {
		return harvestIgnored(ctx, m.Project.Repository, worktree, dir, "", nil)
	}
	if rel, err := filepath.Rel(worktree, dest); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return HarvestSummary{}, refuse(RefuseOutputUnreadable, "--harvest-to %s is inside the workspace being removed", dest)
	}
	var managed []string // layer outputs Cambium can rebuild from a baseline
	for _, rule := range m.Project.Rules {
		switch rule.Mode {
		case config.LayerClone, config.LayerRecreate, config.LayerShare:
			managed = append(managed, filepath.ToSlash(rule.Path))
		}
	}
	return harvestIgnored(ctx, m.Project.Repository, worktree, dir, dest, func(rel string) bool {
		return notWorthRescuing(rel, managed)
	})
}

// regenerable names directories that any package manager or tool recreates.
var regenerable = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, ".next": true, ".turbo": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	".gradle": true, ".vite": true, ".vinext": true, "coverage": true, ".cache": true,
	".pnpm-store": true, ".DS_Store": true,
}

// notWorthRescuing reports whether an ignored file is rebuildable rather than
// work: a regenerable dependency directory, a Cambium-managed layer, a .pyc, or
// Cargo compiler state inside a target directory (incremental caches, build
// fingerprints, build-script output and hashed intermediates in deps/).
// Binaries, dist, build and local databases are kept.
func notWorthRescuing(rel string, managed []string) bool {
	rel = filepath.ToSlash(rel)
	for _, layer := range managed {
		if rel == layer || strings.HasPrefix(rel, layer+"/") {
			return true
		}
	}
	if strings.HasSuffix(rel, ".pyc") {
		return true
	}
	parts := strings.Split(rel, "/")
	inTarget := false
	for i, part := range parts {
		if regenerable[part] {
			return true
		}
		if part == "target" {
			inTarget = true
			continue
		}
		if !inTarget {
			continue
		}
		switch part {
		case "incremental", ".fingerprint", "build":
			return true
		case "deps":
			for _, ext := range []string{".rlib", ".rmeta", ".o", ".d"} {
				if strings.HasSuffix(parts[len(parts)-1], ext) && i < len(parts)-1 {
					return true
				}
			}
		}
	}
	return false
}
