package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/speculative"
	"github.com/nbardy/cambium/internal/state"
)

func TestForkRetainsRootUntilWorkspacePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager, checkpoint := forkCheckpointFixture(t)
	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(ctx, "original", true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, checkpoint.Ref.Name); err != nil {
		t.Fatal(err)
	}
	runner := &forkRestoreBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	forkProject, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	var forked ForkResult
	var forkErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		forked, forkErr = New(forkProject).Fork(ctx, checkpoint.Root.String(), CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
	}()
	t.Cleanup(func() { runner.release(); cancel(); <-done })
	select {
	case <-runner.entered:
	case <-done:
		t.Fatalf("fork finished before restore: %v", forkErr)
	case <-ctx.Done():
		t.Fatal("fork did not reach restore")
	}
	gcCtx, cancelGC := context.WithTimeout(ctx, 200*time.Millisecond)
	gc, gcErr := manager.RootGC(gcCtx, 0, false)
	cancelGC()
	runner.release()
	<-done
	if forkErr != nil {
		t.Fatalf("fork failed after root collection attempt: %v", forkErr)
	}
	assertPublishedRoot(t, manager, forked.Workspace.Name, checkpoint.Root)
	if !errors.Is(gcErr, context.DeadlineExceeded) {
		t.Fatalf("GC did not wait for restore and publication: result=%#v error=%v", gc, gcErr)
	}
	gc, err = manager.RootGC(ctx, 0, false)
	if err != nil || slices.Contains(gc.ReleasedRoots, checkpoint.Root) || !slices.Contains(gc.Protected, checkpoint.Root) {
		t.Fatalf("published fork did not protect its root: %#v, %v", gc, err)
	}
}

func TestForkRejectsRootCollectedBeforeRestoration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager, checkpoint := forkCheckpointFixture(t)
	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(ctx, "original", true, true); err != nil {
		t.Fatal(err)
	}
	runner := &metadataBarrierRunner{
		entered: make(chan struct{}), resume: make(chan struct{}),
		match: func(command execx.Command) bool {
			return command.Name == "git" && slices.Contains(command.Args, "worktree") && slices.Contains(command.Args, "add")
		},
	}
	forkProject, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
	var forkErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, forkErr = New(forkProject).Fork(ctx, checkpoint.Ref.Name, CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
	}()
	t.Cleanup(func() { release(); cancel(); <-done })
	select {
	case <-runner.entered:
	case <-done:
		t.Fatalf("fork finished before worktree creation: %v", forkErr)
	case <-ctx.Done():
		t.Fatal("fork did not reach worktree creation")
	}
	if err := store.DropRef(ctx, checkpoint.Ref.Name); err != nil {
		t.Fatal(err)
	}
	gc, err := manager.RootGC(ctx, 0, false)
	if err != nil || !slices.Contains(gc.ReleasedRoots, checkpoint.Root) {
		t.Fatalf("fixture did not collect the now-unowned root: %#v, %v", gc, err)
	}
	if _, err := store.GetRoot(ctx, checkpoint.Root); err != nil {
		t.Fatalf("GC unexpectedly removed Git's object: %v", err)
	}
	release()
	<-done
	if forkErr == nil {
		t.Fatal("fork succeeded after its source root reference and lineage metadata were collected")
	}
	if !strings.Contains(forkErr.Error(), "root") {
		t.Fatalf("fork did not report its missing root: %v", forkErr)
	}
	if _, err := manager.Registry.Load("forked"); !errors.Is(err, state.ErrWorkspaceNotFound) {
		t.Fatalf("failed fork published a workspace with no retained root: %v", err)
	}
}

