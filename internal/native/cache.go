package native

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/speculative"
)

type CacheEntry struct {
	Kind         string        `json:"kind"`
	ID           string        `json:"id"`
	Path         string        `json:"path"`
	Age          time.Duration `json:"-"`
	AgeSeconds   float64       `json:"age_seconds"`
	LogicalBytes int64         `json:"logical_bytes"`
}

type CachePruneResult struct {
	Selected       []CacheEntry `json:"selected"`
	Removed        []CacheEntry `json:"removed"`
	Protected      int          `json:"protected"`
	Retained       int          `json:"retained"`
	MaxUnusedBytes *int64       `json:"max_unused_bytes,omitempty"`
	ByteAccounting string       `json:"byte_accounting"`
	ProtectedBytes int64        `json:"protected_bytes"`
	EligibleBytes  int64        `json:"eligible_bytes"`
	RetainedBytes  int64        `json:"retained_bytes"`
	SelectedBytes  int64        `json:"selected_bytes"`
	RemovedBytes   int64        `json:"removed_bytes"`
}

// CachePruneOptions selects exactly one eviction policy. A non-nil byte budget
// limits unreferenced cache retention; referenced entries are always protected
// outside that budget. Zero removes every unreferenced entry. Byte accounting
// sums regular-file lengths, including cache metadata, without following links.
// It is not a physical allocation limit on filesystems with shared extents.
type CachePruneOptions struct {
	OlderThan      time.Duration
	MaxUnusedBytes *int64
	DryRun         bool
}

// PruneCaches removes old immutable baselines and prepared layers that are not
// referenced by a registered workspace or an in-flight durable operation.
// Cache preparation and pruning share cache-admin.lock, while create records
// every selected cache before releasing that lock. This closes the dangerous
// prepare/prune/use race without putting a daemon or lock in the workspace I/O
// path after creation.
func (b *Backend) PruneCaches(ctx context.Context, olderThan time.Duration, dryRun bool) (CachePruneResult, error) {
	return b.PruneCachesWithOptions(ctx, CachePruneOptions{OlderThan: olderThan, DryRun: dryRun})
}

