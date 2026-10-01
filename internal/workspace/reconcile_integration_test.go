package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

func TestAdoptAndReconcileOrdinaryGitWorktreeLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	external := filepath.Join(t.TempDir(), "external")
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
