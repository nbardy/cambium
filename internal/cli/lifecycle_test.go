package cli

import (
	"context"
	"encoding/json"
	"os"
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
	merged := f.createEphemeral("merged")
	keep := f.create("not-ephemeral") // long-lived checkout: never swept
	gitCLI(t, keep, "merge", "-q", "--ff-only", "main")
	writeFile(t, filepath.Join(merged, "m.txt"), "m\n")
	gitCLI(t, merged, "add", ".")
	gitCLI(t, merged, "commit", "-q", "-m", "m")
	gitCLI(t, f.root, "merge", "-q", "--ff-only", strings.TrimSpace(gitOut(t, merged, "rev-parse", "HEAD")))
	unmerged := f.create("unmerged")
	writeFile(t, filepath.Join(unmerged, "u.txt"), "u\n")
	gitCLI(t, unmerged, "add", ".")
	gitCLI(t, unmerged, "commit", "-q", "-m", "u")
	fresh := f.createEphemeral("fresh") // no commits of its own: kept only while --older-than says it is young
	var result workspace.PruneMergedResult
	if err := json.Unmarshal([]byte(f.mustRun("prune", "--merged", "main", "--older-than", "1h", "--json")), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "merged" {
		t.Fatalf("want only merged removed, got %+v", result)
	}
	requireExists(t, merged, false)
	requireExists(t, unmerged, true)
	requireExists(t, fresh, true)
	requireExists(t, keep, true)
	// Alone, with no --older-than, prune --merged acts: a workspace already in REF is removed.
	if err := json.Unmarshal([]byte(f.mustRun("prune", "--merged", "main", "--json")), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "fresh" {
		t.Fatalf("prune --merged alone must remove the merged no-commit workspace, got %+v", result)
	}
	requireExists(t, fresh, false)
	requireExists(t, unmerged, true)
	requireExists(t, keep, true)
}

func (f *safeFixture) createEphemeral(name string) string {
	f.t.Helper()
	f.mustRun("create", "--materializer", "git", "--ephemeral", name)
	return strings.TrimSpace(f.mustRun("path", name))
}

// merge_target + prune_merged_on_add: making a workspace sweeps finished
// ephemeral ones; maintain does the same with no --merged flag.
func TestMergeTargetSweepsOnAddAndMaintain(t *testing.T) {
	f := newSafeFixture(t)
	writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[settings]\nmerge_target = \"main\"\nprune_merged_on_add = true\n")
	finish := func(name string) string {
		path := f.createEphemeral(name)
		writeFile(t, filepath.Join(path, name+".txt"), "x\n")
		gitCLI(t, path, "add", ".")
		gitCLI(t, path, "commit", "-q", "-m", name)
		gitCLI(t, f.root, "merge", "-q", "--ff-only", strings.TrimSpace(gitOut(t, path, "rev-parse", "HEAD")))
		return path
	}
	first := finish("first")
	f.createEphemeral("second") // sweeps first
	requireExists(t, first, false)
	second := finish("second-done")
	f.mustRun("maintain")
	requireExists(t, second, false)
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

// Every removal, --force included, first saves the workspace as a hidden
// checkpoint, and `recover --removed` brings the work back.
func TestRemoveCheckpointsFirstAndRecoverRestores(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("doomed")
	writeFile(t, filepath.Join(path, "untracked.txt"), "irreplaceable\n")
	f.mustRun("remove", "--force", "doomed")
	requireExists(t, path, false)

	listed := f.mustRun("recover", "--removed")
	ref := strings.Fields(listed)[0]
	if !strings.HasPrefix(ref, workspace.RemovedPrefix+"doomed-") {
		t.Fatalf("no removal checkpoint listed:\n%s", listed)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored")
	f.mustRun("recover", "--removed", "--path", restoredPath, ref, "restored")
	got, err := os.ReadFile(filepath.Join(restoredPath, "untracked.txt"))
	if err != nil || string(got) != "irreplaceable\n" {
		t.Fatalf("restored workspace lost the untracked file: %q %v", got, err)
	}
}

// A refused guarded removal deletes nothing and leaves no checkpoint behind;
// a successful one leaves exactly one.
func TestGuardedRemoveCheckpointOnlyWhenItRemoves(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("dirty")
	writeFile(t, filepath.Join(path, "wip.txt"), "x\n")
	f.refused("dirty", "remove", "--json", "dirty")
	if out := f.mustRun("recover", "--removed"); !strings.Contains(out, "no removal checkpoints") {
		t.Fatalf("a refused removal left a checkpoint:\n%s", out)
	}
	f.mustRun("remove", "--salvage-ref", "refs/salvage/dirty", "dirty")
	if out := f.mustRun("recover", "--removed"); !strings.Contains(out, workspace.RemovedPrefix+"dirty-") {
		t.Fatalf("a successful removal left no checkpoint:\n%s", out)
	}
}

// `cambium run` registers a use marker for the length of its child, so a
// concurrent prune or remove leaves the workspace alone; the marker is gone
// when the child exits.
func TestRunRegistersAUseMarkerForItsChild(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("running")
	manager, err := f.app.manager(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	value, err := manager.Registry.Load("running")
	if err != nil {
		t.Fatal(err)
	}
	markers := manager.Project.UseDir(value.ID)
	out := f.mustRun("run", "running", "--", "ls", markers)
	if strings.TrimSpace(out) == "" {
		t.Fatalf("no use marker existed while the child ran: %q", out)
	}
	if holders, err := manager.InUse(value); err != nil || len(holders) != 0 {
		t.Fatalf("marker outlived the child: %v %v", holders, err)
	}
	f.mustRun("remove", "running")
	requireExists(t, path, false)
}

// submodules = true: a new workspace gets its kit from the invoking checkout,
// removal refuses unpushed kit work, then drops a clean kit so the worktree
// can go (git worktree remove refuses populated submodules).
func TestSubmodulesPopulateAndRemove(t *testing.T) {
	f := newSafeFixture(t)
	kit := filepath.Join(t.TempDir(), "kit")
	gitCLI(t, filepath.Dir(kit), "init", "-q", "-b", "main", kit)
	writeFile(t, filepath.Join(kit, "k.txt"), "k\n")
	gitCLI(t, kit, "add", ".")
	gitCLI(t, kit, "commit", "-q", "-m", "kit")
	gitCLI(t, f.root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", kit, "kit")
	writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[settings]\nsubmodules = true\n")
	gitCLI(t, f.root, "add", ".")
	gitCLI(t, f.root, "commit", "-q", "-m", "kit submodule")

	ws := f.create("kitws")
	requireExists(t, filepath.Join(ws, "kit", "k.txt"), true)
	if got := strings.TrimSpace(gitOut(t, filepath.Join(ws, "kit"), "remote", "get-url", "origin")); got != kit {
		t.Fatalf("kit remote not restored to .gitmodules url: %s", got)
	}
	writeFile(t, filepath.Join(ws, "kit", "new.txt"), "n\n")
	gitCLI(t, filepath.Join(ws, "kit"), "add", ".")
	gitCLI(t, filepath.Join(ws, "kit"), "commit", "-q", "-m", "unpushed")
	gitCLI(t, ws, "add", "kit")
	gitCLI(t, ws, "commit", "-q", "-m", "bump kit")
	f.refused("submodule-unpushed", "remove", "--json", "kitws")
	requireExists(t, ws, true)
	gitCLI(t, ws, "reset", "-q", "--hard", "HEAD~1")
	gitCLI(t, filepath.Join(ws, "kit"), "reset", "-q", "--hard", "HEAD~1")
	f.mustRun("remove", "kitws")
	requireExists(t, ws, false)
}
