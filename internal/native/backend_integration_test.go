package native_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/project"
)

func TestWorkspacePreservesGitAndFilesystemState(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, true)

	value := config.Default()
	value.Layers = []config.LayerRule{
		{Path: "deps", Mode: config.LayerClone, Fingerprint: []string{"deps.lock"}},
		{Path: "scratch", Mode: config.LayerEmpty},
		{Path: "shared-cache", Mode: config.LayerShare},
	}
	writeConfig(t, root, value)

	backend := openBackend(t, ctx, root)
	workspace, err := backend.Create(ctx, native.CreateSpec{Name: "agent-one", Ref: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Remove(context.Background(), workspace.Name, true, true) })

	if workspace.Status != model.WorkspaceReady || !workspace.EnvironmentReady {
		t.Fatalf("workspace was not ready: %#v", workspace)
	}
	if len(workspace.LayerIDs) != 3 {
		t.Fatalf("expected three materialized layer records, got %d", len(workspace.LayerIDs))
	}
	if data := mustRead(t, filepath.Join(workspace.Path, "src", "a.txt")); data != "alpha\n" {
		t.Fatalf("unexpected tracked content: %q", data)
	}

	scriptInfo, err := os.Stat(filepath.Join(workspace.Path, "bin", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && scriptInfo.Mode()&0o111 == 0 {
		t.Fatalf("executable bit was lost: %v", scriptInfo.Mode())
	}
	if runtime.GOOS != "windows" {
		target, err := os.Readlink(filepath.Join(workspace.Path, "link-to-a"))
		if err != nil {
			t.Fatalf("tracked symlink was not preserved: %v", err)
		}
		if target != filepath.ToSlash(filepath.Join("src", "a.txt")) && target != filepath.Join("src", "a.txt") {
			t.Fatalf("unexpected symlink target %q", target)
		}
	}

	workspaceSeed := filepath.Join(workspace.Path, "deps", "seed.txt")
	if data := mustRead(t, workspaceSeed); data != "prepared\n" {
		t.Fatalf("cloned environment was not ready: %q", data)
	}
	mustWrite(t, workspaceSeed, "agent-only\n", 0o644)
	if data := mustRead(t, filepath.Join(root, "deps", "seed.txt")); data != "prepared\n" {
		t.Fatalf("cloned environment mutated its source: %q", data)
	}

	scratchEntries, err := os.ReadDir(filepath.Join(workspace.Path, "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scratchEntries) != 0 {
		t.Fatalf("empty layer was not empty: %v", scratchEntries)
	}
	sharedTarget, err := os.Readlink(filepath.Join(workspace.Path, "shared-cache"))
	if err != nil {
		t.Fatalf("shared layer is not a symlink: %v", err)
	}
	if filepath.Clean(sharedTarget) != filepath.Join(root, "shared-cache") {
		t.Fatalf("shared layer points to %q", sharedTarget)
	}

	mustWrite(t, filepath.Join(workspace.Path, "src", "a.txt"), "workspace\n", 0o644)
	if data := mustRead(t, filepath.Join(root, "src", "a.txt")); data != "alpha\n" {
		t.Fatalf("main working tree was modified: %q", data)
	}
	status := git(t, workspace.Path, "status", "--short")
	if !strings.Contains(status, "src/a.txt") {
		t.Fatalf("Git missed the workspace modification: %q", status)
	}
	common := strings.TrimSpace(git(t, workspace.Path, "rev-parse", "--git-common-dir"))
	if common == "" {
		t.Fatal("linked worktree did not report a common Git directory")
	}
	registered, err := backend.Project.Repository.IsWorktreeRegistered(ctx, workspace.Path)
	if err != nil || !registered {
		t.Fatalf("workspace is not registered as a real Git worktree: %v", err)
	}

	if err := backend.Remove(ctx, workspace.Name, false, false); err == nil {
		t.Fatal("clean removal unexpectedly discarded a dirty workspace")
	}
	if err := backend.Remove(ctx, workspace.Name, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed workspace path still exists: %v", err)
	}
}

func TestPreparedAndFreshIndexesRemainCorrect(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	for _, test := range []struct {
		name     string
		prepared bool
	}{
		{name: "prepared", prepared: true},
		{name: "fresh", prepared: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace, err := backend.Create(ctx, native.CreateSpec{
				Name:          "index-" + test.name,
				Materializer:  model.MaterializerCopy,
				PreparedIndex: boolPtr(test.prepared),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Remove(context.Background(), workspace.Name, true, true)
			if workspace.PreparedIndex != test.prepared {
				t.Fatalf("prepared-index metadata=%t, want %t", workspace.PreparedIndex, test.prepared)
			}
			if status := strings.TrimSpace(git(t, workspace.Path, "status", "--porcelain=v1")); status != "" {
				t.Fatalf("new workspace is dirty: %q", status)
			}
			mustWrite(t, filepath.Join(workspace.Path, "src", "a.txt"), test.name+"\n", 0o644)
			if status := git(t, workspace.Path, "status", "--porcelain=v1"); !strings.Contains(status, "src/a.txt") {
				t.Fatalf("Git missed an edit with prepared=%t: %q", test.prepared, status)
			}
		})
	}
}

func TestRecoveryRollsBackInterruptedCreate(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	t.Setenv("CAMBIUM_FAILPOINT", "after-register")
	_, err := backend.Create(ctx, native.CreateSpec{Name: "crashed", Materializer: model.MaterializerGit})
	if !errors.Is(err, failpoint.ErrInjectedCrash) {
		t.Fatalf("expected injected crash, got %v", err)
	}
	t.Setenv("CAMBIUM_FAILPOINT", "")

	records, err := backend.Journal.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("expected one durable recovery record, got %#v, %v", records, err)
	}
	registered, err := backend.Project.Repository.IsWorktreeRegistered(ctx, records[0].Path)
	if err != nil || !registered {
		t.Fatalf("crashed worktree was not left in a recoverable state: %v", err)
	}
	actions, err := backend.Recover(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Action != "rollback" || actions[0].Status != "resolved" {
		t.Fatalf("unexpected recovery actions: %#v", actions)
	}
	if records, err := backend.Journal.List(); err != nil || len(records) != 0 {
		t.Fatalf("recovery record survived rollback: %#v, %v", records, err)
	}
	if exists, err := backend.Project.Repository.BranchExists(ctx, "cambium/crashed"); err != nil || exists {
		t.Fatalf("unadvanced branch survived rollback: exists=%t err=%v", exists, err)
	}
}

func TestRecoveryProtectsWorkCommittedAfterInterruptedCreate(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	t.Setenv("CAMBIUM_FAILPOINT", "after-register")
	_, err := backend.Create(ctx, native.CreateSpec{Name: "rescued", Materializer: model.MaterializerGit})
	if !errors.Is(err, failpoint.ErrInjectedCrash) {
		t.Fatalf("expected injected crash, got %v", err)
	}
	t.Setenv("CAMBIUM_FAILPOINT", "")
	records, err := backend.Journal.List()
	if err != nil || len(records) != 1 {
		t.Fatalf("expected one recovery record: %#v, %v", records, err)
	}
	record := records[0]
	mustWrite(t, filepath.Join(record.Path, "rescue.txt"), "important work\n", 0o644)
	git(t, record.Path, "add", "rescue.txt")
	git(t, record.Path, "commit", "-m", "work after crash")

	actions, err := backend.Recover(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Action != "protect" || actions[0].Status != "blocked" {
		t.Fatalf("advanced branch was not protected: %#v", actions)
	}
	if exists, _ := backend.Project.Repository.BranchExists(ctx, record.Branch); !exists {
		t.Fatal("recovery deleted an advanced branch")
	}
	if data := mustRead(t, filepath.Join(record.Path, "rescue.txt")); data != "important work\n" {
		t.Fatalf("recovery damaged rescued data: %q", data)
	}

	// Explicit cleanup for the intentionally blocked operation.
	if err := backend.Project.Repository.WorktreeRemove(ctx, record.Path, true); err != nil {
		t.Fatal(err)
	}
	if err := backend.Project.Repository.DeleteBranch(ctx, record.Branch, true); err != nil {
		t.Fatal(err)
	}
	if err := backend.Journal.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentWorkspaceCreation(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	const count = 8
	type result struct {
		name string
		path string
		err  error
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			name := fmt.Sprintf("parallel-%02d", index)
			workspace, err := backend.Create(ctx, native.CreateSpec{Name: name, Materializer: model.MaterializerGit})
			results <- result{name: name, path: workspace.Path, err: err}
		}(i)
	}
	wg.Wait()
	close(results)

	var names []string
	for result := range results {
		if result.err != nil {
			t.Errorf("create %s: %v", result.name, result.err)
			continue
		}
		names = append(names, result.name)
		if data := mustRead(t, filepath.Join(result.path, "src", "a.txt")); data != "alpha\n" {
			t.Errorf("%s has bad content %q", result.name, data)
		}
	}
	if len(names) != count {
		t.Fatalf("created %d/%d concurrent workspaces", len(names), count)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := backend.Remove(ctx, name, true, true); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	}
}

func TestConcurrentDuplicateCreateCannotDeleteWinner(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := backend.Create(ctx, native.CreateSpec{Name: "same", Materializer: model.MaterializerGit})
			results <- err
		}()
	}
	var successes, failures int
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("duplicate create results: success=%d failure=%d", successes, failures)
	}
	workspace, err := backend.Registry.Load("same")
	if err != nil {
		t.Fatal(err)
	}
	if data := mustRead(t, filepath.Join(workspace.Path, "src", "a.txt")); data != "alpha\n" {
		t.Fatalf("losing creator damaged winner: %q", data)
	}
	if err := backend.Remove(ctx, "same", true, true); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentDifferentNamesCannotClaimSamePath(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)
	sharedPath := filepath.Join(t.TempDir(), "shared")

	type result struct {
		name string
		err  error
	}
	results := make(chan result, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			_, err := backend.Create(ctx, native.CreateSpec{Name: name, Path: sharedPath, Materializer: model.MaterializerGit})
			results <- result{name: name, err: err}
		}(name)
	}
	var winner string
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatalf("both creators claimed one path: %s and %s", winner, result.name)
			}
			winner = result.name
		}
	}
	if winner == "" {
		t.Fatal("neither creator acquired the path")
	}
	workspace, err := backend.Registry.Load(winner)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Path != sharedPath {
		t.Fatalf("winner path = %q", workspace.Path)
	}
	if data := mustRead(t, filepath.Join(sharedPath, "src", "a.txt")); data != "alpha\n" {
		t.Fatalf("loser damaged shared path: %q", data)
	}
	if err := backend.Remove(ctx, winner, true, true); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePathInsidePrimaryCheckoutIsRejected(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)
	inside := filepath.Join(root, "nested-workspace")
	if _, err := backend.Create(ctx, native.CreateSpec{Name: "nested", Path: inside, Materializer: model.MaterializerGit}); err == nil || !strings.Contains(err.Error(), "inside the primary working tree") {
		t.Fatalf("unsafe nested worktree path was accepted: %v", err)
	}
	if _, err := os.Stat(inside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected path was still created: %v", err)
	}
}

