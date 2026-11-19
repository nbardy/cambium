package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/workspace"
)

// Gap 1: a clean workspace holding a commit that no remote and no other branch
// holds is never removed silently.
func TestRemoveRefusesOrSalvagesUnpushedCommits(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("work")
	writeFile(t, filepath.Join(path, "done.txt"), "only here\n")
	gitCLI(t, path, "add", ".")
	gitCLI(t, path, "commit", "-q", "-m", "unpushed work")
	head := gitOut(t, path, "rev-parse", "HEAD")

	detail := f.refused("unpushed", "remove", "--json", "work")
	if !strings.Contains(detail["detail"].(string), "1 unpushed commit") {
		t.Fatalf("refusal must say how many commits: %v", detail)
	}
	requireExists(t, path, true)
	// A refusal also holds under --delete-branch, the case that would lose the commit outright.
	f.refused("unpushed", "remove", "--json", "--delete-branch", "work")
	requireExists(t, path, true)

	out := f.mustRun("remove", "--json", "--salvage-ref", "refs/salvage/work", "work")
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["salvageRef"] != "refs/salvage/work" {
		t.Fatalf("salvage must be reported: %q (%v)", out, err)
	}
	requireExists(t, path, false)
	if got := gitOut(t, f.root, "rev-parse", "refs/salvage/work"); got != head {
		t.Fatalf("salvage ref holds %s, want the workspace HEAD %s", got, head)
	}
}

func TestRemoveAllowsCommitsHeldElsewhere(t *testing.T) {
	f := newSafeFixture(t)
	// Held by another branch: nothing is stranded.
	merged := f.create("merged-local")
	writeFile(t, filepath.Join(merged, "m.txt"), "m\n")
	gitCLI(t, merged, "add", ".")
	gitCLI(t, merged, "commit", "-q", "-m", "m")
	gitCLI(t, f.root, "merge", "-q", "--ff-only", gitOut(t, merged, "rev-parse", "HEAD"))
	f.mustRun("remove", "merged-local")
	requireExists(t, merged, false)

	// Held by a remote-tracking ref: pushed.
	remote := t.TempDir()
	gitCLI(t, remote, "init", "-q", "--bare", "-b", "main")
	gitCLI(t, f.root, "remote", "add", "origin", remote)
	pushed := f.create("pushed")
	writeFile(t, filepath.Join(pushed, "p.txt"), "p\n")
	gitCLI(t, pushed, "add", ".")
	gitCLI(t, pushed, "commit", "-q", "-m", "p")
	f.refused("unpushed", "remove", "--json", "pushed")
	gitCLI(t, pushed, "push", "-q", "origin", "HEAD")
	f.mustRun("remove", "pushed")
	requireExists(t, pushed, false)
}

// Gap 1 for prune and maintain: an unmerged workspace with unpushed commits is
// kept by both, and gc (without --force) skips it too.
func TestPruneMaintainAndGCKeepUnpushedWork(t *testing.T) {
	f := newSafeFixture(t)
	path := f.createEphemeral("wip")
	writeFile(t, filepath.Join(path, "w.txt"), "w\n")
	gitCLI(t, path, "add", ".")
	gitCLI(t, path, "commit", "-q", "-m", "wip")
	f.mustRun("prune", "--merged", "main", "--older-than", "1ns")
	f.mustRun("maintain", "--merged", "main", "--older-than", "1ns")
	f.mustRun("gc", "--ephemeral", "--older-than", "1ns")
	requireExists(t, path, true)
	// gc with nothing unpushed removes it only after the commit is held elsewhere.
	gitCLI(t, f.root, "merge", "-q", "--ff-only", gitOut(t, path, "rev-parse", "HEAD"))
	f.mustRun("gc", "--ephemeral", "--older-than", "1ns")
	requireExists(t, path, false)
}

// Gap 3: --dry-run lists exactly what the real run removes, a dirty workspace included.
func TestPruneMergedDryRunMatchesRealRun(t *testing.T) {
	f := newSafeFixture(t)
	clean := f.createEphemeral("clean-one")
	dirty := f.createEphemeral("dirty-one")
	writeFile(t, filepath.Join(dirty, "scratch.txt"), "uncommitted\n")
	var dry, real workspace.PruneMergedResult
	if err := json.Unmarshal([]byte(f.mustRun("prune", "--merged", "main", "--dry-run", "--json")), &dry); err != nil {
		t.Fatal(err)
	}
	if strings.Join(dry.Selected, ",") != "clean-one" {
		t.Fatalf("dry run must not select the dirty workspace: %+v", dry)
	}
	requireExists(t, clean, true)
	text := f.mustRun("prune", "--merged", "main", "--dry-run")
	if strings.Contains(text, "would-remove\tdirty-one") || !strings.Contains(text, "would-remove\tclean-one") || !strings.Contains(text, "kept\tdirty-one") {
		t.Fatalf("dry-run text: %s", text)
	}
	if err := json.Unmarshal([]byte(f.mustRun("prune", "--merged", "main", "--json")), &real); err != nil {
		t.Fatal(err)
	}
	if strings.Join(real.Removed, ",") != strings.Join(dry.Selected, ",") {
		t.Fatalf("dry run %v differs from real run %v", dry.Selected, real.Removed)
	}
	requireExists(t, dirty, true)
}

