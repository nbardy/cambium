package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

func TestCachePruneProtectsLiveWorkspacesAndRemovesOldOrphans(t *testing.T) {
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

	workspace, err := backend.Create(ctx, native.CreateSpec{Name: "cache-owner", Materializer: model.MaterializerCopy})
	if err != nil {
		t.Fatal(err)
	}
	ageCacheRoots(t, backend, 48*time.Hour)

	protected, err := backend.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if protected.Protected != 2 || len(protected.Removed) != 0 {
		t.Fatalf("live workspace caches were not protected: %#v", protected)
	}
	if err := backend.Remove(ctx, workspace.Name, true, true); err != nil {
		t.Fatal(err)
	}
	ageCacheRoots(t, backend, 48*time.Hour)

	dryRun, err := backend.PruneCaches(ctx, time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRun.Selected) != 2 || len(dryRun.Removed) != 0 {
		t.Fatalf("unexpected dry-run result: %#v", dryRun)
	}
	assertCacheCounts(t, backend, 1, 1)

	removed, err := backend.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed.Removed) != 2 {
		t.Fatalf("old unreferenced caches were not removed: %#v", removed)
	}
	assertCacheCounts(t, backend, 0, 0)
}

func TestCachePruneProtectsInterruptedCreateOperation(t *testing.T) {
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

	t.Setenv("CAMBIUM_FAILPOINT", "after-register")
	_, err := backend.Create(ctx, native.CreateSpec{Name: "cache-interrupted", Materializer: model.MaterializerCopy})
	if !errors.Is(err, failpoint.ErrInjectedCrash) {
		t.Fatalf("expected injected interruption, got %v", err)
	}
	t.Setenv("CAMBIUM_FAILPOINT", "")
	records, err := backend.Journal.List()
	if err != nil || len(records) != 1 || len(records[0].LayerIDs) != 1 {
		t.Fatalf("cache references were not journaled: %#v, %v", records, err)
	}
	ageCacheRoots(t, backend, 48*time.Hour)
	result, err := backend.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Protected != 2 || len(result.Removed) != 0 {
		t.Fatalf("in-flight operation caches were not protected: %#v", result)
	}
	if _, err := backend.Recover(ctx, false); err != nil {
		t.Fatal(err)
	}
	ageCacheRoots(t, backend, 48*time.Hour)
	result, err = backend.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 2 {
		t.Fatalf("recovered operation caches were not prunable: %#v", result)
	}
}

func ageCacheRoots(t *testing.T, backend *native.Backend, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	for _, root := range []string{backend.Project.BaselinesDir(), backend.Project.LayersDir()} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			path := filepath.Join(root, entry.Name(), "ready")
			if err := os.Chtimes(path, when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func assertCacheCounts(t *testing.T, backend *native.Backend, baselines, layers int) {
	t.Helper()
	for _, item := range []struct {
		path string
		want int
	}{{backend.Project.BaselinesDir(), baselines}, {backend.Project.LayersDir(), layers}} {
		entries, err := os.ReadDir(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != item.want {
			t.Fatalf("cache count for %s = %d, want %d", item.path, len(entries), item.want)
		}
	}
}
