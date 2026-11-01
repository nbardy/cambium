package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

func TestCacheBudgetEvictsOldestAcrossBothRoots(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	backend := openBackend(t, ctx, initializeRepository(t, false))
	now := time.Now()
	oldest := budgetCacheEntry(t, backend.Project.LayersDir(), "oldest", 12, now.Add(-3*time.Hour))
	middle := budgetCacheEntry(t, backend.Project.BaselinesDir(), "middle", 8, now.Add(-2*time.Hour))
	newest := budgetCacheEntry(t, backend.Project.LayersDir(), "newest", 4, now.Add(-time.Hour))

	// The exact boundary retains everything. A lower budget must evict the
	// oldest layer before the newer baseline, despite the root traversal order.
	boundary, err := backend.PruneCachesWithOptions(ctx, budgetOptions(24, false))
	if err != nil || len(boundary.Selected) != 0 || boundary.Retained != 3 || boundary.RetainedBytes != 24 {
		t.Fatalf("exact budget boundary was not retained: %#v, %v", boundary, err)
	}
	dryRun, err := backend.PruneCachesWithOptions(ctx, budgetOptions(12, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := budgetEntryNames(dryRun.Selected); !reflect.DeepEqual(got, []string{"layer/oldest"}) {
		t.Fatalf("selected %v, want oldest layer", got)
	}
	if dryRun.ByteAccounting != "logical_regular_file_bytes" || dryRun.MaxUnusedBytes == nil || *dryRun.MaxUnusedBytes != 12 ||
		dryRun.EligibleBytes != 24 || dryRun.SelectedBytes != 12 || dryRun.RetainedBytes != 12 || dryRun.RemovedBytes != 0 || dryRun.Retained != 2 {
		t.Fatalf("incorrect dry-run accounting: %#v", dryRun)
	}
	for _, path := range []string{oldest, middle, newest} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry-run changed %s: %v", path, err)
		}
	}
	removed, err := backend.PruneCachesWithOptions(ctx, budgetOptions(12, false))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(budgetEntryNames(removed.Selected), budgetEntryNames(dryRun.Selected)) || removed.RemovedBytes != 12 {
		t.Fatalf("actual prune disagreed with its dry-run: %#v", removed)
	}
	if _, err := os.Stat(oldest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest cache survived eviction: %v", err)
	}
	for _, path := range []string{middle, newest} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained cache disappeared: %v", err)
		}
	}
}

func TestCacheBudgetUsesReadyTimestampAndDeterministicTies(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	backend := openBackend(t, ctx, initializeRepository(t, false))
	old := time.Now().Add(-24 * time.Hour)
	for _, entry := range []struct{ root, name string }{
		{backend.Project.LayersDir(), "a"},
		{backend.Project.BaselinesDir(), "b"},
		{backend.Project.BaselinesDir(), "a"},
	} {
		budgetCacheEntry(t, entry.root, entry.name, 5, old)
	}
	// Directory mtime is old, but the ready marker was touched on reuse.
	reused := budgetCacheEntry(t, backend.Project.LayersDir(), "reused", 5, time.Now())
	if err := os.Chtimes(reused, old.Add(-time.Hour), old.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	result, err := backend.PruneCachesWithOptions(ctx, budgetOptions(5, false))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"baseline/a", "baseline/b", "layer/a"}
	if got := budgetEntryNames(result.Removed); !reflect.DeepEqual(got, want) {
		t.Fatalf("eviction order = %v, want %v", got, want)
	}
	if result.EligibleBytes != 20 || result.RemovedBytes != 15 || result.RetainedBytes != 5 {
		t.Fatalf("incorrect LRU accounting: %#v", result)
	}
	if _, err := os.Stat(reused); err != nil {
		t.Fatalf("recently reused cache was evicted: %v", err)
	}
}

