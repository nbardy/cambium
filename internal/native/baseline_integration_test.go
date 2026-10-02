package native_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

// Regression guard for baseline disk cost. Before derived baselines, every new
// commit paid a full checkout (812 MB per agent commit in a 761 MB repo,
// 2026-10-01). This asserts the derived path is actually taken for a small
// diff, and that its result is indistinguishable from a fresh checkout:
// modified, deleted, and added paths, plus a clean `git status` in a workspace.
func TestNewCommitBaselineIsDerivedFromNearestBaseline(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := initializeRepository(t, false)
	if !(native.Cloner{}).Probe(ctx, filepath.Join(root, ".git")).Supported {
		t.Skip("copy-on-write unavailable; derived baselines need it")
	}
	for i := 0; i < 20; i++ {
		mustWrite(t, filepath.Join(root, "src", "file"+string(rune('a'+i))+".txt"), strings.Repeat("x", 4096), 0o644)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "more files")
	backend := openBackend(t, ctx, root)

	first, err := backend.Prepare(ctx, "HEAD", model.MaterializerCoW, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Baseline.Source != "checkout" {
		t.Fatalf("first baseline source = %q, want checkout", first.Baseline.Source)
	}

	mustWrite(t, filepath.Join(root, "src", "a.txt"), "changed\n", 0o644)
	git(t, root, "rm", "-q", filepath.Join("src", "fileb.txt"))
	mustWrite(t, filepath.Join(root, "src", "added.txt"), "new\n", 0o644)
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "small change")

	second, err := backend.Prepare(ctx, "HEAD", model.MaterializerCoW, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Baseline.Source != "derived:"+first.Commit {
		t.Fatalf("second baseline source = %q, want derived:%s", second.Baseline.Source, first.Commit)
	}

	workspace, err := backend.Create(ctx, native.CreateSpec{Name: "derived", Ref: "HEAD", Materializer: model.MaterializerCoW})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Remove(context.Background(), workspace.Name, true, true) })
	if data := mustRead(t, filepath.Join(workspace.Path, "src", "a.txt")); data != "changed\n" {
		t.Fatalf("modified file = %q", data)
	}
	if data := mustRead(t, filepath.Join(workspace.Path, "src", "added.txt")); data != "new\n" {
		t.Fatalf("added file = %q", data)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "src", "fileb.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file survived in derived baseline: %v", err)
	}
	if status := git(t, workspace.Path, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("derived workspace is not clean:\n%s", status)
	}
}
