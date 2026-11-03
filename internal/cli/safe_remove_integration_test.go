package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These run the real CLI against real temp repositories. Each refusal test
// asserts both the typed reason and that the workspace survived: the failure
// mode being guarded is silent data loss, not a wrong message.

type safeFixture struct {
	t      *testing.T
	root   string
	app    *App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newSafeFixture(t *testing.T) *safeFixture {
	t.Helper()
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	writeFile(t, filepath.Join(root, ".gitignore"), "output/\n")
	gitCLI(t, root, "add", ".gitignore")
	gitCLI(t, root, "commit", "-q", "-m", "ignore output")
	app, stdout, stderr := testApp(root)
	f := &safeFixture{t: t, root: root, app: app, stdout: stdout, stderr: stderr}
	f.mustRun("init", "--detect-layers=false")
	return f
}

func (f *safeFixture) run(args ...string) int {
	resetBuffers(f.stdout, f.stderr)
	return f.app.Run(context.Background(), args)
}

func (f *safeFixture) mustRun(args ...string) string {
	f.t.Helper()
	if code := f.run(args...); code != 0 {
		f.t.Fatalf("%v failed (%d): %s", args, code, f.stderr.String())
	}
	return f.stdout.String()
}

func (f *safeFixture) create(name string) string {
	f.t.Helper()
	f.mustRun("create", "--materializer", "git", name)
	return strings.TrimSpace(f.mustRun("path", name))
}

// refused runs args, requires exit 3 and the JSON reason, and returns detail.
func (f *safeFixture) refused(reason string, args ...string) map[string]any {
	f.t.Helper()
	if code := f.run(args...); code != 3 {
		f.t.Fatalf("%v: want exit 3 (%s), got %d; stdout=%s stderr=%s", args, reason, code, f.stdout.String(), f.stderr.String())
	}
	var value map[string]any
	if err := json.Unmarshal(f.stdout.Bytes(), &value); err != nil {
		f.t.Fatalf("refusal JSON: %v: %s", err, f.stdout.String())
	}
	if value["reason"] != reason {
		f.t.Fatalf("want reason %q, got %v", reason, value)
	}
	return value
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func requireExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if got := err == nil; got != want {
		t.Fatalf("%s exists=%t, want %t (%v)", path, got, want, err)
	}
}

func TestSafeRemoveDirtyRefusesThenSalvagesUntracked(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("agent")
	writeFile(t, filepath.Join(path, "notes", "untracked.txt"), "only copy\n")

	f.refused("dirty", "remove", "--safe", "--json", "agent")
	requireExists(t, filepath.Join(path, "notes", "untracked.txt"), true)

	if code := f.run("remove", "--safe", "--force", "agent"); code != 1 {
		t.Fatalf("--safe --force must be a usage error, got %d", code)
	}

	out := f.mustRun("remove", "--safe", "--json", "--salvage-ref", "refs/salvage/agent", "agent")
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["removed"] != true || result["salvageRef"] != "refs/salvage/agent" {
		t.Fatalf("unexpected success JSON %q (%v)", out, err)
	}
	requireExists(t, path, false)
	if got := gitOut(t, f.root, "show", "refs/salvage/agent:notes/untracked.txt"); got != "only copy" {
		t.Fatalf("salvage commit lost the untracked file: %q", got)
	}
	// The salvage commit sits on the workspace HEAD, so it is diffable against it.
	if parent := gitOut(t, f.root, "rev-parse", "refs/salvage/agent^"); parent != gitOut(t, f.root, "rev-parse", "main") {
		t.Fatalf("salvage parent %s is not the workspace HEAD", parent)
	}
}

func TestSafeRemoveRefusesInProgressAndUnmerged(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("agent")

	marker := gitOut(t, path, "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	writeFile(t, marker, gitOut(t, path, "rev-parse", "HEAD")+"\n")
	f.refused("in-progress", "remove", "--safe", "--json", "agent")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(path, "src", "b.txt"), "work\n")
	gitCLI(t, path, "add", ".")
	gitCLI(t, path, "commit", "-q", "-m", "agent work")
	f.refused("unmerged", "remove", "--safe", "--json", "--require-merged", "main", "agent")
	requireExists(t, path, true)

	gitCLI(t, f.root, "merge", "-q", "--no-ff", "-m", "integrate", gitOut(t, path, "rev-parse", "--abbrev-ref", "HEAD"))
	f.mustRun("remove", "--safe", "--json", "--require-merged", "main", "agent")
	requireExists(t, path, false)
}

func TestSafeRemoveHarvestsIgnoredOutput(t *testing.T) {
	f := newSafeFixture(t)
	path := f.create("agent")
	writeFile(t, filepath.Join(path, "output", "run1", "gauges.bin"), "paid GPU bytes")
	writeFile(t, filepath.Join(path, "output", "shared.txt"), "same")
	writeFile(t, filepath.Join(f.root, "output", "shared.txt"), "different")

	// A differing destination refuses the whole harvest: nothing moves.
	f.refused("output-conflict", "remove", "--safe", "--json", "--harvest", "output", "agent")
	requireExists(t, filepath.Join(path, "output", "run1", "gauges.bin"), true)
	requireExists(t, filepath.Join(f.root, "output", "run1", "gauges.bin"), false)

	writeFile(t, filepath.Join(f.root, "output", "shared.txt"), "same")
	out := f.mustRun("remove", "--safe", "--json", "--harvest", "output", "agent")
	var result struct {
		Removed   bool `json:"removed"`
		Harvested struct {
			Moved        int `json:"moved"`
			Deduplicated int `json:"deduplicated"`
		} `json:"harvested"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Removed || result.Harvested.Moved != 1 || result.Harvested.Deduplicated != 1 {
		t.Fatalf("unexpected harvest result %s", out)
	}
	data, err := os.ReadFile(filepath.Join(f.root, "output", "run1", "gauges.bin"))
	if err != nil || string(data) != "paid GPU bytes" {
		t.Fatalf("harvested output missing from primary: %q %v", data, err)
	}
	requireExists(t, path, false)
}

func TestArchivePushesVerifiedTagThenDeletesBranch(t *testing.T) {
	f := newSafeFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCLI(t, f.root, "init", "-q", "--bare", remote)
	gitCLI(t, f.root, "remote", "add", "origin", remote)
	gitCLI(t, f.root, "branch", "feature")
	sha := gitOut(t, f.root, "rev-parse", "feature")

	f.refused("checked-out", "archive", "--json", "main", "archive/main")
	f.refused("missing", "archive", "--json", "no-such-branch", "archive/none")

	var result map[string]any
	if err := json.Unmarshal([]byte(f.mustRun("archive", "--json", "feature", "archive/feature")), &result); err != nil {
		t.Fatal(err)
	}
	if result["archived"] != true || result["sha"] != sha {
		t.Fatalf("unexpected archive result %v", result)
	}
	if got := gitOut(t, remote, "rev-parse", "refs/tags/archive/feature"); got != sha {
		t.Fatalf("remote tag %s, want %s", got, sha)
	}
	if out, err := exec.Command("git", "-C", f.root, "rev-parse", "--verify", "-q", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("branch survived archive: %s", out)
	}

	if err := json.Unmarshal([]byte(f.mustRun("archive", "--json", "feature", "archive/feature")), &result); err != nil {
		t.Fatal(err)
	}
	if result["archived"] != true || result["already"] != true {
		t.Fatalf("rerun was not idempotent: %v", result)
	}

	// A local tag at a different commit must never be "verified" by pushing.
	gitCLI(t, f.root, "branch", "other", "main~1")
	gitCLI(t, f.root, "tag", "taken", "main")
	f.refused("tag-conflict", "archive", "--json", "other", "taken")
	gitOut(t, f.root, "rev-parse", "--verify", "refs/heads/other")
}
