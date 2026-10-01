package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/speculative"
)

type CacheEntry struct {
	Kind       string        `json:"kind"`
	ID         string        `json:"id"`
	Path       string        `json:"path"`
	Age        time.Duration `json:"-"`
	AgeSeconds float64       `json:"age_seconds"`
}

type CachePruneResult struct {
	Selected  []CacheEntry `json:"selected"`
	Removed   []CacheEntry `json:"removed"`
	Protected int          `json:"protected"`
	Retained  int          `json:"retained"`
}

// PruneCaches removes old immutable baselines and prepared layers that are not
// referenced by a registered workspace or an in-flight durable operation.
// Cache preparation and pruning share cache-admin.lock, while create records
// every selected cache before releasing that lock. This closes the dangerous
// prepare/prune/use race without putting a daemon or lock in the workspace I/O
// path after creation.
func (b *Backend) PruneCaches(ctx context.Context, olderThan time.Duration, dryRun bool) (CachePruneResult, error) {
	if olderThan <= 0 {
		return CachePruneResult{}, errors.New("cache prune age must be positive")
	}
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "cache-admin.lock"))
	if err != nil {
		return CachePruneResult{}, err
	}
	defer lock.Release()

	protectedBaselines := map[string]struct{}{}
	protectedLayers := map[string]struct{}{}
	workspaces, err := b.Registry.List()
	if err != nil {
		return CachePruneResult{}, err
	}
	for _, workspace := range workspaces {
		if workspace.BaseCommit != "" {
			protectedBaselines[workspace.BaseCommit] = struct{}{}
		}
		for _, id := range workspace.LayerIDs {
			protectedLayers[id] = struct{}{}
		}
	}
	store, storeErr := speculative.Open(b.Project.Repository, b.Project.SpeculativeDir(), b.Project.LocksDir())
	if storeErr != nil {
		return CachePruneResult{}, storeErr
	}
	speculativeLayers, storeErr := store.ReferencedEnvironmentIDs()
	if storeErr != nil {
		return CachePruneResult{}, storeErr
	}
	for _, id := range speculativeLayers {
		protectedLayers[id] = struct{}{}
	}

	operations, err := b.Journal.List()
	if err != nil {
		return CachePruneResult{}, err
	}
	for _, record := range operations {
		if record.BaseCommit != "" {
			protectedBaselines[record.BaseCommit] = struct{}{}
		}
		for _, id := range record.LayerIDs {
			protectedLayers[id] = struct{}{}
		}
	}

	result := CachePruneResult{}
	if err := b.pruneCacheRoot(b.Project.BaselinesDir(), "baseline", protectedBaselines, olderThan, dryRun, &result); err != nil {
		return result, err
	}
	if err := b.pruneCacheRoot(b.Project.LayersDir(), "layer", protectedLayers, olderThan, dryRun, &result); err != nil {
		return result, err
	}
	sort.Slice(result.Selected, func(i, j int) bool {
		if result.Selected[i].Kind == result.Selected[j].Kind {
			return result.Selected[i].ID < result.Selected[j].ID
		}
		return result.Selected[i].Kind < result.Selected[j].Kind
	})
	sort.Slice(result.Removed, func(i, j int) bool {
		if result.Removed[i].Kind == result.Removed[j].Kind {
			return result.Removed[i].ID < result.Removed[j].ID
		}
		return result.Removed[i].Kind < result.Removed[j].Kind
	})
	return result, nil
}

func (b *Backend) pruneCacheRoot(root, kind string, protected map[string]struct{}, olderThan time.Duration, dryRun bool, result *CachePruneResult) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now()
	for _, directoryEntry := range entries {
		id := directoryEntry.Name()
		path := filepath.Join(root, id)
		if _, ok := protected[id]; ok {
			result.Protected++
			continue
		}
		info, err := directoryEntry.Info()
		if err != nil {
			return err
		}
		modified := info.ModTime()
		if readyInfo, readyErr := os.Stat(filepath.Join(path, "ready")); readyErr == nil {
			modified = readyInfo.ModTime()
		} else if !errors.Is(readyErr, os.ErrNotExist) {
			return readyErr
		}
		age := now.Sub(modified)
		if age < olderThan {
			result.Retained++
			continue
		}
		entry := CacheEntry{Kind: kind, ID: id, Path: path, Age: age, AgeSeconds: age.Seconds()}
		result.Selected = append(result.Selected, entry)
		if dryRun {
			continue
		}
		if err := fsx.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s cache %s: %w", kind, id, err)
		}
		result.Removed = append(result.Removed, entry)
	}
	return nil
}

func touchCacheEntry(root string) error {
	path := filepath.Join(root, "ready")
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		return fmt.Errorf("touch cache %s: %w", root, err)
	}
	return nil
}
