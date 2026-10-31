package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/speculative"
)

func TestForkProtectsCheckpointLayerUntilWorkspacePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	repo := initSpecRepo(t)
	writeReadinessFile(t, repo, ".gitignore", "node_modules/\n")
	writeReadinessFile(t, repo, "package.json", "{\"name\":\"fork-cache\"}\n")
	writeReadinessFile(t, repo, "package-lock.json", "{\"lockfileVersion\":3,\"version\":\"1\"}\n")
	writeReadinessFile(t, repo, "node_modules/pkg/version.txt", "main-v1\n")
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-m", "initial environment")
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	source, err := manager.Create(ctx, CreateSpec{Name: "source", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	writeReadinessFile(t, source.Path, "package-lock.json", "{\"lockfileVersion\":3,\"version\":\"2\"}\n")
	writeReadinessFile(t, source.Path, "node_modules/pkg/version.txt", "checkpoint-v2\n")
	checkpoint, err := manager.Checkpoint(ctx, source.Name, CheckpointOptions{RefName: "checkpoint-v2"})
	if err != nil {
		t.Fatal(err)
	}
	layers, err := native.LoadLayers(p, checkpoint.EnvironmentIDs)
	if err != nil {
		t.Fatal(err)
	}
	var layer native.LayerSnapshot
	for _, candidate := range layers {
		if candidate.Rule.Path == "node_modules" {
			layer = candidate
		}
	}
	if !layer.Present || layer.Payload == "" || slices.Contains(source.LayerIDs, layer.ID) {
		t.Fatalf("fixture did not capture a distinct v2 payload: original=%#v layer=%#v", source.LayerIDs, layer)
	}
	if err := manager.Remove(ctx, source.Name, true, true); err != nil {
		t.Fatal(err)
	}
	runner := &forkLayerCopyPause{payload: layer.Payload, entered: make(chan struct{}), resume: make(chan struct{})}
	defer runner.release()
	forkProject, err := project.Open(ctx, repo, runner)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		value ForkResult
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := New(forkProject).Fork(ctx, checkpoint.Ref.Name, CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
		done <- outcome{value, err}
	}()
	select {
	case <-runner.entered:
	case result := <-done:
		t.Fatalf("fork returned before copying its v2 layer: %v", result.err)
	case <-ctx.Done():
		t.Fatal("fork did not reach the checkpoint-layer copy")
	}
	pending, err := manager.Registry.Load("forked")
	if err != nil || slices.Contains(pending.LayerIDs, layer.ID) {
		t.Fatalf("fork unexpectedly published the v2 receipt before its copy: %#v, %v", pending, err)
	}
	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	// Both root mutation and GC now wait for the active fork's publication.
	// Queue them while its actual v2 payload copy remains paused.
	dropCtx := &metadataWaitContext{Context: ctx, waiting: make(chan struct{})}
	dropDone := make(chan struct{})
	var dropErr error
	go func() {
		defer close(dropDone)
		dropErr = store.DropRef(dropCtx, checkpoint.Ref.Name)
	}()
	gcCtx := &metadataWaitContext{Context: ctx, waiting: make(chan struct{})}
	gcDone := make(chan struct{})
	var gc speculative.GCResult
	var gcErr error
	go func() {
		defer close(gcDone)
		gc, gcErr = manager.RootGC(gcCtx, 0, false)
	}()
	t.Cleanup(func() { runner.release(); cancel(); <-dropDone; <-gcDone })
	for _, waiter := range []struct {
		name    string
		waiting <-chan struct{}
		done    <-chan struct{}
	}{
		{"drop", dropCtx.waiting, dropDone},
		{"GC", gcCtx.waiting, gcDone},
	} {
		select {
		case <-waiter.waiting:
		case <-waiter.done:
			t.Fatalf("root %s finished while fork publication was paused", waiter.name)
		case <-ctx.Done():
			t.Fatalf("root %s did not reach the publication lock", waiter.name)
		}
	}
	pruneCtx, cancelPrune := context.WithTimeout(ctx, 300*time.Millisecond)
	pruned, pruneErr := manager.PruneCaches(pruneCtx, time.Nanosecond, false)
	cancelPrune()
	if !errors.Is(pruneErr, context.DeadlineExceeded) {
		t.Errorf("prune did not wait for the active fork's cache lease: result=%#v err=%v", pruned, pruneErr)
	}
	if _, err := os.Stat(layer.Payload); err != nil {
		t.Errorf("prune deleted the checkpoint payload during its actual copy: %v", err)
	}
	runner.release()
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		t.Fatal("fork did not complete after copy resumed")
	}
	if result.err != nil {
		t.Fatalf("fork failed after concurrent pruning: %v", result.err)
	}
	<-dropDone
	<-gcDone
	if dropErr != nil || gcErr != nil || slices.Contains(gc.ReleasedRoots, checkpoint.Root) || !slices.Contains(gc.Protected, checkpoint.Root) {
		t.Fatalf("queued root collection did not retain the published fork: drop=%v GC=%#v error=%v", dropErr, gc, gcErr)
	}
	assertFile(t, filepath.Join(result.value.Workspace.Path, "node_modules", "pkg", "version.txt"), "checkpoint-v2\n")
	assertFile(t, filepath.Join(result.value.Workspace.Path, "package-lock.json"), "{\"lockfileVersion\":3,\"version\":\"2\"}\n")
	published, err := manager.Registry.Load("forked")
	if err != nil || !slices.Contains(published.LayerIDs, layer.ID) {
		t.Fatalf("completed fork did not persist its v2 receipt reference: %#v, %v", published, err)
	}
	if _, err := manager.PruneCaches(ctx, time.Nanosecond, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layer.Payload); err != nil {
		t.Fatalf("fork publication did not protect the layer after releasing its cache lease: %v", err)
	}
}

// Pause the real copy of the selected checkpoint payload, not ClonePath's CoW
// probe or the initial v1 environment copied during workspace creation.
type forkLayerCopyPause struct {
	execx.OSRunner
	payload string
	entered chan struct{}
	resume  chan struct{}
	paused  sync.Once
	once    sync.Once
}

func (r *forkLayerCopyPause) release() { r.once.Do(func() { close(r.resume) }) }

func (r *forkLayerCopyPause) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	if command.Name == "cp" {
		for _, arg := range command.Args {
			if filepath.Clean(arg) != filepath.Clean(r.payload) {
				continue
			}
			r.paused.Do(func() {
				close(r.entered)
				select {
				case <-r.resume:
				case <-ctx.Done():
				}
			})
			if err := ctx.Err(); err != nil {
				return execx.Result{}, err
			}
			break
		}
	}
	return r.OSRunner.Run(ctx, command)
}
