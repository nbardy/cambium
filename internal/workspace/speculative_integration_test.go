package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

func TestCheckpointAndForkRealGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	original, err := manager.Create(ctx, CreateSpec{Name: "original", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, "original", true, true)

	if err := os.WriteFile(filepath.Join(original.Path, "tracked.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(original.Path, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original.Path, "new.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new.sh", filepath.Join(original.Path, "new-link")); err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("abcdefghij0123456789"), 140000)
	if err := os.WriteFile(filepath.Join(original.Path, "large.bin"), large, 0o644); err != nil {
		t.Fatal(err)
	}

	checkpoint, err := manager.Checkpoint(ctx, "original", CheckpointOptions{RefName: "experiment/base", Message: "dirty agent state"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.ChangedPaths) != 5 || checkpoint.Root.IsZero() {
		t.Fatalf("unexpected checkpoint: %#v", checkpoint)
	}
	baseDocs := strings.TrimSpace(runGitTest(t, repo, "rev-parse", checkpoint.BaseCommit+":docs"))
	rootDocs := strings.TrimSpace(runGitTest(t, repo, "rev-parse", checkpoint.Root.String()+":docs"))
	if baseDocs != rootDocs {
		t.Fatalf("unchanged Git subtree was not structurally shared: %s vs %s", baseDocs, rootDocs)
	}

	forked, err := manager.Fork(ctx, "experiment/base", CreateSpec{Name: "forked", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, "forked", true, true)
	if forked.Root != checkpoint.Root || forked.Applied != 5 {
		t.Fatalf("unexpected fork result: %#v", forked)
	}
	assertFile(t, filepath.Join(forked.Workspace.Path, "tracked.txt"), "changed\n")
	if _, err := os.Stat(filepath.Join(forked.Workspace.Path, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file exists or stat failed unexpectedly: %v", err)
	}
	assertFile(t, filepath.Join(forked.Workspace.Path, "new.sh"), "#!/bin/sh\necho hi\n")
	info, err := os.Stat(filepath.Join(forked.Workspace.Path, "new.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode not restored: mode=%v err=%v", info, err)
	}
	target, err := os.Readlink(filepath.Join(forked.Workspace.Path, "new-link"))
	if err != nil || target != "new.sh" {
		t.Fatalf("symlink not restored: target=%q err=%v", target, err)
	}
	restoredLarge, err := os.ReadFile(filepath.Join(forked.Workspace.Path, "large.bin"))
	if err != nil || !bytes.Equal(restoredLarge, large) {
		t.Fatalf("large file not restored: bytes=%d err=%v", len(restoredLarge), err)
	}
	status, err := projectValue.Repository.StatusPorcelain(ctx, forked.Workspace.Path)
	if err != nil || strings.TrimSpace(status) == "" {
		t.Fatalf("fork should preserve a dirty overlay: %q %v", status, err)
	}

	// Re-checkpointing the restored overlay produces the same content root.
	replayed, err := manager.Checkpoint(ctx, "forked", CheckpointOptions{RefName: "experiment/replayed", Parent: "experiment/base"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Root != checkpoint.Root {
		t.Fatalf("deterministic replay changed root: %s vs %s", replayed.Root, checkpoint.Root)
	}

	// Primary checkout remains authoritative and untouched.
	assertFile(t, filepath.Join(repo, "tracked.txt"), "original\n")
	assertFile(t, filepath.Join(repo, "deleted.txt"), "delete me\n")
	for _, path := range []string{"new.sh", "new-link", "large.bin"} {
		if _, err := os.Lstat(filepath.Join(repo, path)); !os.IsNotExist(err) {
			t.Fatalf("primary checkout gained %s: %v", path, err)
		}
	}
}

func TestCheckpointDiffTracksOneSpeculativeEdit(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, _ := project.Open(ctx, repo, nil)
	manager := New(projectValue)
	workspaceValue, err := manager.Create(ctx, CreateSpec{Name: "agent", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, "agent", true, true)
	_ = os.WriteFile(filepath.Join(workspaceValue.Path, "tracked.txt"), []byte("one\n"), 0o644)
	first, err := manager.Checkpoint(ctx, "agent", CheckpointOptions{RefName: "first"})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(workspaceValue.Path, "tracked.txt"), []byte("two\n"), 0o644)
	second, err := manager.Checkpoint(ctx, "agent", CheckpointOptions{RefName: "second", Parent: "first"})
	if err != nil {
		t.Fatal(err)
	}
	store, _ := manager.SpecStore()
	diff, err := store.Diff(ctx, first.Root, second.Root)
	if err != nil || len(diff.Changes) != 1 || diff.Changes[0].Path != "tracked.txt" {
		t.Fatalf("unexpected diff: %#v err=%v", diff, err)
	}
}

func initSpecRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deleted.txt"), []byte("delete me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "unchanged.md"), []byte("shared subtree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "initial")
	return root
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func assertFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("%s: got %q err=%v, want %q", path, data, err, expected)
	}
}

func TestCheckpointAutomaticallyAdvancesWorkspaceRootAndLineage(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	workspaceValue, err := manager.Create(ctx, CreateSpec{Name: "lineage", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(workspaceValue.Path, "tracked.txt")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Checkpoint(ctx, "lineage", CheckpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := manager.Registry.Load("lineage")
	if err != nil {
		t.Fatal(err)
	}
	if stored.SpeculativeRoot != first.Root.String() || !first.Ref.Parent.IsZero() {
		t.Fatalf("first checkpoint provenance: workspace=%q ref=%#v", stored.SpeculativeRoot, first.Ref)
	}

	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Checkpoint(ctx, "lineage", CheckpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Root == first.Root || second.Ref.Parent != first.Root {
		t.Fatalf("checkpoint did not advance lineage: first=%s second=%#v", first.Root, second)
	}
	stored, err = manager.Registry.Load("lineage")
	if err != nil {
		t.Fatal(err)
	}
	if stored.SpeculativeRoot != second.Root.String() {
		t.Fatalf("workspace provenance=%q want=%s", stored.SpeculativeRoot, second.Root)
	}

	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, "lineage"); err != nil {
		t.Fatal(err)
	}
	gc, err := manager.RootGC(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) != 0 || gc.RetainedRoots != 2 {
		t.Fatalf("active workspace did not protect lineage: %#v", gc)
	}
	if err := manager.Remove(ctx, "lineage", true, true); err != nil {
		t.Fatal(err)
	}
	gc, err = manager.RootGC(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) != 2 {
		t.Fatalf("removed workspace lineage was not released: %#v", gc)
	}
}

func TestDetachedCheckpointStartsNewLineage(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	projectValue, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	workspaceValue, err := manager.Create(ctx, CreateSpec{Name: "detached", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, "detached", true, true)
	path := filepath.Join(workspaceValue.Path, "tracked.txt")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Checkpoint(ctx, "detached", CheckpointOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Checkpoint(ctx, "detached", CheckpointOptions{Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.Root == first.Root || !second.Ref.Parent.IsZero() {
		t.Fatalf("detached checkpoint retained a parent: first=%s second=%#v", first.Root, second.Ref)
	}
	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, "detached"); err != nil {
		t.Fatal(err)
	}
	gc, err := manager.RootGC(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) != 1 || gc.ReleasedRoots[0] != first.Root || gc.RetainedRoots != 1 {
		t.Fatalf("detached lineage GC: %#v", gc)
	}
}

func TestBranchCorrectEnvironmentReceiptAndCompositeFork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{\"name\":\"fixture\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{\"lockfileVersion\":3,\"version\":\"1\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "pkg", "version.txt"), []byte("main-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "main environment")
	runGitTest(t, root, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{\"lockfileVersion\":3,\"version\":\"2\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", "package-lock.json")
	runGitTest(t, root, "commit", "-m", "feature dependencies")
	runGitTest(t, root, "checkout", "main")

	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	feature, err := manager.Create(ctx, CreateSpec{Name: "feature-env", Ref: "feature", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, feature.Name, true, true)
	if feature.EnvironmentReady {
		t.Fatal("feature incorrectly received main's incompatible node_modules")
	}
	if _, err := os.Stat(filepath.Join(feature.Path, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("incompatible node_modules was materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(feature.Path, ".env")); !os.IsNotExist(err) {
		t.Fatalf("ignored secret was copied: %v", err)
	}
	assertFile(t, filepath.Join(feature.Path, "package-lock.json"), "{\"lockfileVersion\":3,\"version\":\"2\"}\n")

	if err := os.MkdirAll(filepath.Join(feature.Path, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(feature.Path, "node_modules", "pkg", "version.txt"), []byte("feature-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := manager.Checkpoint(ctx, feature.Name, CheckpointOptions{RefName: "feature/ready"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.EnvironmentIDs) == 0 {
		t.Fatal("checkpoint omitted environment receipts")
	}

	forked, err := manager.Fork(ctx, "feature/ready", CreateSpec{Name: "feature-fork", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, forked.Workspace.Name, true, true)
	assertFile(t, filepath.Join(forked.Workspace.Path, "node_modules", "pkg", "version.txt"), "feature-v2\n")
	assertFile(t, filepath.Join(forked.Workspace.Path, "package-lock.json"), "{\"lockfileVersion\":3,\"version\":\"2\"}\n")
	if !forked.Workspace.EnvironmentReady {
		t.Fatalf("forked environment was not ready: %#v", forked.Workspace.EnvironmentMissing)
	}
}

func TestPythonVirtualEnvironmentIsRecreatedNotCloned(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".venv/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname='fixture'\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "uv.lock"), []byte("version = 1\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".venv", "bin", "python"), []byte("absolute-path-sensitive\n"), 0o755)
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "python")
	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	value, err := manager.Create(ctx, CreateSpec{Name: "python", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, value.Name, true, true)
	if _, err := os.Stat(filepath.Join(value.Path, ".venv")); !os.IsNotExist(err) {
		t.Fatalf(".venv was cloned instead of recreated: %v", err)
	}
	if value.EnvironmentReady {
		t.Fatal("workspace with an unprepared recreate policy was reported ready")
	}
}

func TestYarnLayoutsWorkThroughGenericTrackedIgnoredFileRules(t *testing.T) {
	ctx := context.Background()
	for name, trackedPnP := range map[string]bool{"ignored-pnp": false, "tracked-zero-install": true} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			runGitTest(t, root, "init", "-b", "main")
			runGitTest(t, root, "config", "user.email", "test@example.com")
			runGitTest(t, root, "config", "user.name", "Cambium Test")
			ignore := "node_modules/\n.yarn/unplugged/\n"
			if !trackedPnP {
				ignore += ".pnp.cjs\n"
			}
			_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte(ignore), 0o644)
			_ = os.WriteFile(filepath.Join(root, "package.json"), []byte("{\"packageManager\":\"yarn@4\"}\n"), 0o644)
			_ = os.WriteFile(filepath.Join(root, "yarn.lock"), []byte("__metadata:\n  version: 8\n"), 0o644)
			_ = os.WriteFile(filepath.Join(root, ".pnp.cjs"), []byte("module.exports = 'pnp';\n"), 0o644)
			runGitTest(t, root, "add", ".")
			runGitTest(t, root, "commit", "-m", "yarn pnp")

			projectValue, err := project.Open(ctx, root, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(projectValue)
			value, err := manager.Create(ctx, CreateSpec{Name: "yarn", Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Remove(ctx, value.Name, true, true)
			assertFile(t, filepath.Join(value.Path, ".pnp.cjs"), "module.exports = 'pnp';\n")
			if _, err := os.Stat(filepath.Join(value.Path, "node_modules")); !os.IsNotExist(err) {
				t.Fatalf("Yarn PnP workspace unexpectedly required node_modules: %v", err)
			}
			if !value.EnvironmentReady {
				t.Fatalf("Yarn PnP workspace was falsely marked unready: %#v", value.EnvironmentMissing)
			}
		})
	}
}

func TestCustomCambiumPolicyClonesRecreatesAndOverridesBuiltins(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n.company-deps/\n.generated-env/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package.json"), []byte("{\"name\":\"custom\"}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "company.lock"), []byte("v1\n"), 0o644)
	policy := `version = 1

[[path]]
path = "node_modules"
policy = "skip"

[[path]]
path = ".company-deps"
policy = "clone"
inputs = ["company.lock"]
required = true

[[path]]
path = ".generated-env"
policy = "recreate"
inputs = ["company.lock"]
required = true
prepare = ["sh", "-c", "mkdir -p .generated-env && printf ready > .generated-env/state"]
validate = ["sh", "-c", "test -f .generated-env/state"]
`
	_ = os.WriteFile(filepath.Join(root, ".cambium.toml"), []byte(policy), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "pkg", "state"), []byte("must-not-copy\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, ".company-deps"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".company-deps", "state"), []byte("shared-v1\n"), 0o644)
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "custom policy")
	operational := config.Default()
	operational.AllowPolicyCommands = true
	if _, err := config.Write(root, operational, false); err != nil {
		t.Fatal(err)
	}

	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	value, err := manager.Create(ctx, CreateSpec{Name: "custom", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, value.Name, true, true)
	if _, err := os.Stat(filepath.Join(value.Path, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("custom skip did not override node_modules built-in: %v", err)
	}
	assertFile(t, filepath.Join(value.Path, ".company-deps", "state"), "shared-v1\n")
	assertFile(t, filepath.Join(value.Path, ".generated-env", "state"), "ready")
	if !value.EnvironmentReady {
		t.Fatalf("custom prepared environment not ready: %#v", value.EnvironmentMissing)
	}
	explanation, err := manager.ExplainEnvironment(ctx, value.Name)
	if err != nil {
		t.Fatal(err)
	}
	foundGenerated := false
	for _, item := range explanation.Paths {
		if item.Path == ".generated-env" {
			foundGenerated = true
			if !item.Present || !item.Validated {
				t.Fatalf("recreated environment observation was stale: %#v", item)
			}
		}
	}
	if !foundGenerated {
		t.Fatal("recreated environment receipt missing from explanation")
	}
	if err := os.WriteFile(filepath.Join(value.Path, ".company-deps", "state"), []byte("workspace-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(root, ".company-deps", "state"), "shared-v1\n")
}

func TestPopularEcosystemDefaultsUseGenericFilePolicies(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		inputPath  string
		inputData  string
		outputPath string
		policy     string
	}{
		{name: "npm", inputPath: "package-lock.json", inputData: "{}\n", outputPath: "node_modules"},
		{name: "yarn", inputPath: "yarn.lock", inputData: "__metadata:\n  version: 8\n", outputPath: "node_modules"},
		{name: "pnpm", inputPath: "pnpm-lock.yaml", inputData: "lockfileVersion: '9'\n", outputPath: "node_modules"},
		{name: "bun", inputPath: "bun.lock", inputData: "[lockfile]\n", outputPath: "node_modules"},
		{name: "rust-cargo", inputPath: "Cargo.lock", inputData: "version = 4\n", outputPath: "target"},
		{name: "clojure-cli", inputPath: "deps.edn", inputData: "{}\n", outputPath: ".cpcache"},
		{name: "clojurescript-shadow", inputPath: "shadow-cljs.edn", inputData: "{}\n", outputPath: ".shadow-cljs"},
		{name: "gradle", inputPath: "build.gradle", inputData: "plugins {}\n", outputPath: ".gradle"},
		{name: "go-vendor", inputPath: "go.mod", inputData: "module example.com/fixture\n", outputPath: "vendor"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			runGitTest(t, root, "init", "-b", "main")
			runGitTest(t, root, "config", "user.email", "test@example.com")
			runGitTest(t, root, "config", "user.name", "Cambium Test")
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(test.outputPath+"/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, test.inputPath), []byte(test.inputData), 0o644); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(root, test.outputPath, "cambium-fixture.txt")
			if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(payload, []byte(test.name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitTest(t, root, "add", ".")
			runGitTest(t, root, "commit", "-m", test.name)

			projectValue, err := project.Open(ctx, root, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(projectValue)
			value, err := manager.Create(ctx, CreateSpec{Name: test.name, Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Remove(ctx, value.Name, true, true)
			workspacePayload := filepath.Join(value.Path, test.outputPath, "cambium-fixture.txt")
			assertFile(t, workspacePayload, test.name+"\n")
			if err := os.WriteFile(workspacePayload, []byte("workspace-only\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			assertFile(t, payload, test.name+"\n")
		})
	}
}

func TestPolicyCommandsRequireExplicitTrust(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".generated-env/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "environment.lock"), []byte("v1\n"), 0o644)
	policy := `version = 1

[[path]]
path = ".generated-env"
policy = "recreate"
inputs = ["environment.lock"]
required = true
prepare = ["sh", "-c", "mkdir -p .generated-env && printf unsafe > .generated-env/state"]
`
	_ = os.WriteFile(filepath.Join(root, ".cambium.toml"), []byte(policy), 0o644)
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "policy command")

	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	value, err := manager.Create(ctx, CreateSpec{Name: "untrusted", Materializer: model.MaterializerGit})
	if err == nil {
		_ = manager.Remove(ctx, value.Name, true, true)
		t.Fatal("unreviewed repository policy command executed without explicit trust")
	}
	if !strings.Contains(err.Error(), "allow_policy_commands") {
		t.Fatalf("unexpected trust error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".generated-env")); !os.IsNotExist(statErr) {
		t.Fatalf("policy command changed the primary checkout: %v", statErr)
	}
}

func TestSpeculativeRootProtectsPreparedEnvironmentFromCachePrune(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package.json"), []byte("{\"name\":\"cache\"}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{}\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "pkg", "state"), []byte("ready\n"), 0o644)
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "environment")

	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	value, err := manager.Create(ctx, CreateSpec{Name: "owner", Materializer: model.MaterializerCopy})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := manager.Checkpoint(ctx, value.Name, CheckpointOptions{RefName: "protected-env"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.EnvironmentIDs) == 0 {
		t.Fatal("checkpoint did not pin an environment")
	}
	layerID := checkpoint.EnvironmentIDs[0]
	if err := manager.Remove(ctx, value.Name, true, true); err != nil {
		t.Fatal(err)
	}
	ageReady := func() {
		when := time.Now().Add(-48 * time.Hour)
		path := filepath.Join(projectValue.LayersDir(), layerID, "ready")
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	ageReady()
	result, err := manager.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(projectValue.LayersDir(), layerID)); err != nil {
		t.Fatalf("speculative root did not protect environment layer: %#v err=%v", result, err)
	}

	store, err := manager.SpecStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DropRef(ctx, "protected-env"); err != nil {
		t.Fatal(err)
	}
	gc, err := manager.RootGC(ctx, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.ReleasedRoots) == 0 {
		t.Fatalf("root was not released: %#v", gc)
	}
	ageReady()
	result, err = manager.PruneCaches(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(projectValue.LayersDir(), layerID)); !os.IsNotExist(err) {
		t.Fatalf("unreferenced environment layer survived prune: %#v err=%v", result, err)
	}
}

func TestEnvironmentCaptureDefaultsToWorkspaceHeadNotPrimaryHead(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package.json"), []byte("{\"name\":\"head\"}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("v1\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "state"), []byte("v1\n"), 0o644)
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "v1")
	mainHead := strings.TrimSpace(runGitTest(t, root, "rev-parse", "HEAD"))

	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	value, err := manager.Create(ctx, CreateSpec{Name: "head", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, value.Name, true, true)
	_ = os.WriteFile(filepath.Join(value.Path, "package-lock.json"), []byte("v2\n"), 0o644)
	runGitTest(t, value.Path, "add", "package-lock.json")
	runGitTest(t, value.Path, "commit", "-m", "v2")
	workspaceHead := strings.TrimSpace(runGitTest(t, value.Path, "rev-parse", "HEAD"))
	if workspaceHead == mainHead {
		t.Fatal("workspace commit did not advance")
	}
	_ = os.WriteFile(filepath.Join(value.Path, "node_modules", "state"), []byte("v2\n"), 0o644)
	explanation, err := manager.CaptureEnvironment(ctx, value.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if explanation.SourceRef != workspaceHead {
		t.Fatalf("capture used %s, want workspace HEAD %s (primary %s)", explanation.SourceRef, workspaceHead, mainHead)
	}
}

func TestTargetBranchCambiumPolicyOverridesPrimaryCheckoutPolicy(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "init", "-b", "main")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Cambium Test")
	_ = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package.json"), []byte("{\"name\":\"policy\"}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{}\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "state"), []byte("prepared\n"), 0o644)
	mainPolicy := "version = 1\n[[path]]\npath = \"node_modules\"\npolicy = \"skip\"\n"
	_ = os.WriteFile(filepath.Join(root, ".cambium.toml"), []byte(mainPolicy), 0o644)
	runGitTest(t, root, "add", ".")
	runGitTest(t, root, "commit", "-m", "main skips environment")
	runGitTest(t, root, "checkout", "-b", "feature-policy")
	featurePolicy := "version = 1\n[[path]]\npath = \"node_modules\"\npolicy = \"clone\"\ninputs = [\"package-lock.json\"]\nrequired = true\n"
	_ = os.WriteFile(filepath.Join(root, ".cambium.toml"), []byte(featurePolicy), 0o644)
	runGitTest(t, root, "add", ".cambium.toml")
	runGitTest(t, root, "commit", "-m", "feature clones environment")
	runGitTest(t, root, "checkout", "main")

	projectValue, err := project.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(projectValue)
	mainWorkspace, err := manager.Create(ctx, CreateSpec{Name: "main-policy", Ref: "main", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, mainWorkspace.Name, true, true)
	if _, err := os.Stat(filepath.Join(mainWorkspace.Path, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("main policy did not skip node_modules: %v", err)
	}

	featureWorkspace, err := manager.Create(ctx, CreateSpec{Name: "feature-policy", Ref: "feature-policy", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(ctx, featureWorkspace.Name, true, true)
	assertFile(t, filepath.Join(featureWorkspace.Path, "node_modules", "state"), "prepared\n")
	if !featureWorkspace.EnvironmentReady {
		t.Fatalf("target branch policy was not applied: %#v", featureWorkspace.EnvironmentMissing)
	}
}
