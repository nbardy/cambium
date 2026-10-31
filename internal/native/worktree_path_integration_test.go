package native_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/state"
)

func TestCreateAndRemoveWorktreeWithEmbeddedNewlinePath(t *testing.T) {
	testCreateAndRemoveWorktreePath(t, "agent\nworktree")
}

func TestCreateAndRemoveWorktreeWithTrailingWhitespacePath(t *testing.T) {
	for _, name := range []string{"agent-space ", "agent-tab\t", "agent-newline\n", "agent-cr\r"} {
		t.Run(name, func(t *testing.T) {
			testCreateAndRemoveWorktreePath(t, name)
		})
	}
}

func testCreateAndRemoveWorktreePath(t *testing.T, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows path rules do not preserve these trailing/control characters")
	}
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	backend := openBackend(t, ctx, root)
	path := filepath.Join(canonicalTempDir(t), name)
	workspace, err := backend.Create(ctx, native.CreateSpec{
		Name: "newline-path", Path: path, Materializer: model.MaterializerGit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Path != path || workspace.Status != model.WorkspaceReady {
		t.Fatalf("workspace identity changed: %#v", workspace)
	}
	if registered, err := backend.Project.Repository.IsWorktreeRegistered(ctx, path); err != nil || !registered {
		t.Fatalf("created worktree is not registered: %t, %v", registered, err)
	}
	mustWrite(t, filepath.Join(path, "src", "a.txt"), "local work\n", 0o644)
	if err := backend.Remove(ctx, workspace.Name, false, true); err == nil {
		t.Fatal("removal did not protect dirty worktree contents")
	}
	if value := mustRead(t, filepath.Join(path, "src", "a.txt")); value != "local work\n" {
		t.Fatalf("failed removal changed local work: %q", value)
	}
	inside := openBackend(t, ctx, path)
	if err := inside.Remove(ctx, workspace.Name, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace path survived removal: %v", err)
	}
	if _, err := backend.Registry.Load(workspace.Name); !errors.Is(err, state.ErrWorkspaceNotFound) {
		t.Fatalf("workspace metadata survived removal: %v", err)
	}
	if exists, err := backend.Project.Repository.BranchExists(ctx, workspace.Branch); err != nil || exists {
		t.Fatalf("workspace branch survived removal: %t, %v", exists, err)
	}
	if records, err := backend.Journal.List(); err != nil || len(records) != 0 {
		t.Fatalf("removal left pending operations: %#v, %v", records, err)
	}
}
