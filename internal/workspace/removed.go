package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/speculative"
)

// RemovedPrefix names the hidden checkpoints taken before a workspace is
// removed: removed/<workspace>-<UTC time>.
const RemovedPrefix = "removed/"

// RemovedKeep is how long `maintain` keeps a pre-removal checkpoint.
const RemovedKeep = 14 * 24 * time.Hour

// SaveBeforeRemove saves a workspace as a hidden checkpoint so that no
// removal, --force included, destroys work that cannot be rebuilt from Git.
// It returns "" when there is nothing to save: the primary checkout (never
// removable) or a workspace whose directory is already gone. Any other
// failure stops the removal; a workspace is never deleted unsaved.
func (m *Manager) SaveBeforeRemove(ctx context.Context, name string) (string, error) {
	value, err := m.Registry.Load(name)
	if err != nil {
		return "", nil // not registered: let the remover report the ordinary error
	}
	if native.SamePath(value.Path, m.Project.PrimaryRoot()) || value.Status != model.WorkspaceReady {
		return "", nil
	}
	if _, err := os.Stat(value.Path); err != nil {
		return "", nil
	}
	ref := fmt.Sprintf("%s%s-%s", RemovedPrefix, name, time.Now().UTC().Format("20060102T150405Z"))
	if _, err := m.Checkpoint(ctx, name, CheckpointOptions{RefName: ref, Message: "before remove", Detach: true, Passive: true}); err != nil {
		return "", fmt.Errorf("checkpoint %q before removal (nothing was removed): %w", name, err)
	}
	return ref, nil
}

func (m *Manager) DiscardCheckpoint(ctx context.Context, ref string) error {
	store, err := m.SpecStore()
	if err != nil {
		return err
	}
	return store.DropRef(ctx, ref)
}

// RemoveCheckpointed saves a workspace, then removes it. It returns the
// checkpoint ref. Manager.Remove is the unsaved primitive underneath; every
// command that removes goes through a Save-first path.
func (m *Manager) RemoveCheckpointed(ctx context.Context, name string, force, deleteBranch bool) (checkpoint string, err error) {
	checkpoint, err = m.SaveBeforeRemove(ctx, name)
	if err != nil {
		return "", err
	}
	if err := m.Native.Remove(ctx, name, force, deleteBranch); err != nil {
		m.DiscardIfSurvived(ctx, name, checkpoint)
		return "", err
	}
	return checkpoint, nil
}

// DiscardIfSurvived drops a checkpoint whose removal failed before deleting
// anything: it would only hold cache bytes for a workspace that still exists.
func (m *Manager) DiscardIfSurvived(ctx context.Context, name, checkpoint string) {
	if checkpoint == "" {
		return
	}
	if value, err := m.Registry.Load(name); err == nil {
		if _, statErr := os.Stat(value.Path); statErr == nil {
			_ = m.DiscardCheckpoint(ctx, checkpoint)
		}
	}
}

// RemovedCheckpoints lists the pre-removal checkpoints, newest first.
func (m *Manager) RemovedCheckpoints(ctx context.Context) ([]speculative.Ref, error) {
	store, err := m.SpecStore()
	if err != nil {
		return nil, err
	}
	refs, err := store.ListRefs(ctx)
	if err != nil {
		return nil, err
	}
	var removed []speculative.Ref
	for _, ref := range refs {
		if strings.HasPrefix(ref.Name, RemovedPrefix) {
			removed = append(removed, ref)
		}
	}
	return removed, nil
}

// ExpireRemovedCheckpoints drops pre-removal checkpoints older than olderThan.
// Their cache bytes are then freed by the next root gc / prune.
func (m *Manager) ExpireRemovedCheckpoints(ctx context.Context, olderThan time.Duration, dryRun bool) ([]string, error) {
	removed, err := m.RemovedCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	var expired []string
	for _, ref := range removed {
		if time.Since(ref.CreatedAt) < olderThan {
			continue
		}
		expired = append(expired, ref.Name)
		if dryRun {
			continue
		}
		if err := m.DiscardCheckpoint(ctx, ref.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return expired, err
		}
	}
	return expired, nil
}
