package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

func TestAdoptAndReconcileOrdinaryGitWorktreeLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	external := filepath.Join(canonicalTempDir(t), "external")
	runGitTest(t, repo, "worktree", "add", "-b", "external-branch", external, "main")

	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	adopted, err := manager.Adopt(ctx, "external", external)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Status != model.WorkspaceReady || adopted.Branch != "external-branch" || adopted.Path != external {
		t.Fatalf("unexpected adopted workspace: %#v", adopted)
	}

	moved := filepath.Join(filepath.Dir(external), "external-moved")
	runGitTest(t, repo, "worktree", "move", external, moved)
	dryRun, err := manager.Reconcile(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRun.Changes) != 1 || dryRun.Changes[0].Action != "move" || dryRun.Changes[0].To != moved {
		t.Fatalf("dry-run did not report move: %#v", dryRun)
	}
	unchanged, err := manager.Registry.Load("external")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Path != external {
		t.Fatalf("dry-run mutated registry path: %s", unchanged.Path)
	}

	result, err := manager.Reconcile(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 || result.Changes[0].Action != "move" {
		t.Fatalf("reconcile did not apply move: %#v", result)
	}
	updated, err := manager.Registry.Load("external")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Path != moved || updated.Status != model.WorkspaceReady {
		t.Fatalf("moved workspace metadata was not repaired: %#v", updated)
	}

	runGitTest(t, repo, "worktree", "remove", moved)
	result, err = manager.Reconcile(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	broken, err := manager.Registry.Load("external")
	if err != nil {
		t.Fatal(err)
	}
	if broken.Status != model.WorkspaceBroken {
		t.Fatalf("removed Git worktree was not marked broken: %#v result=%#v", broken, result)
	}
}

func TestAdoptRejectsPrimaryDetachedAndUnknownPaths(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	if _, err := manager.Adopt(ctx, "primary", repo); err == nil {
		t.Fatal("primary worktree was adopted")
	}
	unknown := filepath.Join(t.TempDir(), "unknown")
	if err := os.MkdirAll(unknown, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Adopt(ctx, "unknown", unknown); err == nil {
		t.Fatal("unregistered directory was adopted")
	}

	detached := filepath.Join(t.TempDir(), "detached")
	runGitTest(t, repo, "worktree", "add", "--detach", detached, "main")
	defer runGitTest(t, repo, "worktree", "remove", "--force", detached)
	if _, err := manager.Adopt(ctx, "detached", detached); err == nil {
		t.Fatal("detached worktree was adopted without a branch")
	}
}

func TestAdoptUsesWorktreeIdentityFromLinkedCheckout(t *testing.T) {
	for _, target := range []string{"primary", "invoking-linked"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			repo := initSpecRepo(t)
			external := filepath.Join(canonicalTempDir(t), "external")
			runGitTest(t, repo, "worktree", "add", "-b", "external-branch", external, "main")
			projectValue, err := project.Open(ctx, external, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(projectValue)
			if target == "primary" {
				if _, err := manager.Adopt(ctx, "primary", repo); err == nil {
					t.Fatal("adopted the primary checkout when invoked from a linked worktree")
				}
				if values, err := manager.Registry.List(); err != nil || len(values) != 0 {
					t.Fatalf("rejected primary adoption changed registry: %#v, %v", values, err)
				}
				return
			}
			adopted, err := manager.Adopt(ctx, "external", external)
			if err != nil {
				t.Fatalf("cannot adopt the invoking linked worktree: %v", err)
			}
			if adopted.Path != external || adopted.Branch != "external-branch" {
				t.Fatalf("wrong adopted worktree: %#v", adopted)
			}
			if err := manager.Remove(ctx, "external", false, true); err != nil {
				t.Fatalf("remove adopted invoking worktree: %v", err)
			}
		})
	}
}

func TestAdoptRejectsDuplicateWorktreeAliases(t *testing.T) {
	for _, duplicatePath := range []string{"same", "symlink", "moved", "moved-and-renamed"} {
		t.Run(duplicatePath, func(t *testing.T) {
			ctx := context.Background()
			repo := initSpecRepo(t)
			external := filepath.Join(canonicalTempDir(t), "external")
			runGitTest(t, repo, "worktree", "add", "-b", "external-branch", external, "main")
			projectValue, err := project.Open(ctx, repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(projectValue)
			original, err := manager.Adopt(ctx, "original", external)
			if err != nil {
				t.Fatal(err)
			}
			path := external
			switch duplicatePath {
			case "symlink":
				path = filepath.Join(filepath.Dir(external), "alias")
				if err := os.Symlink(external, path); err != nil {
					t.Fatal(err)
				}
			case "moved", "moved-and-renamed":
				path = filepath.Join(filepath.Dir(external), "moved")
				runGitTest(t, repo, "worktree", "move", external, path)
				if duplicatePath == "moved-and-renamed" {
					runGitTest(t, path, "branch", "-m", "renamed-branch")
				}
			}
			if _, err := manager.Adopt(ctx, "duplicate", path); err == nil {
				t.Fatal("adopted an already managed worktree under another name")
			}
			values, err := manager.Registry.List()
			if err != nil || len(values) != 1 || values[0].ID != original.ID {
				t.Fatalf("duplicate adoption changed the owner: %#v, %v", values, err)
			}
		})
	}
}

func TestAdoptPreservesPendingCreationClaims(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	crashedPath := filepath.Join(canonicalTempDir(t), "pending")
	t.Setenv("CAMBIUM_FAILPOINT", "after-register")
	_, err = manager.Create(ctx, CreateSpec{Name: "pending", Path: crashedPath, Materializer: model.MaterializerGit})
	if !errors.Is(err, failpoint.ErrInjectedCrash) {
		t.Fatalf("expected interrupted creation: %v", err)
	}
	t.Setenv("CAMBIUM_FAILPOINT", "")
	external := filepath.Join(canonicalTempDir(t), "external")
	runGitTest(t, repo, "worktree", "add", "-b", "external-branch", external, "main")
	for _, attempt := range []struct{ name, path string }{{"alias", crashedPath}, {"pending", external}} {
		if _, err := manager.Adopt(ctx, attempt.name, attempt.path); err == nil {
			t.Fatalf("adopted a pending creation's path or name: %#v", attempt)
		}
	}
	if values, err := manager.Registry.List(); err != nil || len(values) != 0 {
		t.Fatalf("adoption published a competing workspace: %#v, %v", values, err)
	}
	if records, err := manager.Native.Journal.List(); err != nil || len(records) != 1 || records[0].Name != "pending" {
		t.Fatalf("adoption changed the pending operation: %#v, %v", records, err)
	}
	if _, err := manager.Recover(ctx, false); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptRejectsOrphanedOwnershipReceipt(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	original, err := manager.Create(ctx, CreateSpec{Name: "original", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Registry.Delete(original.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Adopt(ctx, "replacement", original.Path); err == nil {
		t.Fatal("adopted a worktree that still carries another workspace's ownership receipt")
	}
	if err := manager.Registry.Save(original); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(ctx, original.Name, false, true); err != nil {
		t.Fatalf("adoption disturbed the original ownership receipt: %v", err)
	}
}

func TestConcurrentAdoptionPublishesOneWorkspace(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	external := filepath.Join(canonicalTempDir(t), "external")
	runGitTest(t, repo, "worktree", "add", "-b", "external-branch", external, "main")
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			<-start
			_, err := manager.Adopt(ctx, name, external)
			results <- err
		}(name)
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if values, err := manager.Registry.List(); err != nil || len(values) != 1 || successes != 1 {
		t.Fatalf("concurrent adoption published competing identities: successes=%d, registry=%#v, err=%v", successes, values, err)
	}
}

// canonicalTempDir resolves symlinks in t.TempDir(). On macOS the temp dir is
// /var/... but Cambium canonicalizes workspace paths to /private/var/..., so
// expectations built from the raw temp dir fail spuriously.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
