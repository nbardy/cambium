package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/speculative"
	"github.com/nbardy/cambium/internal/state"
)

type CheckpointOptions struct {
	RefName string
	Message string
	Parent  string
	Detach  bool
}

type CheckpointResult struct {
	Root           speculative.ID  `json:"root"`
	Tree           string          `json:"tree"`
	Ref            speculative.Ref `json:"ref"`
	BaseCommit     string          `json:"base_commit"`
	ChangedPaths   []string        `json:"changed_paths"`
	EnvironmentIDs []string        `json:"environment_ids,omitempty"`
	Attempts       int             `json:"attempts"`
}

type ForkResult struct {
	Root      speculative.ID  `json:"root"`
	Tree      string          `json:"tree"`
	Workspace model.Workspace `json:"workspace"`
	Applied   int             `json:"applied"`
}

func (m *Manager) SpecStore() (*speculative.Store, error) {
	return speculative.Open(m.Project.Repository, m.Project.SpeculativeDir(), m.Project.LocksDir())
}

func (m *Manager) Checkpoint(ctx context.Context, workspaceName string, options CheckpointOptions) (CheckpointResult, error) {
	lock, err := m.Native.LockWorkspace(ctx, workspaceName)
	if err != nil {
		return CheckpointResult{}, err
	}
	defer lock.Release()
	workspaceValue, err := m.loadReadyWorkspaceLocked(ctx, workspaceName)
	if err != nil {
		return CheckpointResult{}, err
	}
	store, err := m.SpecStore()
	if err != nil {
		return CheckpointResult{}, err
	}
	refName := strings.TrimSpace(options.RefName)
	if refName == "" {
		refName = workspaceName
	}
	parent, err := m.resolveCheckpointParent(ctx, store, workspaceValue, refName, options.Parent, options.Detach)
	if err != nil {
		return CheckpointResult{}, err
	}
	checkpoint, err := store.Capture(ctx, workspaceValue.Path, speculative.Ref{
		Name:            refName,
		Parent:          parent,
		SourceWorkspace: workspaceValue.Name,
		Message:         options.Message,
		CreatedAt:       time.Now().UTC(),
	})
	if err != nil {
		return CheckpointResult{}, err
	}
	cacheLock, err := m.lockEnvironmentCache(ctx)
	if err != nil {
		return CheckpointResult{}, err
	}
	defer cacheLock.Release()
	layers, err := native.CaptureWorkspaceLayers(ctx, m.Project, workspaceValue.Path, checkpoint.Root.ID.String(), m.Native.Cloner, m.Project.Config.RequireCoW)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("capture workspace environments: %w", err)
	}
	environmentIDs := make([]string, 0, len(layers))
	for _, layer := range layers {
		environmentIDs = append(environmentIDs, layer.ID)
	}
	checkpoint.Ref.EnvironmentIDs = environmentIDs
	if err := store.SaveRef(ctx, checkpoint.Root, checkpoint.Ref); err != nil {
		return CheckpointResult{}, fmt.Errorf("attach environments to checkpoint: %w", err)
	}
	ready, missing, unverified := environmentReadiness(layers)
	workspaceValue.SpeculativeRoot = checkpoint.Root.ID.String()
	workspaceValue.LayerIDs = environmentIDs
	workspaceValue.EnvironmentReady = ready
	workspaceValue.EnvironmentMissing = missing
	workspaceValue.EnvironmentUnverified = unverified
	workspaceValue.LastUsedAt = time.Now().UTC()
	if err := m.saveWorkspaceLocked(ctx, workspaceValue); err != nil {
		return CheckpointResult{}, fmt.Errorf("record workspace speculative root: %w", err)
	}
	return CheckpointResult{
		Root:           checkpoint.Root.ID,
		Tree:           checkpoint.Root.Tree,
		Ref:            checkpoint.Ref,
		BaseCommit:     checkpoint.Root.BaseCommit,
		ChangedPaths:   checkpoint.ChangedPaths,
		EnvironmentIDs: environmentIDs,
		Attempts:       checkpoint.Attempts,
	}, nil
}