func TestCacheBudgetCountsMetadataWithoutFollowingSymlinks(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	backend := openBackend(t, ctx, initializeRepository(t, false))
	external := canonicalTempDir(t)
	mustWrite(t, filepath.Join(external, "payload"), strings.Repeat("x", 1024), 0o644)
	mustWrite(t, filepath.Join(external, "ready"), "external-ready", 0o644)
	entry := budgetCacheEntry(t, backend.Project.LayersDir(), "with-links", 7, time.Now())
	// The helper's metadata is part of the seven logical bytes. Links to
	// external files/directories must neither increase the total nor be read.
	for name, target := range map[string]string{
		"linked-file": filepath.Join(external, "payload"),
		"linked-dir":  external,
		"broken-link": filepath.Join(external, "does-not-exist"),
	} {
		if err := os.Symlink(target, filepath.Join(entry, name)); err != nil {
			t.Fatal(err)
		}
	}
	rootLink := filepath.Join(backend.Project.BaselinesDir(), "external-link")
	if err := os.Symlink(external, rootLink); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(backend.Project.LayersDir(), ".interrupted-empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := backend.PruneCachesWithOptions(ctx, budgetOptions(0, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 3 || result.EligibleBytes != 7 || result.SelectedBytes != 7 || result.RemovedBytes != 7 || result.RetainedBytes != 0 {
		t.Fatalf("zero budget or link accounting was incorrect: %#v", result)
	}
	for _, path := range []string{entry, rootLink, empty} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("zero budget retained %s: %v", path, err)
		}
	}
	if got := mustRead(t, filepath.Join(external, "payload")); got != strings.Repeat("x", 1024) {
		t.Fatal("pruning a symlink changed its external target")
	}
	if got := mustRead(t, filepath.Join(external, "ready")); got != "external-ready" {
		t.Fatal("pruning a symlink changed its external ready marker")
	}
}

func TestCacheBudgetProtectsWorkspaceAndInterruptedCreate(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "workspace"
		if interrupted {
			name = "journal"
		}
		t.Run(name, func(t *testing.T) {
			requireGit(t)
			ctx := context.Background()
			root := initializeRepository(t, true)
			writePolicy(t, root, `
[[path]]
path = "deps"
policy = "clone"
inputs = ["deps.lock"]
`)
			backend := openBackend(t, ctx, root)
			if interrupted {
				t.Setenv("CAMBIUM_FAILPOINT", "after-register")
			}
			workspace, err := backend.Create(ctx, native.CreateSpec{Name: "budget-owner", Materializer: model.MaterializerCopy})
			if interrupted {
				if !errors.Is(err, failpoint.ErrInjectedCrash) {
					t.Fatalf("expected interrupted create, got %v", err)
				}
				t.Setenv("CAMBIUM_FAILPOINT", "")
			} else if err != nil {
				t.Fatal(err)
			}
			orphan := budgetCacheEntry(t, backend.Project.LayersDir(), "orphan", 11, time.Now())
			protected, err := backend.PruneCachesWithOptions(ctx, budgetOptions(0, false))
			if err != nil {
				t.Fatal(err)
			}
			if protected.Protected != 2 || protected.ProtectedBytes == 0 || protected.EligibleBytes != 11 || protected.RemovedBytes != 11 || len(protected.Removed) != 1 {
				t.Fatalf("zero budget did not distinguish protected and unused entries: %#v", protected)
			}
			if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unprotected cache survived: %v", err)
			}
			assertCacheCounts(t, backend, 1, 1)
			if interrupted {
				if _, err := backend.Recover(ctx, false); err != nil {
					t.Fatal(err)
				}
			} else {
				if got := mustRead(t, filepath.Join(workspace.Path, "deps", "seed.txt")); got != "prepared\n" {
					t.Fatal("budget prune changed a live environment")
				}
				if err := backend.Remove(ctx, workspace.Name, true, true); err != nil {
					t.Fatal(err)
				}
			}
			released, err := backend.PruneCachesWithOptions(ctx, budgetOptions(0, false))
			if err != nil {
				t.Fatal(err)
			}
			if released.Protected != 0 || len(released.Removed) != 2 || released.RemovedBytes != protected.ProtectedBytes || released.RetainedBytes != 0 {
				t.Fatalf("released caches were not removed: %#v", released)
			}
			assertCacheCounts(t, backend, 0, 0)
		})
	}
}

