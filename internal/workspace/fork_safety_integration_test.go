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
	"github.com/nbardy/cambium/internal/state"
)

func TestForkCannotDeleteConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	manager, checkpoint := forkCheckpointFixture(t)
	runner := &forkRestoreBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	forkProject, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	forkManager := New(forkProject)
	done := make(chan error, 1)
	go func() {
		_, err := forkManager.Fork(ctx, checkpoint.Root.String(), CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
		done <- err
	}()
	t.Cleanup(func() { runner.release() })
	select {
	case <-runner.entered:
	case err := <-done:
		t.Fatalf("fork returned before restoring: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not reach restore")
	}
	created, err := manager.Registry.Load("forked")
	if err != nil {
		t.Fatal(err)
	}
	removeCtx, cancel := context.WithTimeout(ctx, time.Second)
	removeErr := manager.Remove(removeCtx, created.Name, true, true)
	cancel()
	if removeErr == nil {
		// The old implementation allowed replacement during Restore. Its error
		// rollback then removed this unrelated replacement by name.
		replacement, err := manager.Create(ctx, CreateSpec{Name: created.Name, Materializer: model.MaterializerGit})
		if err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(replacement.Path, "replacement-local.txt")
		if err := os.WriteFile(sentinel, []byte("must survive\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runner.release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("fork did not finish after restore resumed")
		}
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("fork rollback deleted a concurrent replacement's local work: %v", err)
		}
		t.Fatal("removal was allowed while fork restoration was still active")
	}
	if !errors.Is(removeErr, context.DeadlineExceeded) {
		t.Fatalf("concurrent removal returned %v, want a lock wait deadline", removeErr)
	}
	runner.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not finish after restore resumed")
	}
	current, err := manager.Registry.Load(created.Name)
	if err != nil || current.ID != created.ID || current.SpeculativeRoot != checkpoint.Root.String() {
		t.Fatalf("fork metadata was lost or replaced: %#v, %v", current, err)
	}
	assertFile(t, filepath.Join(current.Path, "tracked.txt"), "checkpoint state\n")
}

func TestFailedForkPreservesAdvancedBranch(t *testing.T) {
	ctx := context.Background()
	manager, checkpoint := forkCheckpointFixture(t)
	runner := &forkRestoreBarrier{entered: make(chan struct{}), resume: make(chan struct{}), fail: true}
	forkProject, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := New(forkProject).Fork(ctx, checkpoint.Root.String(), CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
		done <- err
	}()
	t.Cleanup(func() { runner.release() })
	select {
	case <-runner.entered:
	case err := <-done:
		t.Fatalf("fork returned before restoring: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not reach restore")
	}
	created, err := manager.Registry.Load("forked")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "tracked.txt"), []byte("new committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, created.Path, "commit", "-am", "user commit while fork is active")
	advanced, err := manager.Project.Repository.ResolveCommitAt(ctx, created.Path, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runner.release()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("injected restore failure did not fail the fork")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not finish after restore failed")
	}
	if head, err := manager.Project.Repository.BranchCommit(ctx, created.Branch); err != nil || head != advanced {
		t.Fatalf("fork rollback deleted an advanced branch: head=%s, want %s, error=%v", head, advanced, err)
	}
	current, err := manager.Registry.Load(created.Name)
	if err != nil || current.ID != created.ID {
		t.Fatalf("failed fork lost metadata for the protected workspace: %#v, %v", current, err)
	}
	assertFile(t, filepath.Join(created.Path, "tracked.txt"), "new committed work\n")
}

func TestFailedForkCleansUpItsOwnWorkspace(t *testing.T) {
	ctx := context.Background()
	manager, checkpoint := forkCheckpointFixture(t)
	runner := &forkRestoreBarrier{entered: make(chan struct{}), resume: make(chan struct{}), fail: true}
	runner.release()
	forkProject, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(canonicalTempDir(t), "failed-fork")
	_, err = New(forkProject).Fork(ctx, checkpoint.Root.String(), CreateSpec{
		Name: "failed", Branch: "failed-fork", Path: path, Materializer: model.MaterializerGit,
	})
	if err == nil {
		t.Fatal("injected restore failure did not fail the fork")
	}
	if _, err := manager.Registry.Load("failed"); !errors.Is(err, state.ErrWorkspaceNotFound) {
		t.Fatalf("failed fork retained its own metadata: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed fork retained its own worktree: %v", err)
	}
	if exists, err := manager.Project.Repository.BranchExists(ctx, "failed-fork"); err != nil || exists {
		t.Fatalf("failed fork retained its original branch: %t, %v", exists, err)
	}
	if records, err := manager.Native.Journal.List(); err != nil || len(records) != 0 {
		t.Fatalf("failed fork left an unfinished operation: %#v, %v", records, err)
	}
}