func TestWorkspacePathCannotReenterPrimaryCheckoutThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	aliasRoot := t.TempDir()
	alias := filepath.Join(aliasRoot, "repo-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(alias, "nested-workspace")
	if _, err := backend.Create(ctx, native.CreateSpec{Name: "symlink-nested", Path: inside, Materializer: model.MaterializerGit}); err == nil || !strings.Contains(err.Error(), "inside the primary working tree") {
		t.Fatalf("symlinked nested worktree path was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested-workspace")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected symlink path still created data: %v", err)
	}
}

func TestWorkspacePathCanonicalizesSymlinkedExternalParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	writeConfig(t, root, config.Default())
	backend := openBackend(t, ctx, root)

	external := t.TempDir()
	aliasRoot := t.TempDir()
	alias := filepath.Join(aliasRoot, "external-link")
	if err := os.Symlink(external, alias); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(alias, "workspace")
	workspace, err := backend.Create(ctx, native.CreateSpec{Name: "external-symlink", Path: requested, Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Remove(context.Background(), workspace.Name, true, true)
	want := filepath.Join(external, "workspace")
	if workspace.Path != want {
		t.Fatalf("canonical workspace path = %q, want %q", workspace.Path, want)
	}
	if data := mustRead(t, filepath.Join(want, "src", "a.txt")); data != "alpha\n" {
		t.Fatalf("canonical workspace has bad content: %q", data)
	}
}

func TestLayerSafetyRejectsTrackedAndUnignoredPaths(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	t.Run("tracked", func(t *testing.T) {
		root := initializeRepository(t, false)
		value := config.Default()
		value.Layers = []config.LayerRule{{Path: "src", Mode: config.LayerClone}}
		writeConfig(t, root, value)
		backend := openBackend(t, ctx, root)
		if _, err := backend.Create(ctx, native.CreateSpec{Name: "unsafe"}); err == nil || !strings.Contains(err.Error(), "tracked") {
			t.Fatalf("expected tracked-layer refusal, got %v", err)
		}
	})

	t.Run("unignored", func(t *testing.T) {
		root := initializeRepository(t, false)
		if err := os.MkdirAll(filepath.Join(root, "local-state"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(root, "local-state", "value"), "x", 0o644)
		value := config.Default()
		value.Layers = []config.LayerRule{{Path: "local-state", Mode: config.LayerClone}}
		writeConfig(t, root, value)
		backend := openBackend(t, ctx, root)
		if _, err := backend.Create(ctx, native.CreateSpec{Name: "unsafe"}); err == nil || !strings.Contains(err.Error(), "not ignored") {
			t.Fatalf("expected unignored-layer refusal, got %v", err)
		}
	})

	t.Run("negated-child", func(t *testing.T) {
		root := initializeRepository(t, false)
		if err := os.MkdirAll(filepath.Join(root, "managed"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(root, "managed", "ignored.bin"), "ignored", 0o644)
		mustWrite(t, filepath.Join(root, "managed", "keep.txt"), "not ignored", 0o644)
		withNegation := mustRead(t, filepath.Join(root, ".gitignore")) + "managed/*\n!managed/keep.txt\n"
		mustWrite(t, filepath.Join(root, ".gitignore"), withNegation, 0o644)
		git(t, root, "add", ".gitignore")
		git(t, root, "commit", "-q", "-m", "nested ignore")
		value := config.Default()
		value.Layers = []config.LayerRule{{Path: "managed", Mode: config.LayerClone}}
		writeConfig(t, root, value)
		backend := openBackend(t, ctx, root)
		if _, err := backend.Create(ctx, native.CreateSpec{Name: "negated"}); err == nil || !strings.Contains(err.Error(), "contains untracked files") {
			t.Fatalf("expected negated unignored child to be rejected, got %v", err)
		}
	})
}

func initializeRepository(t *testing.T, environments bool) string {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "config", "user.name", "Cambium Test")
	git(t, root, "config", "user.email", "cambium@example.invalid")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "src", "a.txt"), "alpha\n", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "bin", "run.sh"), "#!/bin/sh\necho ok\n", 0o755)
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.ToSlash(filepath.Join("src", "a.txt")), filepath.Join(root, "link-to-a")); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "deps.lock"), "deps-v1\n", 0o644)
	ignored := "deps/\nscratch/\nshared-cache/\n"
	mustWrite(t, filepath.Join(root, ".gitignore"), ignored, 0o644)
	if environments {
		for _, path := range []string{"deps", "shared-cache"} {
			if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		mustWrite(t, filepath.Join(root, "deps", "seed.txt"), "prepared\n", 0o644)
		mustWrite(t, filepath.Join(root, "shared-cache", "shared.txt"), "shared\n", 0o644)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "initial")
	return root
}

func openBackend(t *testing.T, ctx context.Context, root string) *native.Backend {
	t.Helper()
	projectValue, err := project.Open(ctx, root, execx.OSRunner{})
	if err != nil {
		t.Fatal(err)
	}
	return native.New(projectValue)
}

func writeConfig(t *testing.T, root string, value config.Config) {
	t.Helper()
	if _, err := config.Write(root, value, false); err != nil {
		t.Fatal(err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes)
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func boolPtr(value bool) *bool { return &value }