func TestCacheBudgetRejectsInvalidOptionsWithoutChangingCaches(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	backend := openBackend(t, ctx, initializeRepository(t, false))
	entry := budgetCacheEntry(t, backend.Project.LayersDir(), "unused", 4, time.Now())
	negative := int64(-1)
	zero := int64(0)
	for _, options := range []native.CachePruneOptions{
		{},
		{MaxUnusedBytes: &negative},
		{MaxUnusedBytes: &zero, OlderThan: time.Hour},
		{MaxUnusedBytes: &zero, OlderThan: -time.Hour},
		{OlderThan: -time.Hour},
	} {
		if _, err := backend.PruneCachesWithOptions(ctx, options); err == nil {
			t.Fatalf("accepted invalid options: %#v", options)
		}
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("invalid options changed cache: %v", err)
	}
}

func TestCacheBudgetCancellationAndInventoryErrorsDoNotPrune(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	backend := openBackend(t, ctx, initializeRepository(t, false))
	entry := budgetCacheEntry(t, backend.Project.BaselinesDir(), "unused", 4, time.Now())
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := backend.PruneCachesWithOptions(cancelled, budgetOptions(0, false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prune returned %v", err)
	}
	lock, err := lockfile.Acquire(filepath.Join(backend.Project.LocksDir(), "cache-admin.lock"))
	if err != nil {
		t.Fatal(err)
	}
	waiting, stopWaiting := context.WithTimeout(ctx, 100*time.Millisecond)
	_, waitErr := backend.PruneCachesWithOptions(waiting, budgetOptions(0, false))
	stopWaiting()
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("prune did not wait for cache-admin lock: %v", waitErr)
	}

	// Inventory must finish before any removal. A cache-root symlink is
	// rejected, and even the already inventoried baseline remains untouched.
	external := canonicalTempDir(t)
	mustWrite(t, filepath.Join(external, "keep"), "external data", 0o644)
	if err := os.Remove(backend.Project.LayersDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, backend.Project.LayersDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PruneCachesWithOptions(ctx, budgetOptions(0, false)); err == nil || !strings.Contains(err.Error(), "root is not a directory") {
		t.Fatalf("cache root symlink was not rejected: %v", err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("cancelled or failed inventory changed an unused cache: %v", err)
	}
	if got := mustRead(t, filepath.Join(external, "keep")); got != "external data" {
		t.Fatal("cache-root symlink traversal changed external data")
	}
}

func TestCacheAgePruneRetainsPolicyAndAddsByteAccounting(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	backend := openBackend(t, ctx, initializeRepository(t, false))
	oldest := budgetCacheEntry(t, backend.Project.LayersDir(), "old", 12, time.Now().Add(-2*time.Hour))
	newest := budgetCacheEntry(t, backend.Project.LayersDir(), "new", 8, time.Now())
	result, err := backend.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.MaxUnusedBytes != nil || result.ByteAccounting != "logical_regular_file_bytes" || result.EligibleBytes != 20 || result.RemovedBytes != 12 || result.RetainedBytes != 8 || result.Retained != 1 {
		t.Fatalf("age prune accounting is incorrect: %#v", result)
	}
	if _, err := os.Stat(oldest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old entry was retained: %v", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("young entry was removed: %v", err)
	}
}

func budgetOptions(bytes int64, dryRun bool) native.CachePruneOptions {
	return native.CachePruneOptions{MaxUnusedBytes: &bytes, DryRun: dryRun}
}

func budgetEntryNames(entries []native.CacheEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Kind+"/"+entry.ID)
	}
	return names
}

func budgetCacheEntry(t *testing.T, root, name string, bytes int, lastUsed time.Time) string {
	t.Helper()
	if bytes < 2 {
		t.Fatal("cache fixture must include its two metadata bytes")
	}
	path := filepath.Join(root, name)
	mustWrite(t, filepath.Join(path, "metadata.json"), "{}", 0o644)
	mustWrite(t, filepath.Join(path, "payload", "data"), strings.Repeat("x", bytes-2), 0o644)
	mustWrite(t, filepath.Join(path, "ready"), "", 0o644)
	if err := os.Chtimes(filepath.Join(path, "ready"), lastUsed, lastUsed); err != nil {
		t.Fatal(err)
	}
	return path
}
