package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/operation"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/state"
)

type CreateSpec struct {
	Name          string
	Ref           string
	Branch        string
	Path          string
	Ephemeral     bool
	RequireCoW    bool
	Materializer  model.Materializer
	PreparedIndex *bool
}

type PrepareResult struct {
	Commit   string              `json:"commit"`
	Plan     MaterializationPlan `json:"plan"`
	Baseline *Baseline           `json:"baseline,omitempty"`
	Layers   []LayerSnapshot     `json:"layers"`
}

type RecoveryAction struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
}

type Backend struct {
	Project  *project.Project
	Registry state.Registry
	Cloner   Cloner
	Journal  operation.Journal
}

func New(project *project.Project) *Backend {
	return &Backend{
		Project:  project,
		Registry: state.New(project.MetadataDir()),
		Cloner:   Cloner{Runner: project.Runner},
		Journal:  operation.New(project.OperationsDir()),
	}
}

// LockWorkspace serializes lifecycle operations on the same workspace name.
// Composition paths such as adoption must use the same lock as create/remove.
func (b *Backend) LockWorkspace(ctx context.Context, name string) (*lockfile.Lock, error) {
	return lockfile.AcquireContext(ctx, b.workspaceLockPath(name))
}

func (b *Backend) Prepare(ctx context.Context, ref string, requested model.Materializer, requireCoW bool) (PrepareResult, error) {
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "cache-admin.lock"))
	if err != nil {
		return PrepareResult{}, err
	}
	defer lock.Release()
	return b.prepareAt(ctx, ref, requested, requireCoW, b.Project.WorktreesDir())
}

// prepareAt resolves materialization against the filesystem that will actually
// hold the workspace. Custom workspace paths may be on a different volume than
// Cambium's default state directory, so probing only the default location can
// produce a false CoW plan followed by a late clone failure.
func (b *Backend) prepareAt(ctx context.Context, ref string, requested model.Materializer, requireCoW bool, destinationParent string) (PrepareResult, error) {
	commit, err := b.Project.Repository.ResolveCommit(ctx, ref)
	if err != nil {
		return PrepareResult{}, err
	}
	if requested == "" {
		requested = b.Project.Config.Materializer
	}
	plan, err := b.Cloner.Plan(ctx, requested, b.Project.BaselinesDir(), destinationParent, requireCoW || b.Project.Config.RequireCoW)
	if err != nil {
		return PrepareResult{}, err
	}
	var baseline *Baseline
	if plan.UseBaseline {
		value, err := EnsureBaseline(ctx, b.Project, b.Cloner, commit)
		if err != nil {
			return PrepareResult{}, err
		}
		baseline = &value
	}
	layers, err := PrepareLayers(ctx, b.Project, commit, b.Cloner, requireCoW || b.Project.Config.RequireCoW)
	if err != nil {
		return PrepareResult{}, err
	}
	return PrepareResult{Commit: commit, Plan: plan, Baseline: baseline, Layers: layers}, nil
}

