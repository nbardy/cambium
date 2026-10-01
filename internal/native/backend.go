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
		value, err := EnsureBaseline(ctx, b.Project, commit)
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
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "workspace-"+slug(spec.Name)+".lock"))
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
	record, beginErr := b.Journal.Begin(operation.KindCreate, spec.Name, spec.Branch, spec.Path, prepared.Commit)
	if beginErr != nil {
		_ = cacheLock.Release()
		return model.Workspace{}, beginErr
	}
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
	createdWorktree := false
	createdBranch := false
	pathOwned := false
	defer func() {
		if err == nil || errors.Is(err, failpoint.ErrInjectedCrash) {
			return
		}
		// Registration and refs live in the common Git directory. Serialize the
		// rollback just like creation so a failed concurrent create cannot damage
		// another agent's successful workspace.
		cleanupLock, _ := lockfile.AcquireContext(context.Background(), filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
		if cleanupLock != nil {
			defer cleanupLock.Release()
		}
		if createdWorktree {
			_ = b.Project.Repository.WorktreeRemove(context.Background(), spec.Path, true)
		}
		if createdBranch {
			_ = b.Project.Repository.DeleteBranch(context.Background(), spec.Branch, true)
		}
		if pathOwned {
			_ = os.RemoveAll(spec.Path)
		}
		_ = b.Journal.Delete(record.ID)
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
	pathOwned = true
	createdWorktree = true // cleanup is safe even if Git fails midway
	createdBranch = true
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
	releaseErr := gitLock.Release()
	if err != nil {
		return model.Workspace{}, fmt.Errorf("register linked worktree: %w", err)
	}
	if releaseErr != nil {
		return model.Workspace{}, releaseErr
	}
	if err = b.Journal.Advance(&record, operation.StageWorktreeRegistered); err != nil {
		return model.Workspace{}, err
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

	id, err := state.NewID()
	if err != nil {
		return model.Workspace{}, err
	}
	now := time.Now().UTC()
	workspace = model.Workspace{
		Version:            2,
		ID:                 id,
		Name:               spec.Name,
		Status:             model.WorkspaceReady,
		RepositoryRoot:     b.Project.Repository.Root,
		CommonGitDir:       b.Project.Repository.CommonGitDir,
		BaseCommit:         prepared.Commit,
		BaseRef:            spec.Ref,
		Branch:             spec.Branch,
		Path:               spec.Path,
		Requested:          spec.Materializer,
		CloneMode:          cloneMode,
		PreparedIndex:      preparedIndex,
		Ephemeral:          spec.Ephemeral,
		CreatedAt:          now,
		LastUsedAt:         now,
		LayerIDs:           layerResult.IDs,
		EnvironmentReady:   layerResult.Ready,
		EnvironmentMissing: layerResult.Missing,
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

func (b *Backend) Remove(ctx context.Context, name string, force, deleteBranch bool) (err error) {
	workspace, err := b.Registry.Load(name)
	if err != nil {
		return err
	}
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "workspace-"+slug(name)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Release()
	record, err := b.Journal.Begin(operation.KindRemove, workspace.Name, workspace.Branch, workspace.Path, workspace.BaseCommit)
	if err != nil {
		return err
	}
	record.Force = force
	record.DeleteBranch = deleteBranch
	if err := b.Journal.Save(record); err != nil {
		return err
	}
	sideEffect := false
	defer func() {
		if err == nil || errors.Is(err, failpoint.ErrInjectedCrash) || sideEffect {
			return
		}
		_ = b.Journal.Delete(record.ID)
	}()
	gitLock, lockErr := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
	if lockErr != nil {
		return lockErr
	}
	err = b.Project.Repository.WorktreeRemove(ctx, workspace.Path, force)
	releaseErr := gitLock.Release()
	if err != nil {
		return err
	}
	if releaseErr != nil {
		return releaseErr
	}
	sideEffect = true
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
		if err = b.Project.Repository.DeleteBranch(ctx, workspace.Branch, force); err != nil {
			return fmt.Errorf("workspace removed, but branch %q was retained: %w", workspace.Branch, err)
		}
	}
	return b.Journal.Delete(record.ID)
}

func (b *Backend) Recover(ctx context.Context, dryRun bool) ([]RecoveryAction, error) {
	var gitLock *lockfile.Lock
	var err error
	if !dryRun {
		gitLock, err = lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "git-admin.lock"))
		if err != nil {
			return nil, err
		}
		defer gitLock.Release()
	}
	records, err := b.Journal.List()
	if err != nil {
		return nil, err
	}
	actions := make([]RecoveryAction, 0, len(records))
	for _, record := range records {
		action := RecoveryAction{OperationID: record.ID, Kind: string(record.Kind), Name: record.Name}
		switch record.Kind {
		case operation.KindCreate:
			exists, loadErr := b.Registry.Exists(record.Name)
			if loadErr != nil {
				return actions, loadErr
			}
			if exists {
				action.Action = "finalize"
				action.Status = "resolved"
				action.Message = "workspace metadata exists; removed stale operation record"
				if !dryRun {
					if err := b.Journal.Delete(record.ID); err != nil {
						return actions, err
					}
				}
				actions = append(actions, action)
				continue
			}
			branchExists, branchErr := b.Project.Repository.BranchExists(ctx, record.Branch)
			if branchErr != nil {
				return actions, branchErr
			}
			if branchExists {
				commit, commitErr := b.Project.Repository.BranchCommit(ctx, record.Branch)
				if commitErr != nil {
					return actions, commitErr
				}
				if commit != record.BaseCommit {
					action.Action = "protect"
					action.Status = "blocked"
					action.Message = fmt.Sprintf("branch advanced to %s; refusing automatic deletion", commit)
					actions = append(actions, action)
					continue
				}
			}
			action.Action = "rollback"
			action.Status = "resolved"
			if !dryRun {
				registered, regErr := b.Project.Repository.IsWorktreeRegistered(ctx, record.Path)
				if regErr != nil {
					return actions, regErr
				}
				if registered {
					if err := b.Project.Repository.WorktreeRemove(ctx, record.Path, true); err != nil {
						return actions, fmt.Errorf("recover worktree %s: %w", record.Path, err)
					}
				} else {
					_ = os.RemoveAll(record.Path)
				}
				if branchExists {
					if err := b.Project.Repository.DeleteBranch(ctx, record.Branch, true); err != nil {
						return actions, err
					}
				}
				if err := b.Journal.Delete(record.ID); err != nil {
					return actions, err
				}
			}
			action.Message = "removed incomplete linked worktree and unadvanced branch"
		case operation.KindRemove:
			action.Action = "finish-remove"
			registered, regErr := b.Project.Repository.IsWorktreeRegistered(ctx, record.Path)
			if regErr != nil {
				return actions, regErr
			}
			if !dryRun && registered {
				if err := b.Project.Repository.WorktreeRemove(ctx, record.Path, record.Force); err != nil {
					action.Status = "blocked"
					action.Message = err.Error()
					actions = append(actions, action)
					continue
				}
			}
			if !dryRun {
				if err := b.Registry.Delete(record.Name); err != nil {
					return actions, err
				}
				if record.DeleteBranch {
					exists, err := b.Project.Repository.BranchExists(ctx, record.Branch)
					if err != nil {
						return actions, err
					}
					if exists {
						if err := b.Project.Repository.DeleteBranch(ctx, record.Branch, record.Force); err != nil {
							action.Status = "blocked"
							action.Message = err.Error()
							actions = append(actions, action)
							continue
						}
					}
				}
				if err := b.Journal.Delete(record.ID); err != nil {
					return actions, err
				}
			}
			action.Status = "resolved"
			action.Message = "completed interrupted removal"
		default:
			action.Action = "inspect"
			action.Status = "blocked"
			action.Message = fmt.Sprintf("unknown operation kind %q", record.Kind)
		}
		actions = append(actions, action)
	}
	if !dryRun {
		_ = b.Project.Repository.WorktreePrune(ctx)
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
