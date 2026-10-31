package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

func TestReconcileFromLinkedCheckoutReportsActualUnmanagedWorktrees(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	parent := canonicalTempDir(t)
	invoking := filepath.Join(parent, "invoking")
	other := filepath.Join(parent, "other")
	runGitTest(t, repo, "worktree", "add", "-b", "invoking", invoking, "main")
	runGitTest(t, repo, "worktree", "add", "-b", "other", other, "main")
	p, err := project.Open(ctx, invoking, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	want := []string{invoking, other}
	slices.Sort(want)
	for _, dryRun := range []bool{true, false} {
		result, err := manager.Reconcile(ctx, dryRun)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(result.Unmanaged, want) {
			t.Fatalf("dryRun=%t: unmanaged=%#v, want linked checkouts %#v", dryRun, result.Unmanaged, want)
		}
	}
}

// The pause retains a real Git discovery result while an ordinary concurrent
// removal completes. Reconcile must never publish that stale registry snapshot.
type reconcileDiscoveryPause struct {
	execx.OSRunner
	once     sync.Once
	captured chan struct{}
	resume   chan struct{}
}

func (r *reconcileDiscoveryPause) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	result, err := r.OSRunner.Run(ctx, command)
	if err != nil || command.Name != "git" || !strings.Contains(strings.Join(command.Args, " "), "worktree list --porcelain") {
		return result, err
	}
	pause := false
	r.once.Do(func() {
		pause = true
		close(r.captured)
	})
	if pause {
		select {
		case <-r.resume:
		case <-ctx.Done():
			return execx.Result{}, ctx.Err()
		}
	}
	return result, nil
}

func TestReconcilePreservesConcurrentRemovalAndReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "removed"
		if replace {
			name = "replaced"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			repo := initSpecRepo(t)
			p, err := project.Open(ctx, repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(p)
			value, err := manager.Create(ctx, CreateSpec{Name: "agent", Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			value.Status = model.WorkspaceBroken
			if err := manager.Registry.Save(value); err != nil {
				t.Fatal(err)
			}
			runner := &reconcileDiscoveryPause{captured: make(chan struct{}), resume: make(chan struct{})}
			var once sync.Once
			resume := func() { once.Do(func() { close(runner.resume) }) }
			defer resume()
			pausedProject, err := project.Open(ctx, repo, runner)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result ReconcileResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := New(pausedProject).Reconcile(ctx, false)
				done <- outcome{result, err}
			}()
			select {
			case <-runner.captured:
			case <-ctx.Done():
				t.Fatal("reconcile did not reach discovery pause")
			}
			if err := manager.Remove(ctx, value.Name, false, true); err != nil {
				t.Fatalf("concurrent removal failed: %v", err)
			}
			var replacement model.Workspace
			if replace {
				if _, err := manager.Create(ctx, CreateSpec{Name: value.Name, Materializer: model.MaterializerGit}); err != nil {
					t.Fatalf("concurrent replacement failed: %v", err)
				}
				replacement, err = manager.Registry.Load(value.Name)
				if err != nil || replacement.ID == value.ID {
					t.Fatalf("replacement did not get its own identity: %#v, %v", replacement, err)
				}
			}
			resume()
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			if replace {
				actual, err := manager.Registry.Load(value.Name)
				if err != nil || !reflect.DeepEqual(actual, replacement) {
					t.Fatalf("reconcile overwrote replacement metadata: actual=%#v want=%#v err=%v", actual, replacement, err)
				}
			} else if exists, err := manager.Registry.Exists(value.Name); err != nil || exists {
				t.Fatalf("reconcile resurrected removed workspace metadata: exists=%t err=%v", exists, err)
			}
			if len(result.result.Changes) != 0 {
				t.Fatalf("reconcile reported stale repairs for a removed identity: %#v", result.result.Changes)
			}
		})
	}
}

func TestReconcileHandlesWorkspaceNamesSharingALock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	parent := canonicalTempDir(t)
	for _, spec := range []CreateSpec{
		{Name: "agent/a", Branch: "first", Path: filepath.Join(parent, "first"), Materializer: model.MaterializerGit},
		{Name: "agent-a", Branch: "second", Path: filepath.Join(parent, "second"), Materializer: model.MaterializerGit},
	} {
		value, err := manager.Create(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		value.Status = model.WorkspaceBroken
		if err := manager.Registry.Save(value); err != nil {
			t.Fatal(err)
		}
	}
	result, err := manager.Reconcile(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 2 {
		t.Fatalf("expected both workspaces to be repaired: %#v", result)
	}
	for _, name := range []string{"agent/a", "agent-a"} {
		value, err := manager.Registry.Load(name)
		if err != nil || value.Status != model.WorkspaceReady {
			t.Fatalf("reconcile did not repair %q: %#v, %v", name, value, err)
		}
	}
}

func TestReconcileFollowsMovedAndRenamedWorktreeIdentity(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	parent := canonicalTempDir(t)
	originalPath := filepath.Join(parent, "original")
	runGitTest(t, repo, "worktree", "add", "-b", "original", originalPath, "main")
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	original, err := manager.Adopt(ctx, "agent", originalPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.Registry.Load(original.Name)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	runGitTest(t, repo, "worktree", "move", originalPath, moved)
	runGitTest(t, moved, "branch", "-m", "renamed")
	dry, err := manager.Reconcile(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := manager.Registry.Load(original.Name)
	if err != nil || !reflect.DeepEqual(unchanged, before) {
		t.Fatalf("dry run mutated workspace metadata: %#v, %v", unchanged, err)
	}
	actual, err := manager.Reconcile(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual.Changes, dry.Changes) || len(actual.Changes) != 2 {
		t.Fatalf("dry-run repairs differ from application: dry=%#v actual=%#v", dry, actual)
	}
	updated, err := manager.Registry.Load(original.Name)
	if err != nil || updated.ID != original.ID || updated.Path != moved || updated.Branch != "renamed" || updated.Status != model.WorkspaceReady {
		t.Fatalf("reconcile lost worktree identity after move/rename: %#v, %v", updated, err)
	}
	if len(actual.Unmanaged) != 0 {
		t.Fatalf("managed identity reported as unmanaged: %#v", actual.Unmanaged)
	}
}

func TestReconcileDryRunReportsMissingWorktreeWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	original, err := manager.Create(ctx, CreateSpec{Name: "missing", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.Registry.Load(original.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(original.Path); err != nil {
		t.Fatal(err)
	}
	dry, err := manager.Reconcile(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Changes) != 1 || dry.Changes[0].Action != "mark-broken" {
		t.Fatalf("dry run did not report a missing worktree: %#v", dry)
	}
	unchanged, err := manager.Registry.Load(original.Name)
	if err != nil || !reflect.DeepEqual(unchanged, before) {
		t.Fatalf("dry run changed registry: %#v, %v", unchanged, err)
	}
	actual, err := manager.Reconcile(ctx, false)
	if err != nil || !reflect.DeepEqual(actual.Changes, dry.Changes) {
		t.Fatalf("dry-run repair differs from application: dry=%#v actual=%#v err=%v", dry, actual, err)
	}
}
