package speculative

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/gitx"
)

func TestGitMerkleCheckpointRestoreDiffAndRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	ctx := context.Background()
	repoPath := initGitRepo(t, 32)
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)

	writeFile(t, filepath.Join(worktree, "src", "file-003.txt"), "changed\n", 0o644)
	if err := os.Remove(filepath.Join(worktree, "src", "file-004.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(worktree, "new.sh"), "#!/bin/sh\necho hi\n", 0o755)
	if err := os.Symlink("new.sh", filepath.Join(worktree, "new-link")); err != nil {
		t.Fatal(err)
	}

	first, err := store.Capture(ctx, worktree, Ref{Name: "experiment/base", Message: "dirty state", SourceWorkspace: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Root.ID.IsZero() || len(first.ChangedPaths) != 4 {
		t.Fatalf("unexpected checkpoint: %#v", first)
	}
	if first.Root.BaseCommit != strings.TrimSpace(runGit(t, worktree, "rev-parse", "HEAD")) {
		t.Fatalf("wrong base: %#v", first.Root)
	}
	if got := strings.TrimSpace(runGit(t, repoPath, "rev-parse", first.Root.ID.String()+"^")); got != first.Root.BaseCommit {
		t.Fatalf("root parent=%s want=%s", got, first.Root.BaseCommit)
	}
	baseDocs := strings.TrimSpace(runGit(t, repoPath, "rev-parse", first.Root.BaseCommit+":docs"))
	rootDocs := strings.TrimSpace(runGit(t, repoPath, "rev-parse", first.Root.ID.String()+":docs"))
	if baseDocs != rootDocs {
		t.Fatalf("unchanged subtree not shared: %s != %s", baseDocs, rootDocs)
	}

	// The deterministic commit metadata makes state identity independent of ref
	// name, message, and checkpoint time.
	repeated, err := store.Capture(ctx, worktree, Ref{Name: "experiment/repeated", Message: "different metadata"})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Root.ID != first.Root.ID || repeated.Root.Tree != first.Root.Tree {
		t.Fatalf("same state produced different roots: %#v %#v", first.Root, repeated.Root)
	}

	refs, err := store.ListRefs(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("list refs: %#v %v", refs, err)
	}
	resolvedID, resolvedRoot, resolvedRef, err := store.Resolve(ctx, "experiment/base")
	if err != nil || resolvedID != first.Root.ID || resolvedRoot != first.Root || resolvedRef == nil || resolvedRef.Name != "experiment/base" {
		t.Fatalf("resolve: id=%s root=%#v ref=%#v err=%v", resolvedID, resolvedRoot, resolvedRef, err)
	}

	restorePath := filepath.Join(t.TempDir(), "restored")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "restored", restorePath, first.Root.BaseCommit)
	defer runGit(t, repoPath, "worktree", "remove", "--force", restorePath)
	applied, err := store.Restore(ctx, first.Root, restorePath)
	if err != nil || applied != 4 {
		t.Fatalf("restore applied=%d err=%v", applied, err)
	}
	assertFile(t, filepath.Join(restorePath, "src", "file-003.txt"), "changed\n")
	if _, err := os.Stat(filepath.Join(restorePath, "src", "file-004.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file restored: %v", err)
	}
	assertFile(t, filepath.Join(restorePath, "new.sh"), "#!/bin/sh\necho hi\n")
	info, err := os.Stat(filepath.Join(restorePath, "new.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode restore: %#v %v", info, err)
	}
	target, err := os.Readlink(filepath.Join(restorePath, "new-link"))
	if err != nil || target != "new.sh" {
		t.Fatalf("symlink restore: %q %v", target, err)
	}
	if status := strings.TrimSpace(runGit(t, restorePath, "status", "--porcelain")); status == "" {
		t.Fatal("restored root should remain dirty relative to the base branch")
	}

	writeFile(t, filepath.Join(worktree, "src", "file-003.txt"), "changed again\n", 0o644)
	second, err := store.Capture(ctx, worktree, Ref{Name: "experiment/second", Parent: first.Root.ID})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := store.Diff(ctx, first.Root.ID, second.Root.ID)
	if err != nil || len(diff.Changes) != 1 || diff.Changes[0].Path != "src/file-003.txt" || diff.Engine != "git-merkle-tree" {
		t.Fatalf("unexpected diff: %#v err=%v", diff, err)
	}

	if output := runGit(t, repoPath, "fsck", "--strict", "--no-reflogs"); strings.Contains(strings.ToLower(output), "error") {
		t.Fatalf("git fsck reported error: %s", output)
	}
}

func TestRootGCReleasesOnlyUnprotectedHiddenRefs(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 4)
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)

	writeFile(t, filepath.Join(worktree, "src", "file-000.txt"), "one\n", 0o644)
	one, err := store.Capture(ctx, worktree, Ref{Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(worktree, "src", "file-000.txt"), "two\n", 0o644)
	two, err := store.Capture(ctx, worktree, Ref{Name: "two", Parent: one.Root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	// one remains protected as the parent of the live ref two.
	gc, err := store.GC(ctx, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) != 0 {
		t.Fatalf("released parent root: %#v", gc)
	}
	if err := store.DropRef(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	gc, err = store.GC(ctx, []ID{two.Root.ID}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) != 0 || gc.RetainedRoots != 2 {
		t.Fatalf("protected root lineage was not retained: %#v", gc)
	}
	gc, err = store.GC(ctx, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) != 2 {
		t.Fatalf("wrong final GC result: %#v", gc)
	}
	released := map[ID]bool{}
	for _, id := range gc.ReleasedRoots {
		released[id] = true
	}
	if !released[one.Root.ID] || !released[two.Root.ID] {
		t.Fatalf("missing released roots: %#v", gc.ReleasedRoots)
	}
	// Deleting Cambium's hidden ref does not run git prune; Git remains the
	// authority for object retention policy.
	if _, err := repository.ResolveCommit(ctx, two.Root.ID.String()); err != nil {
		t.Fatalf("root object was pruned unexpectedly: %v", err)
	}
}

func TestParseNameStatus(t *testing.T) {
	changes, err := parseNameStatus("M\x00a.txt\x00R100\x00old.txt\x00new.txt\x00")
	if err != nil || len(changes) != 2 {
		t.Fatalf("parse: %#v %v", changes, err)
	}
	if changes[1].Status != "R100" || changes[1].FromPath != "old.txt" || changes[1].Path != "new.txt" {
		t.Fatalf("rename parse: %#v", changes[1])
	}
}

func initGitRepo(t *testing.T, files int) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q", "-b", "main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Cambium Test")
	for index := 0; index < files; index++ {
		writeFile(t, filepath.Join(root, "src", fmtIndex(index)), "original\n", 0o644)
	}
	writeFile(t, filepath.Join(root, "docs", "unchanged.md"), "shared subtree\n", 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "initial")
	return root
}

func fmtIndex(index int) string { return fmt.Sprintf("file-%03d.txt", index) }

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("%s: got %q err=%v want=%q", path, data, err, expected)
	}
}

func TestRootMetadataAgeGuard(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 1)
	repository, _ := gitx.Discover(ctx, repoPath, nil)
	store, _ := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)
	writeFile(t, filepath.Join(worktree, "src", "file-000.txt"), "dirty\n", 0o644)
	checkpoint, err := store.Capture(ctx, worktree, Ref{Name: "young"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, "young"); err != nil {
		t.Fatal(err)
	}
	gc, err := store.GC(ctx, nil, time.Hour, false)
	if err != nil || len(gc.ReleasedRoots) != 0 || gc.RetainedRoots != 1 {
		t.Fatalf("young root not retained: %#v %v", gc, err)
	}
	if _, err := os.Stat(store.rootMetadataPath(checkpoint.Root.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureLeavesRealIndexUntouchedAndSnapshotsVisibleFiles(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 2)
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent-index", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)

	path := filepath.Join(worktree, "src", "file-000.txt")
	writeFile(t, path, "staged value\n", 0o644)
	runGit(t, worktree, "add", "src/file-000.txt")
	stagedBefore := strings.TrimSpace(runGit(t, worktree, "rev-parse", ":src/file-000.txt"))
	writeFile(t, path, "visible value\n", 0o644)

	checkpoint, err := store.Capture(ctx, worktree, Ref{Name: "index-isolation"})
	if err != nil {
		t.Fatal(err)
	}
	stagedAfter := strings.TrimSpace(runGit(t, worktree, "rev-parse", ":src/file-000.txt"))
	if stagedBefore != stagedAfter {
		t.Fatalf("real index changed: %s != %s", stagedBefore, stagedAfter)
	}
	rootBlob := strings.TrimSpace(runGit(t, repoPath, "show", checkpoint.Root.ID.String()+":src/file-000.txt"))
	if rootBlob != "visible value" {
		t.Fatalf("checkpoint captured %q, want visible worktree value", rootBlob)
	}
	cached := strings.TrimSpace(runGit(t, worktree, "show", ":src/file-000.txt"))
	if cached != "staged value" {
		t.Fatalf("staged content changed: %q", cached)
	}
}

func TestCaptureExcludesIgnoredEnvironmentState(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 1)
	writeFile(t, filepath.Join(repoPath, ".gitignore"), "node_modules/\n", 0o644)
	runGit(t, repoPath, "add", ".gitignore")
	runGit(t, repoPath, "commit", "-q", "-m", "ignore environment")
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent-ignore", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)
	writeFile(t, filepath.Join(worktree, "node_modules", "package", "index.js"), "ignored\n", 0o644)

	checkpoint, err := store.Capture(ctx, worktree, Ref{Name: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.ChangedPaths) != 0 {
		t.Fatalf("ignored environment leaked into checkpoint: %#v", checkpoint.ChangedPaths)
	}
	baseTree := strings.TrimSpace(runGit(t, repoPath, "rev-parse", checkpoint.Root.BaseCommit+"^{tree}"))
	if checkpoint.Root.Tree != baseTree {
		t.Fatalf("ignored state changed tree: %s != %s", checkpoint.Root.Tree, baseTree)
	}
}

func TestRenameCheckpointAndRestore(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 3)
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent-rename", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)
	if err := os.Rename(filepath.Join(worktree, "src", "file-001.txt"), filepath.Join(worktree, "src", "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.Capture(ctx, worktree, Ref{Name: "rename"})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := store.ChangesFromBase(ctx, checkpoint.Root)
	if err != nil || len(changes) != 1 || !strings.HasPrefix(changes[0].Status, "R") || changes[0].FromPath != "src/file-001.txt" || changes[0].Path != "src/renamed.txt" {
		t.Fatalf("rename change: %#v %v", changes, err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "restored-rename", restored, checkpoint.Root.BaseCommit)
	defer runGit(t, repoPath, "worktree", "remove", "--force", restored)
	if _, err := store.Restore(ctx, checkpoint.Root, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(restored, "src", "file-001.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old rename path remains: %v", err)
	}
	assertFile(t, filepath.Join(restored, "src", "renamed.txt"), "original\n")
}

func TestCheckpointAndRestoreUnusualGitPaths(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 1)
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "agent-paths", worktree, "HEAD")
	defer runGit(t, repoPath, "worktree", "remove", "--force", worktree)

	paths := map[string]string{
		"spaces and unicode-한글.txt": "unicode\n",
		"tab\tname.txt":             "tab\n",
		"line\nbreak.txt":           "newline\n",
	}
	for path, content := range paths {
		writeFile(t, filepath.Join(worktree, path), content, 0o644)
	}
	checkpoint, err := store.Capture(ctx, worktree, Ref{Name: "unusual-paths"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.ChangedPaths) != len(paths) {
		t.Fatalf("changed paths=%q", checkpoint.ChangedPaths)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	runGit(t, repoPath, "worktree", "add", "-q", "-b", "restored-paths", restored, checkpoint.Root.BaseCommit)
	defer runGit(t, repoPath, "worktree", "remove", "--force", restored)
	if _, err := store.Restore(ctx, checkpoint.Root, restored); err != nil {
		t.Fatal(err)
	}
	for path, content := range paths {
		assertFile(t, filepath.Join(restored, path), content)
	}
}

func TestGitSHA256RepositoryCheckpoint(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "-q", "--object-format=sha256", "-b", "main")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("Git SHA-256 repositories are unavailable: %v\n%s", err, output)
	}
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Cambium Test")
	writeFile(t, filepath.Join(root, "value.txt"), "base\n", 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-q", "-m", "initial")
	repository, err := gitx.Discover(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "agent")
	runGit(t, root, "worktree", "add", "-q", "-b", "agent-sha256", worktree, "HEAD")
	defer runGit(t, root, "worktree", "remove", "--force", worktree)
	writeFile(t, filepath.Join(worktree, "value.txt"), "changed\n", 0o644)
	checkpoint, err := store.Capture(ctx, worktree, Ref{Name: "sha256"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Root.ID.String()) != 64 || len(checkpoint.Root.Tree) != 64 || len(checkpoint.Root.BaseCommit) != 64 {
		t.Fatalf("unexpected SHA-256 root: %#v", checkpoint.Root)
	}
	resolved, err := store.GetRoot(ctx, checkpoint.Root.ID)
	if err != nil || resolved != checkpoint.Root {
		t.Fatalf("resolve SHA-256 root: %#v %v", resolved, err)
	}
}

func TestGetRootRejectsOrdinaryCommitWithCambiumMessage(t *testing.T) {
	ctx := context.Background()
	repoPath := initGitRepo(t, 1)
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoPath, "ordinary.txt"), "not a root\n", 0o644)
	runGit(t, repoPath, "add", "ordinary.txt")
	runGit(t, repoPath, "commit", "-q", "-m", strings.TrimSpace(rootMessage))
	id, err := ParseID(strings.TrimSpace(runGit(t, repoPath, "rev-parse", "HEAD")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRoot(ctx, id); err == nil || !strings.Contains(err.Error(), "not a Cambium speculative root") {
		t.Fatalf("ordinary commit accepted as root: %v", err)
	}
}