func (m *Manager) resolveCheckpointParent(ctx context.Context, store *speculative.Store, workspaceValue model.Workspace, refName, explicit string, detach bool) (speculative.ID, error) {
	if detach {
		return "", nil
	}
	if value := strings.TrimSpace(explicit); value != "" {
		id, _, _, err := store.Resolve(ctx, value)
		if err != nil {
			return "", fmt.Errorf("resolve explicit parent %q: %w", value, err)
		}
		return id, nil
	}
	if value := strings.TrimSpace(workspaceValue.SpeculativeRoot); value != "" {
		id, err := speculative.ParseID(value)
		if err != nil {
			return "", fmt.Errorf("workspace %q has invalid speculative provenance: %w", workspaceValue.Name, err)
		}
		if _, err := store.GetRoot(ctx, id); err != nil {
			return "", fmt.Errorf("resolve workspace %q speculative provenance: %w", workspaceValue.Name, err)
		}
		return id, nil
	}
	previous, err := store.LoadRef(ctx, refName)
	if err == nil {
		return previous.Root, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return "", fmt.Errorf("resolve previous checkpoint %q: %w", refName, err)
}

func (m *Manager) Fork(ctx context.Context, source string, spec CreateSpec) (result ForkResult, err error) {
	store, err := m.SpecStore()
	if err != nil {
		return ForkResult{}, err
	}
	rootID, root, sourceRef, err := store.Resolve(ctx, source)
	if err != nil {
		return ForkResult{}, err
	}
	if strings.TrimSpace(spec.Name) == "" {
		return ForkResult{}, errors.New("fork workspace name cannot be empty")
	}
	spec.Ref = root.BaseCommit
	created, err := m.Create(ctx, spec)
	if err != nil {
		return ForkResult{}, err
	}
	rollback := true
	defer func() {
		if rollback {
			if cleanupErr := m.removeFailedFork(context.Background(), created); cleanupErr != nil && !errors.Is(cleanupErr, state.ErrWorkspaceNotFound) {
				err = errors.Join(err, fmt.Errorf("clean up failed fork: %w", cleanupErr))
			}
		}
	}()
	workspaceLock, err := m.Native.LockWorkspace(ctx, created.Name)
	if err != nil {
		return ForkResult{}, err
	}
	// Registered after rollback so this lock is released before conditional
	// removal reacquires it. The ID check still protects that handoff.
	defer workspaceLock.Release()
	current, err := m.Registry.Load(created.Name)
	if err != nil {
		return ForkResult{}, err
	}
	if current.ID != created.ID {
		return ForkResult{}, fmt.Errorf("workspace %q was replaced before fork restoration", created.Name)
	}
	created = current
	if _, err := m.Native.VerifyWorkspaceOwnership(ctx, created); err != nil {
		return ForkResult{}, fmt.Errorf("verify fork workspace before restoration: %w", err)
	}

	applied, err := store.Restore(ctx, root, created.Path)
	if err != nil {
		return ForkResult{}, err
	}
	if sourceRef != nil && len(sourceRef.EnvironmentIDs) > 0 {
		// The source checkpoint can be dropped while its environment is being
		// restored. Hold workspace -> cache until the final metadata save gives
		// these receipts a new owner, so pruning cannot remove an active source.
		cacheLock, err := lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "cache-admin.lock"))
		if err != nil {
			return ForkResult{}, err
		}
		defer cacheLock.Release()
		layers, err := native.LoadLayers(m.Project, sourceRef.EnvironmentIDs)
		if err != nil {
			return ForkResult{}, err
		}
		result, err := native.ApplyLayersReplacing(ctx, m.Project, created.Path, layers, m.Native.Cloner, spec.RequireCoW || m.Project.Config.RequireCoW)
		if err != nil {
			return ForkResult{}, fmt.Errorf("restore checkpoint environments: %w", err)
		}
		created.LayerIDs = result.IDs
		created.EnvironmentReady = result.Ready
		created.EnvironmentMissing = result.Missing
		created.EnvironmentUnverified = result.Unverified
	}
	created.SpeculativeRoot = rootID.String()
	created.LastUsedAt = time.Now().UTC()
	if _, err := m.Native.VerifyWorkspaceOwnership(ctx, created); err != nil {
		return ForkResult{}, fmt.Errorf("verify fork workspace before publication: %w", err)
	}
	if err := m.Registry.Save(created); err != nil {
		return ForkResult{}, err
	}
	rollback = false
	return ForkResult{Root: rootID, Tree: root.Tree, Workspace: created, Applied: applied}, nil
}

func (m *Manager) removeFailedFork(ctx context.Context, created model.Workspace) error {
	return m.Native.RemoveCreated(ctx, created)
}

func environmentReadiness(layers []native.LayerSnapshot) (bool, []string, []string) {
	ready := true
	missing := make([]string, 0)
	unverified := make([]string, 0)
	for _, layer := range layers {
		if layer.Rule.Mode == "share" && layer.Rule.Required && !layer.Present {
			ready = false
			missing = append(missing, layer.Rule.Path)
		}
		if layer.Rule.Mode == "clone" || layer.Rule.Mode == "recreate" {
			if !layer.Present && (layer.Expected || layer.Rule.Required) {
				ready = false
				missing = append(missing, layer.Rule.Path)
			} else if layer.Present && !layer.Validated {
				ready = false
				unverified = append(unverified, layer.Rule.Path)
			}
		}
	}
	return ready, missing, unverified
}

func (m *Manager) RootGC(ctx context.Context, olderThan time.Duration, dryRun bool) (speculative.GCResult, error) {
	values, err := m.Registry.List()
	if err != nil {
		return speculative.GCResult{}, err
	}
	protected := make([]speculative.ID, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value.SpeculativeRoot) == "" {
			continue
		}
		id, err := speculative.ParseID(value.SpeculativeRoot)
		if err == nil {
			protected = append(protected, id)
		}
	}
	store, err := m.SpecStore()
	if err != nil {
		return speculative.GCResult{}, err
	}
	return store.GC(ctx, protected, olderThan, dryRun)
}
