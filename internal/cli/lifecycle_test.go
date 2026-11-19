package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/workspace"
)

// userConfig points the per-user config at a temp file for one test, so
// nothing reads the developer's real ~/.config/cambium.
func userConfig(t *testing.T, settings string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "version = 1\n[settings]\n"+settings)
	t.Setenv("CAMBIUM_CONFIG", path)
}

func canon(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func (f *safeFixture) listJSON(args ...string) []map[string]any {
	f.t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(f.mustRun(args...)), &rows); err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// Bug 1: `add` with no COMMIT must branch from the caller's checkout.
func TestAddDefaultsToTheCallersHEAD(t *testing.T) {
	f := newSafeFixture(t)
	first := f.create("first")
	writeFile(t, filepath.Join(first, "second.txt"), "two\n")
	gitCLI(t, first, "add", ".")
	gitCLI(t, first, "commit", "-q", "-m", "second")
	want := strings.TrimSpace(gitOut(t, first, "rev-parse", "HEAD"))

	inner, _, _ := testApp(first)
	nested := filepath.Join(t.TempDir(), "nested")
	if code := inner.Run(context.Background(), []string{"add", "--materializer", "git", "-b", "nested", "-name", "nested", nested}); code != 0 {
		t.Fatal("add from a linked worktree failed")
	}
	if got := strings.TrimSpace(gitOut(t, nested, "rev-parse", "HEAD")); got != want {
		t.Fatalf("add resolved HEAD in the primary checkout: got %s want %s", got, want)
	}
}

// Feature: default workspace root from user config, overridden per repository.
func TestAddWithoutPathUsesWorkspaceRoot(t *testing.T) {
	userRoot := t.TempDir()
	userConfig(t, "workspace_root = \""+userRoot+"\"\n")
	f := newSafeFixture(t)
	out := f.mustRun("add", "--materializer", "git", "-b", "u1", "-name", "u1", "--print-path")
	if got, want := strings.TrimSpace(out), filepath.Join(canon(t, userRoot), "u1"); got != want {
		t.Fatalf("user workspace_root ignored: got %s want %s", got, want)
	}
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[settings]\nworkspace_root = \""+repoRoot+"\"\n")
	out = f.mustRun("add", "--materializer", "git", "-b", "r1", "-name", "r1", "--print-path")
	if got, want := strings.TrimSpace(out), filepath.Join(canon(t, repoRoot), "r1"); got != want {
		t.Fatalf(".cambium.toml must override the user root: got %s want %s", got, want)
	}
}

// Bug 4: removal is guarded by default; --force is the only opt-out.
func TestRemoveIsGuardedByDefault(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("guarded")
	writeFile(t, filepath.Join(path, "scratch.txt"), "unsaved\n")
	f.refused("dirty", "remove", "--json", "guarded")
	requireExists(t, filepath.Join(path, "scratch.txt"), true)
	f.mustRun("remove", "--force", "guarded")
	requireExists(t, path, false)
}

// Bug 7: a held use marker (what `cambium run` writes) protects the workspace.
func TestRemoveAndPruneRefuseInUseWorkspace(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("busy")
	manager, err := f.app.manager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	value, err := manager.Registry.Load("busy")
	if err != nil {
		t.Fatal(err)
	}
	release, err := manager.MarkInUse(value)
	if err != nil {
		t.Fatal(err)
	}
	f.refused("in-use", "remove", "--json", "busy")
	out := f.mustRun("prune", "--merged", "main", "--older-than", "1ns", "--json")
	if strings.Contains(out, `"removed":["busy"]`) {
		t.Fatalf("prune removed an in-use workspace: %s", out)
	}
	requireExists(t, path, true)
	release()
	f.mustRun("remove", "busy")
	requireExists(t, path, false)
}