func (b *Backend) Create(ctx context.Context, spec CreateSpec) (workspace model.Workspace, err error) {
	if err := validateWorkspaceName(spec.Name); err != nil {
		return model.Workspace{}, err
	}
	if spec.Ref == "" {
		spec.Ref = "HEAD"
	}
	if spec.Materializer == "" {
		spec.Materializer = b.Project.Config.Materializer
	}
	if spec.Branch == "" {
		spec.Branch = b.Project.Config.BranchPrefix + slug(spec.Name)
	}
	if err := b.Project.Repository.ValidateBranch(ctx, spec.Branch); err != nil {
		return model.Workspace{}, err
	}
	exists, err := b.Registry.Exists(spec.Name)
	if err != nil {
		return model.Workspace{}, err
	}
	if exists {
		return model.Workspace{}, fmt.Errorf("workspace %q already exists", spec.Name)
	}
	branchExists, err := b.Project.Repository.BranchExists(ctx, spec.Branch)
	if err != nil {
		return model.Workspace{}, err
	}
	if branchExists {
		return model.Workspace{}, fmt.Errorf("branch %q already exists", spec.Branch)
	}
	if spec.Path == "" {
		spec.Path = filepath.Join(b.Project.WorktreesDir(), slug(spec.Name))
	}
	spec.Path, err = canonicalCreationPath(spec.Path)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("resolve workspace path: %w", err)
	}
	repositoryRoot, err := canonicalCreationPath(b.Project.Repository.Root)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("resolve repository root: %w", err)
	}
	managedRoot, err := canonicalCreationPath(b.Project.WorktreesDir())
	if err != nil {
		return model.Workspace{}, fmt.Errorf("resolve managed-worktree root: %w", err)
	}
	if pathWithin(spec.Path, repositoryRoot) && !pathWithin(spec.Path, managedRoot) {
		return model.Workspace{}, fmt.Errorf("workspace path %s is inside the primary working tree; choose a path outside %s", spec.Path, repositoryRoot)
	}
	if _, err := os.Stat(spec.Path); err == nil {
		return model.Workspace{}, fmt.Errorf("workspace path already exists: %s", spec.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return model.Workspace{}, err
	}
	lock, err := b.LockWorkspace(ctx, spec.Name)
	if err != nil {
		return model.Workspace{}, err
	}
	defer lock.Release()

	if err := os.MkdirAll(filepath.Dir(spec.Path), 0o755); err != nil {
		return model.Workspace{}, err
	}
	cacheLock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "cache-admin.lock"))
	if err != nil {
		return model.Workspace{}, err
	}
	prepared, prepareErr := b.prepareAt(ctx, spec.Ref, spec.Materializer, spec.RequireCoW, filepath.Dir(spec.Path))
	if prepareErr != nil {
		_ = cacheLock.Release()
		return model.Workspace{}, prepareErr
	}
	workspaceID, idErr := state.NewID()
	if idErr != nil {
		_ = cacheLock.Release()
		return model.Workspace{}, idErr
	}
	record, beginErr := b.Journal.Begin(operation.KindCreate, spec.Name, spec.Branch, spec.Path, prepared.Commit)
	if beginErr != nil {
		_ = cacheLock.Release()
		return model.Workspace{}, beginErr
	}
	record.WorkspaceID = workspaceID
	for _, layer := range prepared.Layers {
		record.LayerIDs = append(record.LayerIDs, layer.ID)
	}
	if saveErr := b.Journal.Save(record); saveErr != nil {
		_ = cacheLock.Release()
		_ = b.Journal.Delete(record.ID)
		return model.Workspace{}, saveErr
	}
	if releaseErr := cacheLock.Release(); releaseErr != nil {
		_ = b.Journal.Delete(record.ID)
		return model.Workspace{}, releaseErr
	}
	registrationAttempted := false
	defer func() {
		if err == nil || errors.Is(err, failpoint.ErrInjectedCrash) {
			return
		}
		if !registrationAttempted {
			// Rejected shared invariants cannot have created a worktree or
			// branch. Drop only our journal, without inspecting a winner's data.
			_ = b.Journal.Delete(record.ID)
			return
		}
		// Registration and refs live in the common Git directory. Serialize the
		// rollback just like creation so a failed concurrent create cannot damage
		// another agent's successful workspace.
		cleanupLock, cleanupErr := lockfile.AcquireContext(context.Background(), filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback retained operation %s: %w", record.ID, cleanupErr))
			return
		}
		defer cleanupLock.Release()
		action, cleanupErr := b.recoverCreate(context.Background(), record, false)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("rollback retained operation %s: %w", record.ID, cleanupErr))
		} else if action.Status == "blocked" {
			err = errors.Join(err, fmt.Errorf("rollback retained operation %s for inspection: %s", record.ID, action.Message))
		}
	}()

	var cloneMode model.CloneMode
	preparedIndex := false
	gitLock, lockErr := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
	if lockErr != nil {
		return model.Workspace{}, lockErr
	}
	// Recheck all shared invariants while holding the common Git lock. The
	// earlier checks provide fast errors; these checks close races between
	// concurrent processes using the same name, branch, or destination path.
	if exists, checkErr := b.Registry.Exists(spec.Name); checkErr != nil {
		_ = gitLock.Release()
		return model.Workspace{}, checkErr
	} else if exists {
		_ = gitLock.Release()
		return model.Workspace{}, fmt.Errorf("workspace %q already exists", spec.Name)
	}
	if exists, checkErr := b.Project.Repository.BranchExists(ctx, spec.Branch); checkErr != nil {
		_ = gitLock.Release()
		return model.Workspace{}, checkErr
	} else if exists {
		_ = gitLock.Release()
		return model.Workspace{}, fmt.Errorf("branch %q already exists", spec.Branch)
	}
	if _, checkErr := os.Lstat(spec.Path); checkErr == nil {
		_ = gitLock.Release()
		return model.Workspace{}, fmt.Errorf("workspace path already exists: %s", spec.Path)
	} else if !errors.Is(checkErr, os.ErrNotExist) {
		_ = gitLock.Release()
		return model.Workspace{}, checkErr
	}
	registrationAttempted = true
	if prepared.Plan.Resolved == model.MaterializerGit {
		err = b.Project.Repository.WorktreeAddCheckout(ctx, spec.Branch, spec.Path, prepared.Commit)
		cloneMode = model.CloneModeGitCheckout
	} else {
		if prepared.Baseline == nil {
			_ = gitLock.Release()
			return model.Workspace{}, errors.New("materialization plan requires a baseline")
		}
		err = b.Project.Repository.WorktreeAddNoCheckout(ctx, spec.Branch, spec.Path, prepared.Commit)
	}
	if err == nil {
		// The marker proves this operation owns this particular registration;
		// a planned journal entry alone never authorizes removing a path.
		record.WorktreeDir, err = b.claimWorktree(ctx, record.Path, record.Branch, record.WorkspaceID)
	}
	if err == nil {
		err = b.Journal.Advance(&record, operation.StageWorktreeRegistered)
	}
	releaseErr := gitLock.Release()
	if err != nil {
		return model.Workspace{}, fmt.Errorf("register linked worktree: %w", err)
	}
	if releaseErr != nil {
		return model.Workspace{}, releaseErr
	}
	if err = failpoint.Check("after-register"); err != nil {
		return model.Workspace{}, err
	}

	if prepared.Plan.Resolved != model.MaterializerGit {
		cloneMode, err = b.Cloner.CloneTree(ctx, prepared.Baseline.Tree, spec.Path, prepared.Plan.Resolved)
		if err != nil {
			return model.Workspace{}, fmt.Errorf("materialize worktree: %w", err)
		}
		usePrepared := b.Project.Config.PreparedIndex
		if spec.PreparedIndex != nil {
			usePrepared = *spec.PreparedIndex
		}
		if usePrepared {
			if err = InstallPreparedIndex(ctx, b.Project, *prepared.Baseline, spec.Path); err != nil {
				return model.Workspace{}, err
			}
			preparedIndex = true
		} else if err = b.Project.Repository.ReadTree(ctx, spec.Path); err != nil {
			return model.Workspace{}, fmt.Errorf("initialize worktree index: %w", err)
		}
	}
	if err = b.Journal.Advance(&record, operation.StageMaterialized); err != nil {
		return model.Workspace{}, err
	}
	if err = failpoint.Check("after-materialize"); err != nil {
		return model.Workspace{}, err
	}

	clean, status, err := b.Project.Repository.IsTrackedClean(ctx, spec.Path)
	if err != nil {
		return model.Workspace{}, err
	}
	if !clean {
		return model.Workspace{}, fmt.Errorf("materialized worktree is not clean:\n%s", status)
	}
	layerResult, err := ApplyLayers(ctx, b.Project, spec.Path, prepared.Layers, b.Cloner, spec.RequireCoW || b.Project.Config.RequireCoW)
	if err != nil {
		return model.Workspace{}, err
	}
	if err = b.Journal.Advance(&record, operation.StageLayersApplied); err != nil {
		return model.Workspace{}, err
	}
	if err = failpoint.Check("after-layers"); err != nil {
		return model.Workspace{}, err
	}

	now := time.Now().UTC()
	workspace = model.Workspace{
		Version:               2,
		ID:                    workspaceID,
		Name:                  spec.Name,
		Status:                model.WorkspaceReady,
		RepositoryRoot:        b.Project.Repository.Root,
		CommonGitDir:          b.Project.Repository.CommonGitDir,
		WorktreeGitDir:        record.WorktreeDir,
		BaseCommit:            prepared.Commit,
		BaseRef:               spec.Ref,
		Branch:                spec.Branch,
		Path:                  spec.Path,
		Requested:             spec.Materializer,
		CloneMode:             cloneMode,
		PreparedIndex:         preparedIndex,
		Ephemeral:             spec.Ephemeral,
		CreatedAt:             now,
		LastUsedAt:            now,
		LayerIDs:              layerResult.IDs,
		EnvironmentReady:      layerResult.Ready,
		EnvironmentMissing:    layerResult.Missing,
		EnvironmentUnverified: layerResult.Unverified,
	}
	if err = b.Registry.Save(workspace); err != nil {
		return model.Workspace{}, err
	}
	if err = b.Journal.Advance(&record, operation.StageMetadataSaved); err != nil {
		return model.Workspace{}, err
	}
	if err = failpoint.Check("after-metadata"); err != nil {
		return model.Workspace{}, err
	}
	if err = b.Journal.Delete(record.ID); err != nil {
		return model.Workspace{}, err
	}
	return workspace, nil
}

