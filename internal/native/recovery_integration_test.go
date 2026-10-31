package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/operation"
	"github.com/nbardy/cambium/internal/project"
)

func TestCreateFromLinkedWorktreeUsesItsOwnHEAD(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	feature := filepath.Join(canonicalTempDir(t), "feature")
	git(t, root, "worktree", "add", "-b", "feature", feature)
	mustWrite(t, filepath.Join(feature, "src", "a.txt"), "feature-only\n", 0o644)
	git(t, feature, "commit", "-am", "feature change")
	featureHEAD := strings.TrimSpace(git(t, feature, "rev-parse", "HEAD"))
	backend := openBackend(t, ctx, feature)
	child, err := backend.Create(ctx, native.CreateSpec{Name: "child", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if child.BaseCommit != featureHEAD {
		t.Fatalf("child base = %s, want invoking worktree HEAD %s", child.BaseCommit, featureHEAD)
	}
	if content := mustRead(t, filepath.Join(child.Path, "src", "a.txt")); content != "feature-only\n" {
		t.Fatalf("child contains the wrong revision: %q", content)
	}
	if err := backend.Remove(ctx, child.Name, true, true); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveFromInsideWorkspaceAlsoDeletesItsBranch(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	primary := openBackend(t, ctx, root)
	workspace, err := primary.Create(ctx, native.CreateSpec{Name: "self-remove", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	// Opening the backend in the linked worktree is the CLI's context when a
	// user runs cambium remove from that worktree. Its source cwd disappears
	// before branch deletion, while the common Git directory remains valid.
	inside := openBackend(t, ctx, workspace.Path)
	if err := inside.Remove(ctx, workspace.Name, false, true); err != nil {
		t.Fatalf("remove from the workspace failed: %v", err)
	}
	if _, err := os.Stat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace path survived removal: %v", err)
	}
	if exists, err := primary.Project.Repository.BranchExists(ctx, workspace.Branch); err != nil || exists {
		t.Fatalf("workspace branch survived removal: exists=%t, error=%v", exists, err)
	}
	if records, err := primary.Journal.List(); err != nil || len(records) != 0 {
		t.Fatalf("removal left a stale operation: %#v, %v", records, err)
	}
}

// pauseRunner gates one actual Git invocation. Everything, including the
// paused invocation after release, runs against a real temporary repository.
type pauseRunner struct {
	execx.OSRunner
	match   func(execx.Command) bool
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *pauseRunner) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	if r.match(command) {
		r.once.Do(func() { close(r.entered) })
		select {
		case <-r.resume:
		case <-ctx.Done():
			return execx.Result{}, ctx.Err()
		}
	}
	return r.OSRunner.Run(ctx, command)
}

func newPausedBackend(t *testing.T, root string, match func(execx.Command) bool) (*native.Backend, *pauseRunner, func()) {
	t.Helper()
	runner := &pauseRunner{match: match, entered: make(chan struct{}), resume: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(runner.resume) }) }
	t.Cleanup(release)
	p, err := project.Open(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	return native.New(p), runner, release
}

func waitForPause(t *testing.T, runner *pauseRunner) {
	t.Helper()
	select {
	case <-runner.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach its controlled pause")
	}
}

func requireActiveRecovery(t *testing.T, backend *native.Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	actions, err := backend.Recover(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Status != "active" || actions[0].Action != "skip" {
		t.Fatalf("live operation was not skipped: %#v", actions)
	}
}

func TestRecoverySkipsActiveCreateAfterGitRegistration(t *testing.T) {
	requireGit(t)
	root := initializeRepository(t, false)
	path := filepath.Join(canonicalTempDir(t), "agent")
	backend, runner, release := newPausedBackend(t, root, func(command execx.Command) bool {
		return command.Dir == path && strings.Contains(strings.Join(command.Args, " "), "status --porcelain")
	})
	result := make(chan error, 1)
	go func() {
		_, err := backend.Create(context.Background(), native.CreateSpec{Name: "live", Path: path, Materializer: model.MaterializerGit})
		result <- err
	}()
	waitForPause(t, runner)
	requireActiveRecovery(t, backend)
	if content := mustRead(t, filepath.Join(path, "src", "a.txt")); content != "alpha\n" {
		t.Fatalf("recovery changed active workspace: %q", content)
	}
	release()
	if err := <-result; err != nil {
		t.Fatalf("active creation failed after recovery: %v", err)
	}
	if err := backend.Remove(context.Background(), "live", true, true); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverySkipsActiveRemovalWithoutLockInversion(t *testing.T) {
	requireGit(t)
	root := initializeRepository(t, false)
	setup := openBackend(t, context.Background(), root)
	workspace, err := setup.Create(context.Background(), native.CreateSpec{Name: "removing", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	backend, runner, release := newPausedBackend(t, root, func(command execx.Command) bool {
		return strings.Contains(strings.Join(command.Args, " "), "worktree remove")
	})
	result := make(chan error, 1)
	go func() { result <- backend.Remove(context.Background(), workspace.Name, true, true) }()
	waitForPause(t, runner)
	requireActiveRecovery(t, backend)
	if _, err := os.Stat(workspace.Path); err != nil {
		t.Fatalf("recovery touched active removal: %v", err)
	}
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if records, err := backend.Journal.List(); err != nil || len(records) != 0 {
		t.Fatalf("completed removal left operations: %#v, %v", records, err)
	}
}

func TestRecoveryPreservesUncommittedAndUntrackedWork(t *testing.T) {
	for _, kind := range []string{"tracked", "untracked", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			requireGit(t)
			ctx := context.Background()
			root := initializeRepository(t, false)
			backend := openBackend(t, ctx, root)
			t.Setenv("CAMBIUM_FAILPOINT", "after-materialize")
			_, err := backend.Create(ctx, native.CreateSpec{Name: "interrupted", Materializer: model.MaterializerGit})
			if !errors.Is(err, failpoint.ErrInjectedCrash) {
				t.Fatalf("expected interruption, got %v", err)
			}
			t.Setenv("CAMBIUM_FAILPOINT", "")
			records, err := backend.Journal.List()
			if err != nil || len(records) != 1 {
				t.Fatalf("expected operation record, got %#v, %v", records, err)
			}
			path := filepath.Join(records[0].Path, "new-work.txt")
			if kind == "tracked" {
				path = filepath.Join(records[0].Path, "src", "a.txt")
			} else if kind == "ignored" {
				path = filepath.Join(records[0].Path, "scratch", "important.txt")
			}
			mustWrite(t, path, "keep this work\n", 0o644)
			actions, err := backend.Recover(ctx, false)
			if err != nil || len(actions) != 1 || actions[0].Status != "blocked" {
				t.Fatalf("work was not protected: %#v, %v", actions, err)
			}
			if content := mustRead(t, path); content != "keep this work\n" {
				t.Fatalf("recovery discarded %s work: %q", kind, content)
			}
			if exists, err := backend.Project.Repository.BranchExists(ctx, records[0].Branch); err != nil || !exists {
				t.Fatalf("recovery removed its branch: %v", err)
			}
		})
	}
}

func TestRecoveryNeverDeletesAnUnclaimedPlannedPath(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	backend := openBackend(t, ctx, root)
	path := canonicalTempDir(t)
	mustWrite(t, filepath.Join(path, "unrelated.txt"), "not Cambium's data\n", 0o644)
	commit := strings.TrimSpace(git(t, root, "rev-parse", "HEAD"))
	git(t, root, "branch", "cambium/planned")
	_, err := backend.Journal.Begin(operation.KindCreate, "planned", "cambium/planned", path, commit)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := backend.Recover(ctx, false)
	if err != nil || len(actions) != 1 || actions[0].Status != "blocked" {
		t.Fatalf("unclaimed path was not protected: %#v, %v", actions, err)
	}
	if content := mustRead(t, filepath.Join(path, "unrelated.txt")); content != "not Cambium's data\n" {
		t.Fatalf("unclaimed path changed: %q", content)
	}
	if exists, err := backend.Project.Repository.BranchExists(ctx, "cambium/planned"); err != nil || !exists {
		t.Fatalf("unclaimed branch was deleted: %v", err)
	}
}

func TestRecoveryPreservesReplacementRegistrationAtSamePath(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	backend := openBackend(t, ctx, root)
	t.Setenv("CAMBIUM_FAILPOINT", "after-register")
	_, err := backend.Create(ctx, native.CreateSpec{Name: "reused", Materializer: model.MaterializerGit})
	if !errors.Is(err, failpoint.ErrInjectedCrash) {
		t.Fatal(err)
	}
	t.Setenv("CAMBIUM_FAILPOINT", "")
	records, err := backend.Journal.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("expected operation record: %#v, %v", records, err)
	}
	record := records[0]
	git(t, root, "worktree", "remove", "--force", record.Path)
	git(t, root, "worktree", "add", record.Path, record.Branch)
	actions, err := backend.Recover(ctx, false)
	if err != nil || len(actions) != 1 || actions[0].Status != "blocked" {
		t.Fatalf("replacement worktree was not protected: %#v, %v", actions, err)
	}
	if registered, err := backend.Project.Repository.IsWorktreeRegistered(ctx, record.Path); err != nil || !registered {
		t.Fatalf("replacement registration was deleted: %v", err)
	}
}

func TestRecoveryFinishesOwnedInterruptedRemoval(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "unchanged"
		if advanced {
			name = "advanced"
		}
		t.Run(name, func(t *testing.T) {
			requireGit(t)
			ctx := context.Background()
			root := initializeRepository(t, false)
			backend := openBackend(t, ctx, root)
			workspace, err := backend.Create(ctx, native.CreateSpec{Name: name, Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			if advanced {
				mustWrite(t, filepath.Join(workspace.Path, "src", "a.txt"), "committed work\n", 0o644)
				git(t, workspace.Path, "commit", "-am", "valuable work")
			}
			t.Setenv("CAMBIUM_FAILPOINT", "after-worktree-remove")
			if err := backend.Remove(ctx, name, true, true); !errors.Is(err, failpoint.ErrInjectedCrash) {
				t.Fatalf("expected interrupted removal, got %v", err)
			}
			t.Setenv("CAMBIUM_FAILPOINT", "")
			actions, err := backend.Recover(ctx, false)
			if err != nil || len(actions) != 1 {
				t.Fatalf("recover removal: %#v, %v", actions, err)
			}
			if advanced {
				if actions[0].Status != "blocked" {
					t.Fatalf("advanced branch was not protected: %#v", actions)
				}
			} else if actions[0].Action != "finish-remove" || actions[0].Status != "resolved" {
				t.Fatalf("owned removal was not completed: %#v", actions)
			}
			if exists, err := backend.Project.Repository.BranchExists(ctx, workspace.Branch); err != nil || exists != advanced {
				t.Fatalf("branch exists = %t, want %t: %v", exists, advanced, err)
			}
		})
	}
}

func TestRecoveryRereadsJournalAfterTakingLocks(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	backend := openBackend(t, ctx, root)
	commit := strings.TrimSpace(git(t, root, "rev-parse", "HEAD"))
	record, err := backend.Journal.Begin(operation.KindCreate, "finished", "cambium/finished", filepath.Join(canonicalTempDir(t), "absent"), commit)
	if err != nil {
		t.Fatal(err)
	}
	gitLock, err := lockfile.Acquire(filepath.Join(backend.Project.LocksDir(), "git-admin.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer gitLock.Release()
	result := make(chan error, 1)
	go func() {
		actions, err := backend.Recover(ctx, false)
		if err == nil && len(actions) != 0 {
			err = errors.New("recovery processed an operation that was already removed")
		}
		result <- err
	}()
	workspaceLockPath := filepath.Join(backend.Project.LocksDir(), "workspace-finished.lock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(workspaceLockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery did not take its workspace lock")
		}
		time.Sleep(time.Millisecond)
	}
	// Emulate the operation disappearing after the initial snapshot. Recovery
	// must re-read under its locks rather than act on the stale list entry.
	if err := backend.Journal.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := gitLock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