// Gap 2: a repository outside repos_root is found through the user registry.
func TestNameResolutionUsesTheGlobalRegistry(t *testing.T) {
	t.Setenv("CAMBIUM_STATE_DIR", t.TempDir())
	userConfig(t, "repos_root = \""+t.TempDir()+"\"\n") // nowhere near the repository
	f := newSafeFixture(t)
	path := f.create("far")
	outside, stdout, stderr := testApp(t.TempDir())
	if code := outside.Run(context.Background(), []string{"path", "far"}); code != 0 || strings.TrimSpace(stdout.String()) != path {
		t.Fatalf("path via registry: code=%d out=%q err=%s", code, stdout.String(), stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := outside.Run(context.Background(), []string{"inspect", "--json", "far"}); code != 0 {
		t.Fatalf("inspect via registry: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := outside.Run(context.Background(), []string{"path", "no-such-workspace"}); code == 0 {
		t.Fatal("an unknown name must fail")
	}
	for _, want := range []string{"no-such-workspace", "registered", "registry", "from inside the owning repository"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("error must name the fix (%q missing): %s", want, stderr.String())
		}
	}
	resetBuffers(stdout, stderr)
	if code := outside.Run(context.Background(), []string{"remove", "far"}); code != 0 {
		t.Fatalf("remove via registry: %s", stderr.String())
	}
	requireExists(t, path, false)
}

// Gap 4: a protected repository is never touched by maintain, prune, gc or the
// sweep on add; an unprotected one in the same run still is.
func TestProtectedRepositoriesAreNeverSwept(t *testing.T) {
	t.Setenv("CAMBIUM_STATE_DIR", t.TempDir())
	userConfig(t, "repos_root = \""+t.TempDir()+"\"\n")
	finishedIn := func(f *safeFixture, name string) string {
		writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[settings]\nmerge_target = \"main\"\nprune_merged_on_add = true\n")
		path := f.createEphemeral(name)
		writeFile(t, filepath.Join(path, name+".txt"), "x\n")
		gitCLI(t, path, "add", ".")
		gitCLI(t, path, "commit", "-q", "-m", name)
		gitCLI(t, f.root, "merge", "-q", "--ff-only", gitOut(t, path, "rev-parse", "HEAD"))
		return path
	}

	byKey := newSafeFixture(t) // protect = true in the clone-local config
	byKey.mustRun("init", "--force", "--detect-layers=false", "--protect")
	keyed := finishedIn(byKey, "keyed")
	byList := newSafeFixture(t) // listed in the per-user protected_repos
	listed := finishedIn(byList, "listed")
	open := newSafeFixture(t) // unprotected control
	swept := finishedIn(open, "swept")
	userConfig(t, "repos_root = \""+t.TempDir()+"\"\nprotected_repos = [\""+byList.root+"\"]\n")

	for _, f := range []*safeFixture{byKey, byList} {
		if code := f.run("prune", "--merged", "main"); code != 3 || !strings.Contains(f.stderr.String(), "protected") {
			t.Fatalf("prune --merged in a protected repository: exit %d, %s", code, f.stderr.String())
		}
		if code := f.run("prune", "--older-than", "1ns"); code != 3 {
			t.Fatalf("cache prune in a protected repository: exit %d", code)
		}
		if code := f.run("gc", "--ephemeral", "--older-than", "1ns"); code != 3 {
			t.Fatalf("gc in a protected repository: exit %d", code)
		}
		f.mustRun("maintain")
	}
	out := open.mustRun("maintain", "--all-repos", "--json")
	var reports []MaintainReport
	if err := json.Unmarshal([]byte(out), &reports); err != nil {
		t.Fatal(err)
	}
	protected := 0
	for _, report := range reports {
		if report.Protected != "" {
			protected++
		}
	}
	if protected != 2 || len(reports) != 3 {
		t.Fatalf("want 3 repositories, 2 protected: %s", out)
	}
	requireExists(t, keyed, true)
	requireExists(t, listed, true)
	requireExists(t, swept, false)
	// The sweep on add skips them too.
	byKey.createEphemeral("next")
	requireExists(t, keyed, true)
	// An explicit remove still runs its guards and works.
	byKey.mustRun("remove", "keyed")
	requireExists(t, keyed, false)
}

// Gap 5: --harvest-to is the rescue placement. Work worth keeping moves; what
// any tool rebuilds stays behind.
func TestHarvestToRescueDirectory(t *testing.T) {
	f := newSafeFixture(t)
	writeFile(t, filepath.Join(f.root, ".gitignore"), "output/\nnode_modules/\ntarget/\n")
	gitCLI(t, f.root, "add", ".gitignore")
	gitCLI(t, f.root, "commit", "-q", "-m", "ignore deps")
	path := f.create("rescue-me")
	for rel, body := range map[string]string{
		"output/run.log":                         "evidence",
		"target/debug/app":                       "binary",
		"node_modules/pkg/index.js":              "dependency",
		"target/debug/incremental/x/query-cache": "compiler",
		"target/debug/.fingerprint/y/lib-y":      "compiler",
		"target/debug/deps/libz-1.rlib":          "compiler",
	} {
		writeFile(t, filepath.Join(path, rel), body)
	}
	dest := filepath.Join(t.TempDir(), "rescued", "rescue-me")
	if code := f.run("remove", "--harvest", "..", "--harvest-to", dest, "rescue-me"); code == 0 {
		t.Fatal("a harvest directory that climbs out must be refused")
	}
	f.mustRun("remove", "--json", "--harvest", ".", "--harvest-to", dest, "rescue-me")
	requireExists(t, path, false)
	for rel, want := range map[string]bool{
		"output/run.log": true, "target/debug/app": true,
		"node_modules/pkg/index.js": false, "target/debug/incremental/x/query-cache": false,
		"target/debug/.fingerprint/y/lib-y": false, "target/debug/deps/libz-1.rlib": false,
	} {
		requireExists(t, filepath.Join(dest, rel), want)
	}
}

// Gap 6: scrub_env keeps a repository hook from seeing the caller's secrets,
// and from running at all.
func TestScrubEnvDisablesHooksAndSecrets(t *testing.T) {
	t.Setenv("PROVIDER_API_KEY", "sk-secret")
	run := func(scrub bool) string {
		f := newSafeFixture(t)
		seen := filepath.Join(t.TempDir(), "seen")
		hook := filepath.Join(f.root, ".git", "hooks", "post-checkout")
		writeFile(t, hook, "#!/bin/sh\necho \"key=$PROVIDER_API_KEY\" >> "+seen+"\n")
		if err := os.Chmod(hook, 0o755); err != nil {
			t.Fatal(err)
		}
		if scrub {
			f.mustRun("init", "--force", "--detect-layers=false")
			writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[settings]\nscrub_env = true\n")
		}
		f.mustRun("create", "--materializer", "git", "hooked")
		data, _ := os.ReadFile(seen)
		return string(data)
	}
	if got := run(false); !strings.Contains(got, "key=sk-secret") {
		t.Fatalf("control: without scrub_env the hook runs with the caller's environment, saw %q", got)
	}
	if got := run(true); got != "" {
		t.Fatalf("scrub_env must stop the hook and hide the key, saw %q", got)
	}
}

// Inherited repository selectors (set by a Git hook that calls Cambium) never
// redirect Cambium's own Git calls.
func TestInheritedGitDirDoesNotRedirectCambium(t *testing.T) {
	f := newSafeFixture(t)
	other := initializeCLIRepository(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	path := f.create("selected")
	os.Unsetenv("GIT_DIR") // the test's own git calls must see the real repositories
	os.Unsetenv("GIT_WORK_TREE")
	if got := gitOut(t, path, "rev-parse", "--show-toplevel"); canon(t, got) != canon(t, path) {
		t.Fatalf("workspace belongs to the wrong repository: %s", got)
	}
}

// Gap 4, wiring: a caller with its own protection policy (cleanup_tools'
// disk_policy.py) excludes repositories per run, and nothing under them moves.
func TestMaintainExcludeLeavesNamedRepositoriesAlone(t *testing.T) {
	t.Setenv("CAMBIUM_STATE_DIR", t.TempDir())
	userConfig(t, "repos_root = \""+t.TempDir()+"\"\n")
	finishedIn := func(f *safeFixture, name string) string {
		writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[settings]\nmerge_target = \"main\"\n")
		path := f.createEphemeral(name)
		writeFile(t, filepath.Join(path, name+".txt"), "x\n")
		gitCLI(t, path, "add", ".")
		gitCLI(t, path, "commit", "-q", "-m", name)
		gitCLI(t, f.root, "merge", "-q", "--ff-only", gitOut(t, path, "rev-parse", "HEAD"))
		return path
	}
	kept, swept := newSafeFixture(t), newSafeFixture(t)
	keptPath, sweptPath := finishedIn(kept, "kept"), finishedIn(swept, "swept")
	swept.mustRun("maintain", "--all-repos", "--merged", "main", "--exclude", kept.root)
	requireExists(t, keptPath, true)
	requireExists(t, sweptPath, false)
}

// remove --dry-run is the "check NAME" wrappers had: same refusal, no change.
func TestRemoveDryRunRefusesLikeTheRealRemoveAndChangesNothing(t *testing.T) {
	f := newSafeFixture(t)
	clean := f.create("clean")
	dirty := f.create("dirty")
	writeFile(t, filepath.Join(dirty, "x.txt"), "x\n")
	if code := f.run("remove", "--dry-run", "dirty"); code != 3 {
		t.Fatalf("dry-run on a dirty workspace: exit %d, want 3", code)
	}
	f.mustRun("remove", "--dry-run", "clean")
	requireExists(t, clean, true)
	requireExists(t, filepath.Join(dirty, "x.txt"), true)
	f.mustRun("remove", "clean")
	requireExists(t, clean, false)
}