func TestRootGCReadsWorkspaceProtectionsAfterLockWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager, checkpoint := forkCheckpointFixture(t)
	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(ctx, "original", true, true); err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, checkpoint.Ref.Name); err != nil {
		t.Fatal(err)
	}
	destination, err := manager.Create(ctx, CreateSpec{Name: "destination", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	workspaceLock, err := manager.Native.LockWorkspace(ctx, destination.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceLock.Release()
	rootLock, err := store.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { _ = rootLock.Release() }) }
	defer release()
	gcCtx := &metadataWaitContext{Context: ctx, waiting: make(chan struct{})}
	var gc speculative.GCResult
	var gcErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		gc, gcErr = manager.RootGC(gcCtx, 0, false)
	}()
	t.Cleanup(func() { release(); cancel(); <-done })
	select {
	case <-gcCtx.waiting:
	case <-done:
		t.Fatalf("GC did not wait for root publication: %#v, %v", gc, gcErr)
	case <-ctx.Done():
		t.Fatal("GC did not reach the held speculative lock")
	}
	// This is the final publication step of a fork/checkpoint transaction.
	// Both the named workspace and root lock remain held across the write.
	destination.SpeculativeRoot = checkpoint.Root.String()
	if err := manager.Registry.Save(destination); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if gcErr != nil || slices.Contains(gc.ReleasedRoots, checkpoint.Root) || !slices.Contains(gc.Protected, checkpoint.Root) {
		t.Fatalf("GC used workspace protections from before its lock wait: %#v, %v", gc, gcErr)
	}
	assertPublishedRoot(t, manager, destination.Name, checkpoint.Root)
}

func TestCheckpointProtectsRootThroughWorkspacePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manager, _ := forkCheckpointFixture(t)
	original, err := manager.Registry.Load("original")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original.Path, "tracked.txt"), []byte("next checkpoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := 0
	runner := &metadataBarrierRunner{
		entered: make(chan struct{}), resume: make(chan struct{}),
		match: func(command execx.Command) bool {
			if command.Name == "git" && slices.Contains(command.Args, "worktree") && slices.Contains(command.Args, "list") {
				checks++
				return checks == 2
			}
			return false
		},
	}
	p, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
	var checkpoint CheckpointResult
	var checkpointErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		checkpoint, checkpointErr = New(p).Checkpoint(ctx, original.Name, CheckpointOptions{RefName: "next"})
	}()
	t.Cleanup(func() { release(); cancel(); <-done })
	select {
	case <-runner.entered:
	case <-done:
		t.Fatalf("checkpoint finished before metadata publication: %v", checkpointErr)
	case <-ctx.Done():
		t.Fatal("checkpoint did not reach its final ownership check")
	}
	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	dropCtx, cancelDrop := context.WithTimeout(ctx, 200*time.Millisecond)
	dropErr := store.DropRef(dropCtx, "next")
	cancelDrop()
	gcCtx, cancelGC := context.WithTimeout(ctx, 200*time.Millisecond)
	_, gcErr := manager.RootGC(gcCtx, 0, false)
	cancelGC()
	release()
	<-done
	if checkpointErr != nil {
		t.Fatalf("checkpoint failed: %v", checkpointErr)
	}
	assertPublishedRoot(t, manager, original.Name, checkpoint.Root)
	if !errors.Is(dropErr, context.DeadlineExceeded) || !errors.Is(gcErr, context.DeadlineExceeded) {
		t.Fatalf("root publication did not exclude ref mutation/GC: drop=%v gc=%v", dropErr, gcErr)
	}
}

func assertPublishedRoot(t *testing.T, manager *Manager, name string, id speculative.ID) {
	t.Helper()
	value, err := manager.Registry.Load(name)
	if err != nil || value.SpeculativeRoot != id.String() {
		t.Fatalf("workspace did not publish the expected root: %#v, %v", value, err)
	}
	ref := "refs/cambium/speculative/roots/" + id.String()
	resolved, err := manager.Project.Repository.ResolveCommit(context.Background(), ref)
	if err != nil || resolved != id.String() {
		t.Fatalf("successful workspace publication points at a collected root: metadata=%s Git ref=%s error=%v", value.SpeculativeRoot, resolved, err)
	}
}