func (b *Backend) Remove(ctx context.Context, name string, force, deleteBranch bool) error {
	return b.RemoveIf(ctx, name, force, deleteBranch, nil)
}

// RemoveIf rechecks a caller's selection against fresh metadata while holding
// the workspace and Git administration locks. The guard may read Git, but must
// not acquire workspace, Git administration, or cache locks itself.
func (b *Backend) RemoveIf(ctx context.Context, name string, force, deleteBranch bool, guard func(model.Workspace) error) error {
	return b.remove(ctx, name, force, deleteBranch, guard, "")
}

// RemoveCreated rolls back only the newly created workspace instance. It
// preserves an advanced branch, including an update racing the final deletion.
func (b *Backend) RemoveCreated(ctx context.Context, created model.Workspace) error {
	if created.ID == "" || created.BaseCommit == "" {
		return errors.New("created workspace identity and base commit are required for cleanup")
	}
	return b.remove(ctx, created.Name, true, true, func(current model.Workspace) error {
		if current.ID != created.ID {
			return fmt.Errorf("workspace %q was replaced; refusing fork cleanup", created.Name)
		}
		head, err := b.Project.Repository.BranchCommit(ctx, current.Branch)
		if err != nil {
			return err
		}
		if head != created.BaseCommit {
			return fmt.Errorf("workspace %q branch advanced; leaving its worktree and metadata intact", created.Name)
		}
		return nil
	}, created.BaseCommit)
}

