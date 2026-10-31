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

// Each pause follows a real subprocess, after the operation has read its
// workspace metadata. A removal used to finish here and the resumed operation
// could publish the old record over the removal or a same-name replacement.
func TestWorkspaceMetadataTransactionsExcludeRemoval(t *testing.T) {
	for _, operation := range []string{"refresh", "validate", "capture", "checkpoint"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			repo, manager, value := metadataLifecycleFixture(t)
			runner := &metadataBarrierRunner{entered: make(chan struct{}), resume: make(chan struct{})}
			switch operation {
			case "refresh":
				runner.match = func(command execx.Command) bool {
					return command.Name == "git" && slices.Contains(command.Args, "--porcelain") && slices.Contains(command.Args, "list")
				}
			case "validate":
				// A failed validator must record an unready observation, but it
				// must not recreate metadata after removal while the hook ran.
				writeReadinessFile(t, value.Path, "deps/state", "wrong\n")
				runner.match = func(command execx.Command) bool {
					return command.Name == "cmp" && command.Dir == value.Path
				}
			case "capture":
				writeReadinessFile(t, value.Path, "deps.lock", "v2\n")
				writeReadinessFile(t, value.Path, "deps/state", "v2\n")
				commitReadinessFiles(t, value.Path)
				runner.match = func(command execx.Command) bool {
					return command.Name == "cp" && len(command.Args) > 1 &&
						filepath.Clean(command.Args[len(command.Args)-2]) == filepath.Join(value.Path, "deps")
				}
			case "checkpoint":
				writeReadinessFile(t, value.Path, "tracked.txt", "checkpoint contents\n")
				publications := 0
				runner.match = func(command execx.Command) bool {
					if command.Name == "git" && slices.Contains(command.Args, "cambium speculative checkpoint") {
						publications++
						return publications == 2 // the ref now includes environment receipts
					}
					return false
				}
			}
			p, err := project.Open(ctx, repo, runner)
			if err != nil {
				t.Fatal(err)
			}
			writer := New(p)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "refresh":
					_, err = writer.Load(ctx, value.Name, true)
				case "validate":
					_, err = writer.ValidateEnvironment(ctx, value.Name)
				case "capture":
					_, err = writer.CaptureEnvironment(ctx, value.Name, "")
				case "checkpoint":
					_, err = writer.Checkpoint(ctx, value.Name, CheckpointOptions{})
				}
				done <- err
			}()
			t.Cleanup(func() { release(); cancel() })
			select {
			case <-runner.entered:
			case err := <-done:
				t.Fatalf("%s finished before the publication pause: %v", operation, err)
			case <-ctx.Done():
				t.Fatal("operation did not reach the publication pause")
			}

			removeCtx, removeCancel := context.WithTimeout(ctx, 150*time.Millisecond)
			removeErr := manager.Remove(removeCtx, value.Name, true, true)
			removeCancel()
			release()
			var operationErr error
			select {
			case operationErr = <-done:
			case <-ctx.Done():
				t.Fatal("operation did not finish after release")
			}
			if !errors.Is(removeErr, context.DeadlineExceeded) {
				exists, _ := manager.Registry.Exists(value.Name)
				t.Fatalf("%s allowed removal during publication: removal=%v operation=%v metadata resurrected=%t", operation, removeErr, operationErr, removeErr == nil && exists)
			}
			if (operation == "validate") != (operationErr != nil) {
				t.Fatalf("unexpected %s result after release: %v", operation, operationErr)
			}
			if err := manager.Remove(ctx, value.Name, true, true); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Registry.Load(value.Name); !errors.Is(err, state.ErrWorkspaceNotFound) {
				t.Fatalf("completed removal retained workspace metadata: %v", err)
			}
			if _, err := os.Stat(value.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed removal retained the worktree: %v", err)
			}
		})
	}
}

