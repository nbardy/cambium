package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

func TestZeroCacheBudgetPreservesCheckpointReplayAfterSourceRemoval(t *testing.T) {
	ctx := context.Background()
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, ".gitignore", "deps/\n")
	writeReadinessFile(t, repo, "deps.lock", "v1\n")
	writeReadinessFile(t, repo, ".cambium.toml", `version = 1
[[path]]
path = "deps"
policy = "clone"
inputs = ["deps.lock"]
required = true
validate = ["cmp", "deps.lock", "deps/state"]
`)
	commitReadinessFiles(t, repo)
	writeReadinessFile(t, repo, "deps/state", "v1\n")
	manager := readinessManager(t, repo, true)
	original, err := manager.Create(ctx, CreateSpec{Name: "original", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	writeReadinessFile(t, original.Path, "deps.lock", "v2\n")
	writeReadinessFile(t, original.Path, "deps/state", "v2\n")
	checkpoint, err := manager.Checkpoint(ctx, original.Name, CheckpointOptions{RefName: "budget-snapshot"})
	if err != nil || len(checkpoint.EnvironmentIDs) != 1 {
		t.Fatalf("checkpoint did not capture the changed environment: %#v, %v", checkpoint, err)
	}
	if err := manager.Remove(ctx, original.Name, true, true); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "deps")); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	prune := func() native.CachePruneResult {
		t.Helper()
		result, err := manager.PruneCachesWithOptions(ctx, CachePruneOptions{MaxUnusedBytes: &zero})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := prune()
	if result.Protected < 1 || result.ProtectedBytes <= 0 || result.RetainedBytes != 0 || result.RemovedBytes <= 0 {
		t.Fatalf("zero budget did not separate the checkpoint from unused cache: %#v", result)
	}
	layers, err := native.LoadLayers(manager.Project, checkpoint.EnvironmentIDs)
	if err != nil || len(layers) != 1 || !layers[0].Present {
		t.Fatalf("checkpoint payload was not retained: %#v, %v", layers, err)
	}
	assertFile(t, filepath.Join(layers[0].Payload, "state"), "v2\n")

	first, err := manager.Fork(ctx, "budget-snapshot", CreateSpec{Name: "first", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Fork(ctx, "budget-snapshot", CreateSpec{Name: "second", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first.Workspace.Path, second.Workspace.Path} {
		assertFile(t, filepath.Join(path, "deps.lock"), "v2\n")
		assertFile(t, filepath.Join(path, "deps", "state"), "v2\n")
	}
	writeReadinessFile(t, first.Workspace.Path, "deps/state", "changed only in first\n")
	assertFile(t, filepath.Join(second.Workspace.Path, "deps", "state"), "v2\n")
	assertFile(t, filepath.Join(layers[0].Payload, "state"), "v2\n")

	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, "budget-snapshot"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RootGC(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	// Live forks retain their captured environment even after the named alias
	// is dropped and both root GC and an aggressive cache prune run.
	if result := prune(); result.ProtectedBytes <= 0 {
		t.Fatalf("live forks lost cache protection: %#v", result)
	}
	assertFile(t, filepath.Join(layers[0].Payload, "state"), "v2\n")
	for _, name := range []string{first.Workspace.Name, second.Workspace.Name} {
		if err := manager.Remove(ctx, name, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.RootGC(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	result = prune()
	if result.Protected != 0 || result.RetainedBytes != 0 || result.RemovedBytes <= 0 {
		t.Fatalf("released checkpoint payload was not reclaimed: %#v", result)
	}
	for _, root := range []string{manager.Project.BaselinesDir(), manager.Project.LayersDir()} {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("unused cache remains in %s: %v, %v", root, entries, err)
		}
	}
}