// PruneCachesWithOptions inventories both cache roots before changing either.
// Budget mode evicts the least recently used unreferenced entries first across
// both roots, using ready-marker times (or entry times for incomplete entries).
// The retained/selected byte counts describe the plan even in dry-run mode;
// RemovedBytes counts only entries whose removal completed successfully.
func (b *Backend) PruneCachesWithOptions(ctx context.Context, options CachePruneOptions) (CachePruneResult, error) {
	result := CachePruneResult{ByteAccounting: "logical_regular_file_bytes"}
	if options.MaxUnusedBytes != nil {
		if options.OlderThan != 0 {
			return result, errors.New("cache prune age and unused-byte budget are mutually exclusive")
		}
		budget := *options.MaxUnusedBytes
		if budget < 0 {
			return result, errors.New("cache prune unused-byte budget must not be negative")
		}
		result.MaxUnusedBytes = &budget
	} else if options.OlderThan <= 0 {
		return result, errors.New("cache prune age must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(b.Project.LocksDir(), "cache-admin.lock"))
	if err != nil {
		return result, err
	}
	defer lock.Release()

	protectedBaselines := map[string]struct{}{}
	protectedLayers := map[string]struct{}{}
	workspaces, err := b.Registry.List()
	if err != nil {
		return result, err
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
		return result, storeErr
	}
	speculativeLayers, storeErr := store.ReferencedEnvironmentIDs()
	if storeErr != nil {
		return result, storeErr
	}
	for _, id := range speculativeLayers {
		protectedLayers[id] = struct{}{}
	}

	operations, err := b.Journal.List()
	if err != nil {
		return result, err
	}
	for _, record := range operations {
		if record.BaseCommit != "" {
			protectedBaselines[record.BaseCommit] = struct{}{}
		}
		for _, id := range record.LayerIDs {
			protectedLayers[id] = struct{}{}
		}
	}

	now := time.Now()
	var candidates []cachePruneCandidate
	for _, root := range []struct {
		path      string
		kind      string
		protected map[string]struct{}
	}{
		{b.Project.BaselinesDir(), "baseline", protectedBaselines},
		{b.Project.LayersDir(), "layer", protectedLayers},
	} {
		entries, err := inventoryCacheRoot(ctx, root.path, root.kind, root.protected, now, &result)
		if err != nil {
			return result, err
		}
		candidates = append(candidates, entries...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if result.MaxUnusedBytes != nil && !left.modified.Equal(right.modified) {
			return left.modified.Before(right.modified)
		}
		if left.Kind == right.Kind {
			return left.ID < right.ID
		}
		return left.Kind < right.Kind
	})
	result.RetainedBytes = result.EligibleBytes
	for _, candidate := range candidates {
		selected := candidate.Age >= options.OlderThan
		if result.MaxUnusedBytes != nil {
			selected = *result.MaxUnusedBytes == 0 || result.RetainedBytes > *result.MaxUnusedBytes
		}
		if !selected {
			result.Retained++
			continue
		}
		result.Selected = append(result.Selected, candidate.CacheEntry)
		result.SelectedBytes += candidate.LogicalBytes
		result.RetainedBytes -= candidate.LogicalBytes
	}
	if options.DryRun {
		return result, nil
	}
	for _, entry := range result.Selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := fsx.RemoveAll(entry.Path); err != nil {
			return result, fmt.Errorf("remove %s cache %s: %w", entry.Kind, entry.ID, err)
		}
		result.Removed = append(result.Removed, entry)
		result.RemovedBytes += entry.LogicalBytes
	}
	return result, nil
}

type cachePruneCandidate struct {
	CacheEntry
	modified time.Time
}

func inventoryCacheRoot(ctx context.Context, root, kind string, protected map[string]struct{}, now time.Time, result *CachePruneResult) ([]cachePruneCandidate, error) {
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("%s cache root is not a directory: %s", kind, root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var candidates []cachePruneCandidate
	for _, directoryEntry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := directoryEntry.Name()
		path := filepath.Join(root, id)
		size, err := cacheLogicalBytes(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("measure %s cache %s: %w", kind, id, err)
		}
		if _, ok := protected[id]; ok {
			result.Protected++
			if err := addCacheBytes(&result.ProtectedBytes, size); err != nil {
				return nil, err
			}
			continue
		}
		if err := addCacheBytes(&result.EligibleBytes, size); err != nil {
			return nil, err
		}
		info, err := directoryEntry.Info()
		if err != nil {
			return nil, err
		}
		modified := info.ModTime()
		// Do not traverse a cache-entry symlink or trust an external ready
		// marker's timestamp. RemoveAll removes links themselves, not targets.
		if info.IsDir() {
			if readyInfo, readyErr := os.Lstat(filepath.Join(path, "ready")); readyErr == nil {
				if readyInfo.Mode().IsRegular() {
					modified = readyInfo.ModTime()
				}
			} else if !errors.Is(readyErr, os.ErrNotExist) {
				return nil, readyErr
			}
		}
		age := now.Sub(modified)
		entry := CacheEntry{Kind: kind, ID: id, Path: path, Age: age, AgeSeconds: age.Seconds(), LogicalBytes: size}
		candidates = append(candidates, cachePruneCandidate{CacheEntry: entry, modified: modified})
	}
	return candidates, nil
}

func cacheLogicalBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return addCacheBytes(&total, info.Size())
		}
		return nil
	})
	return total, err
}

func addCacheBytes(total *int64, size int64) error {
	if size < 0 || size > math.MaxInt64-*total {
		return errors.New("cache logical byte count exceeds int64")
	}
	*total += size
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
