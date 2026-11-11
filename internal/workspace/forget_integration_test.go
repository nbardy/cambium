package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/state"
)

// Forget may delete a record only when nothing of the workspace is left. Each
// refusal case below is a way a live worktree can look missing from its
// recorded path; forgetting it there would leave real work unmanaged or, with a
// later prune, without its environment layers.
func TestForgetDropsOnlyWorkspacesThatAreGone(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, repo string, value model.Workspace, parent string)
		refused string // substring of the refusal; "" means the record is forgotten
	}{
		{
			name: "worktree removed by git",
			arrange: func(t *testing.T, repo string, value model.Workspace, _ string) {
				runGitTest(t, repo, "worktree", "remove", "--force", value.Path)
			},
		},
		{
			name: "directory deleted and registration pruned",
			arrange: func(t *testing.T, repo string, value model.Workspace, _ string) {
				if err := os.RemoveAll(value.Path); err != nil {
					t.Fatal(err)
				}
				runGitTest(t, repo, "worktree", "prune")
			},
		},
		{
			name:    "worktree still present",
			arrange: func(*testing.T, string, model.Workspace, string) {},
			refused: "still exists",
		},
		{
			name: "directory deleted but Git still registers it",
			arrange: func(t *testing.T, _ string, value model.Workspace, _ string) {
				if err := os.RemoveAll(value.Path); err != nil {
					t.Fatal(err)
				}
			},
			refused: "Git still registers a worktree",
		},
		{
			name: "worktree moved",
			arrange: func(t *testing.T, repo string, value model.Workspace, parent string) {
				runGitTest(t, repo, "worktree", "move", value.Path, filepath.Join(parent, "moved"))
			},
			refused: "moved to",
		},
		{
			name: "branch checked out in a new worktree",
			arrange: func(t *testing.T, repo string, value model.Workspace, parent string) {
				runGitTest(t, repo, "worktree", "remove", "--force", value.Path)
				runGitTest(t, repo, "worktree", "add", filepath.Join(parent, "elsewhere"), value.Branch)
			},
			refused: "is checked out at",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo := initSpecRepo(t)
			p, err := project.Open(ctx, repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(p)
			parent := canonicalTempDir(t)
			value, err := manager.Create(ctx, CreateSpec{Name: "agent", Path: filepath.Join(parent, "checkout"), Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			test.arrange(t, repo, value, parent)

			// A dry run reports the same verdict and never deletes.
			_, dryErr := manager.Forget(ctx, []string{value.Name}, true)
			if _, err := manager.Registry.Load(value.Name); err != nil {
				t.Fatalf("dry run changed the record: %v", err)
			}
			result, err := manager.Forget(ctx, []string{value.Name}, false)
			if (dryErr == nil) != (err == nil) {
				t.Errorf("dry run and real run disagree: dry=%v real=%v", dryErr, err)
			}
			_, loadErr := manager.Registry.Load(value.Name)
			if test.refused == "" {
				if err != nil {
					t.Fatalf("forget refused a workspace that is gone: %v", err)
				}
				if len(result.Forgotten) != 1 || result.Forgotten[0].Name != value.Name || result.Forgotten[0].Path != value.Path {
					t.Errorf("unexpected result: %#v", result)
				}
				if !errors.Is(loadErr, state.ErrWorkspaceNotFound) {
					t.Errorf("record survived forget: %v", loadErr)
				}
				// The branch may hold the only copy of the work.
				if _, err := p.Repository.BranchCommit(ctx, value.Branch); err != nil {
					t.Errorf("forget deleted branch %q: %v", value.Branch, err)
				}
				return
			}
			var remains *WorktreeRemainsError
			if !errors.As(err, &remains) || !strings.Contains(remains.Reason, test.refused) {
				t.Fatalf("expected a refusal containing %q, got %v", test.refused, err)
			}
			if loadErr != nil {
				t.Errorf("a refused forget deleted the record: %v", loadErr)
			}
			// --missing must skip it as well, without failing.
			swept, err := manager.ForgetMissing(ctx, false)
			if err != nil || len(swept.Forgotten) != 0 {
				t.Errorf("ForgetMissing touched a workspace that is not gone: %#v %v", swept, err)
			}
			if _, err := manager.Registry.Load(value.Name); err != nil {
				t.Errorf("ForgetMissing deleted the record: %v", err)
			}
		})
	}
}

// The sweep is what a cleanup job runs: it must drop the dead record and leave
// the live workspace beside it alone.
func TestForgetMissingSweepsOnlyDeadRecords(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	parent := canonicalTempDir(t)
	dead, err := manager.Create(ctx, CreateSpec{Name: "dead", Path: filepath.Join(parent, "dead"), Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Create(ctx, CreateSpec{Name: "live", Path: filepath.Join(parent, "live"), Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "worktree", "remove", "--force", dead.Path)

	result, err := manager.ForgetMissing(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Forgotten) != 1 || result.Forgotten[0].Name != dead.Name {
		t.Fatalf("unexpected sweep: %#v", result)
	}
	if _, err := manager.Registry.Load(live.Name); err != nil {
		t.Errorf("sweep deleted a live workspace record: %v", err)
	}
	if _, err := os.Stat(live.Path); err != nil {
		t.Errorf("sweep touched a live worktree: %v", err)
	}
}
