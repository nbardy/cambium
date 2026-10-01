package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

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
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "reconcile.lock"))
	if err != nil {
		return model.Workspace{}, err
	}
	defer lock.Release()

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
	var found *struct{ head, branch string }
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
			found = &struct{ head, branch string }{item.HEAD, item.Branch}
			break
		}
	}
	if found == nil {
		return model.Workspace{}, fmt.Errorf("%s is not registered as a Git linked worktree", absolute)
	}
	primary, err := existingCanonicalPath(m.Project.Repository.Root)
	if err != nil {
		return model.Workspace{}, err
	}
	if absolute == primary {
		return model.Workspace{}, errors.New("the primary worktree cannot be adopted as an agent workspace")
	}

	layers, err := native.CaptureWorkspaceLayers(ctx, m.Project, absolute, found.head, m.Native.Cloner, m.Project.Config.RequireCoW)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("capture adopted workspace environment: %w", err)
	}
	ready, missing := environmentReadiness(layers)
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
		BaseCommit: found.head, BaseRef: found.head, Branch: found.branch, Path: absolute,
		Requested: model.MaterializerGit, CloneMode: model.CloneModeGitCheckout,
		CreatedAt: now, LastUsedAt: now, LayerIDs: layerIDs,
		EnvironmentReady: ready, EnvironmentMissing: missing,
	}
	if err := m.Registry.Save(value); err != nil {
		return model.Workspace{}, err
	}
	return value, nil
}

func (m *Manager) Reconcile(ctx context.Context, dryRun bool) (ReconcileResult, error) {
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "reconcile.lock"))
	if err != nil {
		return ReconcileResult{}, err
	}
	defer lock.Release()

	registered, err := m.Registry.List()
	if err != nil {
		return ReconcileResult{}, err
	}
	gitWorktrees, err := m.Project.Repository.ListWorktrees(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	byPath := make(map[string]int, len(gitWorktrees))
	byBranch := make(map[string]int, len(gitWorktrees))
	for i, item := range gitWorktrees {
		byPath[filepath.Clean(item.Path)] = i
		if item.Branch != "" {
			byBranch[item.Branch] = i
		}
	}
	managedPaths := map[string]struct{}{filepath.Clean(m.Project.Repository.Root): {}}
	result := ReconcileResult{DryRun: dryRun}
	for _, value := range registered {
		pathKey := filepath.Clean(value.Path)
		index, pathMatch := byPath[pathKey]
		if !pathMatch && value.Branch != "" {
			index, pathMatch = byBranch[value.Branch]
			if pathMatch && gitWorktrees[index].Path != value.Path {
				result.Changes = append(result.Changes, ReconcileChange{Name: value.Name, Action: "move", From: value.Path, To: gitWorktrees[index].Path})
				if !dryRun {
					value.Path = gitWorktrees[index].Path
				}
			}
		}
		if !pathMatch {
			if value.Status != model.WorkspaceBroken {
				result.Changes = append(result.Changes, ReconcileChange{Name: value.Name, Action: "mark-broken", From: value.Path})
				if !dryRun {
					value.Status = model.WorkspaceBroken
				}
			}
		} else {
			item := gitWorktrees[index]
			managedPaths[filepath.Clean(item.Path)] = struct{}{}
			if value.Status != model.WorkspaceReady {
				result.Changes = append(result.Changes, ReconcileChange{Name: value.Name, Action: "mark-ready", To: item.Path})
				if !dryRun {
					value.Status = model.WorkspaceReady
				}
			}
			if !dryRun {
				value.Branch = item.Branch
				value.LastUsedAt = time.Now().UTC()
			}
		}
		if !dryRun {
			if _, statErr := os.Stat(value.Path); statErr != nil && errors.Is(statErr, os.ErrNotExist) {
				value.Status = model.WorkspaceBroken
			}
			if err := m.Registry.Save(value); err != nil {
				return ReconcileResult{}, err
			}
		}
	}
	for _, item := range gitWorktrees {
		if item.Bare {
			continue
		}
		if _, ok := managedPaths[filepath.Clean(item.Path)]; !ok {
			result.Unmanaged = append(result.Unmanaged, item.Path)
		}
	}
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
