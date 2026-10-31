package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/state"
)

type ReconcileChange struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

type ReconcileResult struct {
	Changes   []ReconcileChange `json:"changes"`
	Unmanaged []string          `json:"unmanaged,omitempty"`
	DryRun    bool              `json:"dry_run"`
}

func (m *Manager) Adopt(ctx context.Context, name, path string) (model.Workspace, error) {
	if name == "" {
		return model.Workspace{}, errors.New("adopted workspace name cannot be empty")
	}
	workspaceLock, err := m.Native.LockWorkspace(ctx, name)
	if err != nil {
		return model.Workspace{}, err
	}
	defer workspaceLock.Release()
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "reconcile.lock"))
	if err != nil {
		return model.Workspace{}, err
	}
	defer lock.Release()
	// Match creation's cache -> Git lock order. Adoption publishes environment
	// references and a workspace claim together, before either lock is released.
	cacheLock, err := m.lockEnvironmentCache(ctx)
	if err != nil {
		return model.Workspace{}, err
	}
	defer cacheLock.Release()
	gitLock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "git-admin.lock"))
	if err != nil {
		return model.Workspace{}, err
	}
	defer gitLock.Release()

	if exists, err := m.Registry.Exists(name); err != nil {
		return model.Workspace{}, err
	} else if exists {
		return model.Workspace{}, fmt.Errorf("workspace %q already exists", name)
	}
	absolute, err := existingCanonicalPath(path)
	if err != nil {
		return model.Workspace{}, err
	}
	worktrees, err := m.Project.Repository.ListWorktrees(ctx)
	if err != nil {
		return model.Workspace{}, err
	}
	var found *gitx.Worktree
	for _, item := range worktrees {
		itemPath, canonicalErr := existingCanonicalPath(item.Path)
		if canonicalErr != nil {
			itemPath = filepath.Clean(item.Path)
		}
		if itemPath == absolute {
			if item.Bare {
				return model.Workspace{}, fmt.Errorf("cannot adopt bare worktree %s", absolute)
			}
			if item.Branch == "" {
				return model.Workspace{}, fmt.Errorf("cannot adopt detached worktree %s without an explicit branch", absolute)
			}
			item.Path = absolute
			found = &item
			break
		}
	}
	if found == nil {
		return model.Workspace{}, fmt.Errorf("%s is not registered as a Git linked worktree", absolute)
	}
	worktreeGitDir, err := m.Native.CheckAdoption(ctx, name, *found)
	if err != nil {
		return model.Workspace{}, err
	}

	layers, err := native.CaptureWorkspaceLayers(ctx, m.Project, absolute, found.HEAD, m.Native.Cloner, m.Project.Config.RequireCoW)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("capture adopted workspace environment: %w", err)
	}
	ready, missing, unverified := environmentReadiness(layers)
	layerIDs := make([]string, 0, len(layers))
	for _, layer := range layers {
		layerIDs = append(layerIDs, layer.ID)
	}

	id, err := state.NewID()
	if err != nil {
		return model.Workspace{}, err
	}
	now := time.Now().UTC()
	value := model.Workspace{
		Version: 2, ID: id, Name: name, Status: model.WorkspaceReady,
		RepositoryRoot: m.Project.Repository.Root, CommonGitDir: m.Project.Repository.CommonGitDir,
		WorktreeGitDir: worktreeGitDir,
		BaseCommit:     found.HEAD, BaseRef: found.HEAD, Branch: found.Branch, Path: absolute,
		Requested: model.MaterializerGit, CloneMode: model.CloneModeGitCheckout,
		CreatedAt: now, LastUsedAt: now, LayerIDs: layerIDs,
		EnvironmentReady: ready, EnvironmentMissing: missing, EnvironmentUnverified: unverified,
	}
	// Trusted policy commands can invoke ordinary Git, which does not use our
	// locks. Do not publish the identity observed before such a command changed it.
	if err := m.Native.ClaimAdoption(ctx, value); err != nil {
		return model.Workspace{}, err
	}
	if err := failpoint.Check("after-adopt-claim"); err != nil {
		return model.Workspace{}, err
	}
	if err := m.Registry.Save(value); err != nil {
		return model.Workspace{}, fmt.Errorf("save adopted workspace: %w; ownership receipt retained at %s for inspection", err, worktreeGitDir)
	}
	return value, nil
}

func (m *Manager) Reconcile(ctx context.Context, dryRun bool) (ReconcileResult, error) {
	// Discovery is observational only. A workspace can be removed or replaced
	// after this snapshot, so no discovered record may be saved directly.
	registered, err := m.Registry.List()
	if err != nil {
		return ReconcileResult{}, err
	}
	worktrees, err := m.reconcileWorktrees(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{DryRun: dryRun}
	if dryRun {
		for _, value := range registered {
			_, changes := reconcileState(value, worktrees)
			result.Changes = append(result.Changes, changes...)
		}
	} else {
		// Take all discovered workspace locks in a stable order, then Git.
		// Never hold reconcile.lock while waiting for a workspace: adoption
		// takes its workspace lock before reconcile.lock.
		names := make([]string, 0, len(registered))
		identities := make(map[string]string, len(registered))
		for _, snapshot := range registered {
			names = append(names, snapshot.Name)
			identities[snapshot.Name] = snapshot.ID
		}
		locks, err := m.Native.LockWorkspaces(ctx, names)
		if err != nil {
			return result, err
		}
		defer func() {
			for i := len(locks) - 1; i >= 0; i-- {
				_ = locks[i].Release()
			}
		}()
		gitLock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "git-admin.lock"))
		if err != nil {
			return result, err
		}
		defer gitLock.Release()
		// Reread both sources after locking. A completed removal disappears,
		// and a replacement with the same name retains its different identity.
		registered, err = m.Registry.List()
		if err != nil {
			return result, err
		}
		worktrees, err = m.reconcileWorktrees(ctx)
		if err != nil {
			return result, err
		}
		for i, value := range registered {
			if id, selected := identities[value.Name]; !selected || value.ID != id {
				continue
			}
			updated, changes := reconcileState(value, worktrees)
			if err := m.Registry.Save(updated); err != nil {
				return result, err
			}
			registered[i] = updated
			result.Changes = append(result.Changes, changes...)
		}
	}
	result.Unmanaged = unmanagedWorktrees(registered, worktrees)
	sort.Slice(result.Changes, func(i, j int) bool {
		if result.Changes[i].Name == result.Changes[j].Name {
			return result.Changes[i].Action < result.Changes[j].Action
		}
		return result.Changes[i].Name < result.Changes[j].Name
	})
	sort.Strings(result.Unmanaged)
	return result, nil
}

func (m *Manager) Path(name string) (string, error) {
	value, err := m.Registry.Load(name)
	if err != nil {
		return "", err
	}
	return value.Path, nil
}

func existingCanonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