// Bug 3: NAME commands resolve the workspace from the registry anywhere.
func TestNameCommandsWorkOutsideTheRepository(t *testing.T) {
	f := newSafeFixture(t)
	reposRoot := filepath.Dir(f.root)
	userConfig(t, "repos_root = \""+reposRoot+"\"\n")
	path := f.create("elsewhere")
	outside, stdout, stderr := testApp(t.TempDir())
	if code := outside.Run(context.Background(), []string{"path", "elsewhere"}); code != 0 || strings.TrimSpace(stdout.String()) != path {
		t.Fatalf("path outside repo: code=%d out=%q err=%s", code, stdout.String(), stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := outside.Run(context.Background(), []string{"remove", "elsewhere"}); code != 0 {
		t.Fatalf("remove outside repo: %s", stderr.String())
	}
	requireExists(t, path, false)
}

// Feature: prune --merged REF.
func TestPruneMergedKeepsUnmergedAndDirty(t *testing.T) {
	f := newSafeFixture(t)
	merged := f.create("merged")
	writeFile(t, filepath.Join(merged, "m.txt"), "m\n")
	gitCLI(t, merged, "add", ".")
	gitCLI(t, merged, "commit", "-q", "-m", "m")
	gitCLI(t, f.root, "merge", "-q", "--ff-only", strings.TrimSpace(gitOut(t, merged, "rev-parse", "HEAD")))
	unmerged := f.create("unmerged")
	writeFile(t, filepath.Join(unmerged, "u.txt"), "u\n")
	gitCLI(t, unmerged, "add", ".")
	gitCLI(t, unmerged, "commit", "-q", "-m", "u")
	fresh := f.create("fresh") // no commits of its own: kept unless idle
	var result workspace.PruneMergedResult
	if err := json.Unmarshal([]byte(f.mustRun("prune", "--merged", "main", "--json")), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "merged" {
		t.Fatalf("want only merged removed, got %+v", result)
	}
	requireExists(t, merged, false)
	requireExists(t, unmerged, true)
	requireExists(t, fresh, true)
}

// Features: status --all and maintain --all-repos over every repository
// found under repos_root.
func TestStatusAndMaintainAcrossRepositories(t *testing.T) {
	f := newSafeFixture(t)
	userConfig(t, "repos_root = \""+filepath.Dir(f.root)+"\"\n")
	path := f.create("tracked")
	writeFile(t, filepath.Join(path, "dirty.txt"), "x\n")
	rows := f.listJSON("status", "--all", "--json")
	if len(rows) != 1 || rows[0]["name"] != "tracked" || rows[0]["dirty_paths"] != float64(1) {
		t.Fatalf("status --all: %v", rows)
	}
	if size, _ := rows[0]["size"].(map[string]any); size == nil || size["exclusive_bytes"] == nil {
		t.Fatalf("status must report exclusive bytes: %v", rows[0])
	}
	// A worktree deleted behind Cambium's back is forgotten by maintain.
	gone := f.create("gone")
	gitCLI(t, f.root, "worktree", "remove", "--force", gone)
	out := f.mustRun("maintain", "--all-repos", "--json")
	if !strings.Contains(out, `"gone"`) {
		t.Fatalf("maintain did not forget the missing workspace: %s", out)
	}
	requireExists(t, path, true)
}

// Bug 6: the post-add summary no longer lists unverified environment paths.
func TestAddSummaryOmitsUnverifiedNoise(t *testing.T) {
	f := newSafeFixture(t)
	out := f.mustRun("create", "--materializer", "git", "quiet")
	if strings.Contains(out, "unverified") {
		t.Fatalf("add summary still prints unverified list:\n%s", out)
	}
}

func TestFindRepositoriesSkipsLinkedWorktrees(t *testing.T) {
	f := newSafeFixture(t)
	f.create("linked")
	found, err := project.FindRepositories(filepath.Dir(f.root))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || canon(t, found[0]) != canon(t, f.root) {
		t.Fatalf("want exactly the primary checkout, got %v", found)
	}
}