func (b *Backend) remove(ctx context.Context, name string, force, deleteBranch bool, guard func(model.Workspace) error, expectedBranchTip string) (err error) {
	lock, err := b.LockWorkspace(ctx, name)
	if err != nil {
		return err
	}
	defer lock.Release()
	// Metadata can change while waiting for the workspace lock. Never remove
	// a replacement workspace based on a snapshot read before taking it.
	workspace, err := b.Registry.Load(name)
	if err != nil {
		return err
	}
	gitLock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
	if err != nil {
		return err
	}
	defer gitLock.Release()
	if guard != nil {
		if err := guard(workspace); err != nil {
			return err
		}
	}
	worktreeDir, err := b.VerifyWorkspaceOwnership(ctx, workspace)
	if err != nil {
		return err
	}
	if !force {
		clean, status, err := b.Project.Repository.IsClean(ctx, workspace.Path)
		if err != nil {
			return err
		}
		if !clean {
			return fmt.Errorf("workspace %q has uncommitted or untracked changes; use --force to discard them:\n%s", name, status)
		}
	}
	record, err := b.Journal.Begin(operation.KindRemove, workspace.Name, workspace.Branch, workspace.Path, workspace.BaseCommit)
	if err != nil {
		return err
	}
	record.WorkspaceID = workspace.ID
	record.WorktreeDir = worktreeDir
	record.Force = force
	record.DeleteBranch = deleteBranch
	if err := b.Journal.Save(record); err != nil {
		return err
	}
	if err = b.Project.Repository.WorktreeRemove(ctx, workspace.Path, force); err != nil {
		return err
	}
	if err = b.Journal.Advance(&record, operation.StageWorktreeRemoved); err != nil {
		return err
	}
	if err = failpoint.Check("after-worktree-remove"); err != nil {
		return err
	}
	if err = b.Registry.Delete(name); err != nil {
		return err
	}
	if deleteBranch {
		if expectedBranchTip != "" {
			err = b.Project.Repository.DeleteBranchIfUnchanged(ctx, workspace.Branch, expectedBranchTip)
		} else {
			err = b.Project.Repository.DeleteBranch(ctx, workspace.Branch, force)
		}
		if err != nil {
			return fmt.Errorf("workspace removed, but branch %q was retained: %w", workspace.Branch, err)
		}
	}
	return b.Journal.Delete(record.ID)
}

// Recover uses the same workspace -> Git lock order as create and remove.
// Active operations are skipped rather than being mistaken for crashed ones.
func (b *Backend) Recover(ctx context.Context, dryRun bool) ([]RecoveryAction, error) {
	records, err := b.Journal.List()
	if err != nil {
		return nil, err
	}
	actions := make([]RecoveryAction, 0, len(records))
	for _, snapshot := range records {
		if err := ctx.Err(); err != nil {
			return actions, err
		}
		action, present, err := b.recoverOperation(ctx, snapshot, dryRun)
		if err != nil {
			return actions, err
		}
		if present {
			actions = append(actions, action)
		}
	}
	return actions, nil
}

func validateWorkspaceName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("workspace name cannot be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid workspace name %q", name)
	}
	return nil
}

func slug(value string) string {
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-.")
	if result == "" {
		return "workspace"
	}
	return result
}

// canonicalCreationPath resolves symlinks in the nearest existing ancestor and
// appends the not-yet-created suffix. filepath.EvalSymlinks alone cannot
// resolve a destination that does not exist, while a lexical filepath.Abs check
// can be bypassed by a symlinked parent.
func canonicalCreationPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	current := absolute
	var suffix []string
	for {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %s", absolute)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for index := len(suffix) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, suffix[index])
	}
	return filepath.Clean(resolved), nil
}

func pathWithin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