func TestForkRejectsGitBranchRenameDuringRestore(t *testing.T) {
	ctx := context.Background()
	manager, checkpoint := forkCheckpointFixture(t)
	runner := &forkRestoreBarrier{entered: make(chan struct{}), resume: make(chan struct{})}
	forkProject, err := project.Open(ctx, manager.Project.Repository.Root, runner)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := New(forkProject).Fork(ctx, checkpoint.Root.String(), CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
		done <- err
	}()
	t.Cleanup(func() { runner.release() })
	select {
	case <-runner.entered:
	case err := <-done:
		t.Fatalf("fork returned before restoring: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not reach restore")
	}
	created, err := manager.Registry.Load("forked")
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary Git does not use Cambium's lifecycle locks. Detect the changed
	// registration before publishing a successful workspace observation.
	runGitTest(t, created.Path, "branch", "-m", "renamed-during-restore")
	runner.release()
	select {
	case err := <-done:
		if !errors.Is(err, native.ErrWorktreeOwnership) {
			t.Fatalf("fork did not reject changed ownership before publication: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fork did not finish after restore resumed")
	}
	current, err := manager.Registry.Load(created.Name)
	if err != nil || current.ID != created.ID || current.SpeculativeRoot != "" {
		t.Fatalf("fork published or removed stale metadata: %#v, %v", current, err)
	}
	if exists, err := manager.Project.Repository.BranchExists(ctx, "renamed-during-restore"); err != nil || !exists {
		t.Fatalf("fork cleanup lost the renamed branch: %t, %v", exists, err)
	}
	assertFile(t, filepath.Join(created.Path, "tracked.txt"), "checkpoint state\n")
}

func TestFailedForkCleanupProtectsReplacement(t *testing.T) {
	ctx := context.Background()
	manager, _ := forkCheckpointFixture(t)
	created, err := manager.Create(ctx, CreateSpec{Name: "reused", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(ctx, created.Name, true, true); err != nil {
		t.Fatal(err)
	}
	replacement, err := manager.Create(ctx, CreateSpec{Name: created.Name, Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(replacement.Path, "local.txt")
	if err := os.WriteFile(sentinel, []byte("replacement work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manager.removeFailedFork(ctx, created); err == nil {
		t.Fatal("cleanup accepted the identity of an old workspace")
	}
	current, err := manager.Registry.Load(created.Name)
	if err != nil || current.ID != replacement.ID {
		t.Fatalf("cleanup changed replacement metadata: %#v, %v", current, err)
	}
	assertFile(t, sentinel, "replacement work\n")
}

func forkCheckpointFixture(t *testing.T) (*Manager, CheckpointResult) {
	t.Helper()
	ctx := context.Background()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	original, err := manager.Create(ctx, CreateSpec{Name: "original", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original.Path, "tracked.txt"), []byte("checkpoint state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := manager.Checkpoint(ctx, original.Name, CheckpointOptions{RefName: "fork-source"})
	if err != nil {
		t.Fatal(err)
	}
	return manager, checkpoint
}

type forkRestoreBarrier struct {
	execx.OSRunner
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
	fail    bool
}

func (r *forkRestoreBarrier) release() { r.once.Do(func() { close(r.resume) }) }

func (r *forkRestoreBarrier) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	if command.Name == "git" && slices.Contains(command.Args, "restore") {
		close(r.entered)
		select {
		case <-r.resume:
		case <-ctx.Done():
			return execx.Result{}, ctx.Err()
		}
		if r.fail {
			return execx.Result{}, errors.New("injected restore failure")
		}
	}
	return r.OSRunner.Run(ctx, command)
}