func TestGCRechecksSelectionAfterWaiting(t *testing.T) {
	for _, change := range []string{"touched", "retained", "replacement"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			repo := initSpecRepo(t)
			p, err := project.Open(ctx, repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(p)
			first, err := manager.Create(ctx, CreateSpec{Name: "first", Ephemeral: true, Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			second, err := manager.Create(ctx, CreateSpec{Name: "second", Ephemeral: true, Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-48 * time.Hour)
			first.CreatedAt, first.LastUsedAt = old.Add(-time.Hour), old
			second.CreatedAt, second.LastUsedAt = old, old
			for _, value := range []model.Workspace{first, second} {
				if err := manager.Registry.Save(value); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := manager.Native.LockWorkspace(ctx, first.Name)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Release()
			gcCtx := &metadataWaitContext{Context: ctx, waiting: make(chan struct{})}
			options := GCOptions{EphemeralOnly: true, OlderThan: 24 * time.Hour, Force: true, DeleteBranches: true}
			if change == "replacement" {
				options.OlderThan = 0 // replacement is otherwise still eligible
			}
			type outcome struct {
				result GCResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := manager.GC(gcCtx, options)
				done <- outcome{result, err}
			}()
			// The first removal observes cancellation only when it waits for
			// our held name lock. GC has already taken its registry snapshot.
			select {
			case <-gcCtx.waiting:
			case <-ctx.Done():
				t.Fatal("GC did not reach its first workspace lock")
			}
			switch change {
			case "touched":
				if err := manager.Touch(second.Name); err != nil {
					t.Fatal(err)
				}
			case "retained":
				secondLock, err := manager.Native.LockWorkspace(ctx, second.Name)
				if err != nil {
					t.Fatal(err)
				}
				second.Ephemeral = false
				err = manager.Registry.Save(second)
				_ = secondLock.Release()
				if err != nil {
					t.Fatal(err)
				}
			case "replacement":
				if err := manager.Remove(ctx, second.Name, true, true); err != nil {
					t.Fatal(err)
				}
				replacement, err := manager.Create(ctx, CreateSpec{Name: second.Name, Ephemeral: true, Materializer: model.MaterializerGit})
				if err != nil || replacement.ID == second.ID {
					t.Fatalf("cannot create a distinct replacement: %#v, %v", replacement, err)
				}
				second = replacement
				writeReadinessFile(t, second.Path, "replacement-work.txt", "must survive stale GC\n")
			}
			if err := lock.Release(); err != nil {
				t.Fatal(err)
			}
			var got outcome
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("GC did not finish after release")
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if !slices.Equal(got.result.Removed, []string{first.Name}) || len(got.result.Skipped) != 1 || got.result.Skipped[0].Name != second.Name {
				t.Fatalf("GC removed workspace after its %s selection changed: %#v", change, got.result)
			}
			current, err := manager.Registry.Load(second.Name)
			if err != nil || current.ID != second.ID {
				t.Fatalf("GC lost the current workspace: %#v, %v", current, err)
			}
			if change == "replacement" {
				assertFile(t, filepath.Join(second.Path, "replacement-work.txt"), "must survive stale GC\n")
			}
		})
	}
}

func TestTouchRereadsAfterRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	repo, manager, value := metadataLifecycleFixture(t)
	runner := &metadataBarrierRunner{
		entered: make(chan struct{}), resume: make(chan struct{}),
		match: func(command execx.Command) bool {
			return command.Name == "git" && slices.Contains(command.Args, "worktree") && slices.Contains(command.Args, "remove")
		},
	}
	p, err := project.Open(ctx, repo, runner)
	if err != nil {
		t.Fatal(err)
	}
	remover := New(p)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
	t.Cleanup(release)
	removeDone := make(chan error, 1)
	go func() { removeDone <- remover.Remove(ctx, value.Name, true, true) }()
	select {
	case <-runner.entered:
	case err := <-removeDone:
		t.Fatalf("removal finished before its pause: %v", err)
	case <-ctx.Done():
		t.Fatal("removal did not reach its publication pause")
	}
	touchCtx := &metadataWaitContext{Context: ctx, waiting: make(chan struct{})}
	touchDone := make(chan error, 1)
	go func() { touchDone <- manager.TouchIf(touchCtx, value.Name, value.ID) }()
	select {
	case <-touchCtx.waiting:
	case err := <-touchDone:
		t.Fatalf("touch did not wait for the removing workspace: %v", err)
	case <-ctx.Done():
		t.Fatal("touch did not reach the held lifecycle lock")
	}
	release()
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-touchDone; !errors.Is(err, state.ErrWorkspaceNotFound) {
		t.Fatalf("touch reused metadata read before removal: %v", err)
	}
	if _, err := manager.Registry.Load(value.Name); !errors.Is(err, state.ErrWorkspaceNotFound) {
		t.Fatalf("touch resurrected removed metadata: %v", err)
	}
}

func TestTouchRejectsSameNameReplacement(t *testing.T) {
	ctx := context.Background()
	_, manager, value := metadataLifecycleFixture(t)
	if err := manager.Remove(ctx, value.Name, true, true); err != nil {
		t.Fatal(err)
	}
	replacement, err := manager.Create(ctx, CreateSpec{Name: value.Name, Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.TouchIf(ctx, value.Name, value.ID); err == nil {
		t.Fatal("use of the original workspace touched its replacement")
	}
	current, err := manager.Registry.Load(value.Name)
	if err != nil || current.ID != replacement.ID || !current.LastUsedAt.Equal(replacement.LastUsedAt) {
		t.Fatalf("touch overwrote replacement metadata: %#v, %v", current, err)
	}
}

func TestValidationRejectsOwnershipChangeBeforePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	repo, manager, value := metadataLifecycleFixture(t)
	writeReadinessFile(t, value.Path, "deps/state", "wrong\n")
	runner := &metadataBarrierRunner{
		entered: make(chan struct{}), resume: make(chan struct{}),
		match: func(command execx.Command) bool {
			return command.Name == "cmp" && command.Dir == value.Path
		},
	}
	p, err := project.Open(ctx, repo, runner)
	if err != nil {
		t.Fatal(err)
	}
	writer := New(p)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := writer.ValidateEnvironment(ctx, value.Name)
		done <- err
	}()
	select {
	case <-runner.entered:
	case err := <-done:
		t.Fatalf("validation finished before its pause: %v", err)
	case <-ctx.Done():
		t.Fatal("validation did not reach the publication pause")
	}
	// Ordinary Git does not take Cambium's lifecycle lock. Its change must
	// invalidate this observation rather than being saved as if nothing moved.
	runGitTest(t, value.Path, "branch", "-m", "renamed-outside-cambium")
	release()
	if err := <-done; !errors.Is(err, native.ErrWorktreeOwnership) {
		t.Fatalf("validation accepted a changed branch identity: %v", err)
	}
	current, err := manager.Registry.Load(value.Name)
	if err != nil || !current.EnvironmentReady || !current.LastUsedAt.Equal(value.LastUsedAt) {
		t.Fatalf("rejected observation overwrote metadata: %#v, %v", current, err)
	}
	current, err = manager.Load(ctx, value.Name, true)
	if err != nil || current.Status != model.WorkspaceBroken {
		t.Fatalf("refresh treated a changed branch as the owned workspace: %#v, %v", current, err)
	}
	if _, err := manager.CaptureEnvironment(ctx, value.Name, ""); err == nil {
		t.Fatal("capture accepted a workspace with unverified ownership")
	}
	if _, err := manager.Checkpoint(ctx, value.Name, CheckpointOptions{}); err == nil {
		t.Fatal("checkpoint accepted a workspace with unverified ownership")
	}
}

func metadataLifecycleFixture(t *testing.T) (string, *Manager, model.Workspace) {
	t.Helper()
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, ".gitignore", "deps/\n")
	writeReadinessFile(t, repo, "deps.lock", "v1\n")
	writeReadinessFile(t, repo, "deps/state", "v1\n")
	writeReadinessFile(t, repo, ".cambium.toml", `version = 1
[[path]]
path = "deps"
policy = "clone"
inputs = ["deps.lock"]
required = true
validate = ["cmp", "deps.lock", "deps/state"]
`)
	commitReadinessFiles(t, repo)
	manager := readinessManager(t, repo, true)
	value, err := manager.Create(context.Background(), CreateSpec{Name: "subject", Materializer: model.MaterializerGit})
	if err != nil || !value.EnvironmentReady {
		t.Fatalf("cannot create validated workspace: %#v, %v", value, err)
	}
	return repo, manager, value
}

type metadataBarrierRunner struct {
	execx.OSRunner
	match   func(execx.Command) bool
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *metadataBarrierRunner) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	result, err := r.OSRunner.Run(ctx, command)
	if r.match != nil && r.match(command) && (err == nil || command.Name == "cmp") {
		r.once.Do(func() {
			close(r.entered)
			select {
			case <-r.resume:
			case <-ctx.Done():
			}
		})
	}
	return result, err
}

type metadataWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *metadataWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}
