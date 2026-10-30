package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

func TestCheckpointEnvironmentIncludesFilesUntrackedInRealIndex(t *testing.T) {
	ctx := context.Background()
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, ".gitignore", "node_modules/\ntarget/\n")
	writeReadinessFile(t, repo, "Cargo.toml", "[package]\nname = \"fixture\"\nversion = \"0.1.0\"\n")
	writeReadinessFile(t, repo, "src/main.rs", "fn main() {}\n")
	writeReadinessFile(t, repo, "target/warm-state", "advisory build seed\n")
	commitReadinessFiles(t, repo)
	manager := readinessManager(t, repo, false)
	value, err := manager.Create(ctx, CreateSpec{Name: "checkpoint-inputs", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	writeReadinessFile(t, value.Path, "src/new.rs", "pub fn added() {}\n")
	indexPath, err := manager.Project.Repository.GitPath(ctx, value.Path, "index")
	if err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := manager.Checkpoint(ctx, value.Name, CheckpointOptions{RefName: "with-new-source"})
	if err != nil {
		t.Fatalf("source-sensitive environment rejected a file already captured in its target tree: %v", err)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatalf("environment input inspection modified the real index: %v", err)
	}
	if staged := runGitTest(t, value.Path, "ls-files", "--", "src/new.rs"); staged != "" {
		t.Fatalf("new source was staged in the visible index: %q", staged)
	}
	forked, err := manager.Fork(ctx, "with-new-source", CreateSpec{Name: "fork-new-source", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(forked.Workspace.Path, "src", "new.rs"), "pub fn added() {}\n")
	assertFile(t, filepath.Join(forked.Workspace.Path, "target", "warm-state"), "advisory build seed\n")
	writeReadinessFile(t, value.Path, "src/new.rs", "pub fn changed_again() {}\n")
	if _, err := manager.CaptureEnvironment(ctx, value.Name, checkpoint.Root.String()); err == nil {
		t.Fatal("source-sensitive receipt accepted new bytes that differ from its hidden target tree")
	}
}

func TestRequiredSharedEnvironmentStaysMissingWhenAbsentOrDangling(t *testing.T) {
	for _, initiallyPresent := range []bool{false, true} {
		t.Run(fmt.Sprintf("initially-present-%t", initiallyPresent), func(t *testing.T) {
			ctx := context.Background()
			repo := readinessRepository(t)
			writeReadinessFile(t, repo, ".gitignore", "shared-cache/\n")
			writeReadinessFile(t, repo, "shared.lock", "v1\n")
			writeReadinessFile(t, repo, ".cambium.toml", `version = 1
[[path]]
path = "shared-cache"
policy = "share"
inputs = ["shared.lock"]
required = true
`)
			if initiallyPresent {
				writeReadinessFile(t, repo, "shared-cache/state", "shared data\n")
			}
			commitReadinessFiles(t, repo)
			manager := readinessManager(t, repo, false)
			value, err := manager.Create(ctx, CreateSpec{Name: "shared", Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			if initiallyPresent {
				if !value.EnvironmentReady {
					t.Fatalf("existing explicit share was not available: %#v", value)
				}
				if err := os.RemoveAll(filepath.Join(repo, "shared-cache")); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(filepath.Join(value.Path, "shared-cache"))
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("fixture did not retain a dangling shared link: %v", err)
				}
			} else if value.EnvironmentReady || !slices.Contains(value.EnvironmentMissing, "shared-cache") {
				t.Fatalf("required absent share was marked ready during create: %#v", value)
			}
			for name, observe := range map[string]func() (EnvironmentExplanation, error){
				"explain":  func() (EnvironmentExplanation, error) { return manager.ExplainEnvironment(ctx, value.Name) },
				"validate": func() (EnvironmentExplanation, error) { return manager.ValidateEnvironment(ctx, value.Name) },
			} {
				explanation, err := observe()
				if err != nil || explanation.Ready || !slices.Contains(explanation.Missing, "shared-cache") {
					t.Fatalf("%s promoted an unavailable required share to ready: %#v, %v", name, explanation, err)
				}
			}
		})
	}
}

func TestMissingEnvironmentCacheUpgradesAfterInstall(t *testing.T) {
	ctx := context.Background()
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, "package.json", "{\"name\":\"fixture\"}\n")
	writeReadinessFile(t, repo, "package-lock.json", "v1\n")
	commitReadinessFiles(t, repo)
	manager := readinessManager(t, repo, false)
	first, err := manager.Create(ctx, CreateSpec{Name: "before-install", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if first.EnvironmentReady || !slices.Contains(first.EnvironmentMissing, "node_modules") {
		t.Fatalf("uninstalled JS project was reported ready: %#v", first)
	}
	writeReadinessFile(t, repo, "node_modules/state", "v1\n")
	second, err := manager.Create(ctx, CreateSpec{Name: "after-install", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(second.Path, "node_modules", "state"), "v1\n")
	if second.EnvironmentReady || len(second.EnvironmentMissing) != 0 || !slices.Contains(second.EnvironmentUnverified, "node_modules") {
		t.Fatalf("installed but unvalidated environment must be copied as unverified warm state: %#v", second)
	}
}

func TestSharedToolchainMarkerDoesNotRequireNodeDependencies(t *testing.T) {
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, ".tool-versions", "rust 1.80.0\n")
	writeReadinessFile(t, repo, "Cargo.toml", "[package]\nname = \"fixture\"\nversion = \"0.1.0\"\n")
	commitReadinessFiles(t, repo)
	value, err := readinessManager(t, repo, false).Create(context.Background(), CreateSpec{Name: "rust", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(value.EnvironmentMissing, "node_modules") {
		t.Fatalf("shared toolchain marker incorrectly required a Node installation: %#v", value)
	}
}

func TestEnvironmentValidationUsesCurrentWorkspaceAndRuntime(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell fixture requires sh")
	}
	ctx := context.Background()
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, "package.json", "{\"name\":\"fixture\"}\n")
	writeReadinessFile(t, repo, "package-lock.json", "v1\n")
	writeReadinessFile(t, repo, "node_modules/state", "v1\n")
	writeReadinessFile(t, repo, "validate-env.sh", "test -z \"$CAMBIUM_TEST_REJECT_ENV\" && cmp package-lock.json node_modules/state\n")
	writeReadinessFile(t, repo, ".cambium.toml", `version = 1
[[path]]
path = "node_modules"
policy = "clone"
inputs = ["package.json", "package-lock.json", "validate-env.sh"]
required = true
validate = ["sh", "validate-env.sh"]
`)
	commitReadinessFiles(t, repo)
	manager := readinessManager(t, repo, true)
	value, err := manager.Create(ctx, CreateSpec{Name: "validated", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if !value.EnvironmentReady || len(value.EnvironmentUnverified) != 0 {
		t.Fatalf("validated environment was not ready: %#v", value)
	}

	// The cache has a validated payload. A changed runtime must still execute
	// the hook in the new destination instead of trusting that old result.
	t.Setenv("CAMBIUM_TEST_REJECT_ENV", "1")
	if _, err := manager.Create(ctx, CreateSpec{Name: "changed-runtime", Materializer: model.MaterializerGit}); err == nil {
		t.Fatal("cached validation bypassed the current runtime's failing validator")
	}
	// Explain is observational and must not execute the now-failing hook.
	if _, err := manager.ExplainEnvironment(ctx, value.Name); err != nil {
		t.Fatalf("explain unexpectedly executed validation: %v", err)
	}
	t.Setenv("CAMBIUM_TEST_REJECT_ENV", "")

	writeReadinessFile(t, value.Path, "node_modules/state", "wrong-version\n")
	if _, err := manager.CaptureEnvironment(ctx, value.Name, ""); err == nil {
		t.Fatal("environment capture reused cached validation after live dependencies changed")
	}
	explanation, err := manager.ValidateEnvironment(ctx, value.Name)
	if err == nil || explanation.Ready || !slices.Contains(explanation.Unverified, "node_modules") {
		t.Fatalf("mutated dependencies passed explicit validation: %#v, %v", explanation, err)
	}
	explanation, err = manager.ExplainEnvironment(ctx, value.Name)
	if err != nil || explanation.Ready {
		t.Fatalf("explain resurrected stale cached validation: %#v, %v", explanation, err)
	}
	writeReadinessFile(t, value.Path, "node_modules/state", "v1\n")
	explanation, err = manager.ValidateEnvironment(ctx, value.Name)
	if err != nil || !explanation.Ready {
		t.Fatalf("repaired environment could not be validated: %#v, %v", explanation, err)
	}
	if err := os.RemoveAll(filepath.Join(value.Path, "node_modules")); err != nil {
		t.Fatal(err)
	}
	explanation, err = manager.ExplainEnvironment(ctx, value.Name)
	if err != nil || explanation.Ready || !slices.Contains(explanation.Missing, "node_modules") {
		t.Fatalf("explain ignored deleted live dependencies: %#v, %v", explanation, err)
	}
}

// This uses actual offline npm installs, not marker files standing in for a
// package manager. It reproduces the lockfile-v1/runtime-v2 mismatch found in
// review and checks both dirty inputs and a clean checkout with stale deps.
func TestNPMEnvironmentMatchesDeclaredInputsAndRequiresValidation(t *testing.T) {
	for _, tool := range []string{"node", "npm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("real npm regression requires %s", tool)
		}
	}
	ctx := context.Background()
	repo := readinessRepository(t)
	base := t.TempDir()
	cache := filepath.Join(base, "npm-cache")
	if err := os.MkdirAll(filepath.Join(repo, "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "2.0.0"} {
		pkg := filepath.Join(base, "pkg-"+version)
		writeReadinessFile(t, pkg, "package.json", fmt.Sprintf(`{"name":"fixture-dep","version":%q,"main":"index.js"}`, version))
		writeReadinessFile(t, pkg, "index.js", fmt.Sprintf("module.exports = %q;\n", version))
		runReadinessTool(t, pkg, "npm", "pack", "--ignore-scripts", "--offline", "--cache", cache, "--pack-destination", filepath.Join(repo, "vendor"))
	}
	install := func(version string) {
		writeReadinessFile(t, repo, "package.json", fmt.Sprintf(`{"name":"review","version":"1.0.0","dependencies":{"fixture-dep":"file:vendor/fixture-dep-%s.tgz"}}`, version))
		runReadinessTool(t, repo, "npm", "install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", "--cache", cache)
	}
	install("1.0.0")
	commitReadinessFiles(t, repo)
	install("2.0.0")
	manager := readinessManager(t, repo, false)
	dirty, err := manager.Create(ctx, CreateSpec{Name: "dirty-inputs", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if dirty.EnvironmentReady || !slices.Contains(dirty.EnvironmentMissing, "node_modules") {
		t.Fatalf("mismatched live lockfile was accepted as a ready receipt: %#v", dirty)
	}
	if _, err := os.Lstat(filepath.Join(dirty.Path, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("dependencies built for dirty inputs were published under committed v1: %v", err)
	}

	// Restoring the committed manifests leaves npm's v2 output behind. A clean
	// git status still cannot establish which inputs produced that output.
	runGitTest(t, repo, "restore", "package.json", "package-lock.json")
	if status := strings.TrimSpace(runGitTest(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("stale-dependency fixture was not clean: %q", status)
	}
	stale, err := manager.Create(ctx, CreateSpec{Name: "clean-stale", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if stale.EnvironmentReady || !slices.Contains(stale.EnvironmentUnverified, "node_modules") {
		t.Fatalf("clean checkout's stale installation was trusted: %#v", stale)
	}
	if actual := strings.TrimSpace(runReadinessTool(t, stale.Path, "node", "-p", `require("fixture-dep")`)); actual != "2.0.0" {
		t.Fatalf("fixture did not exercise a stale runtime: %q", actual)
	}
	lockBytes, err := os.ReadFile(filepath.Join(stale.Path, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lockBytes, &lock); err != nil || lock.Packages["node_modules/fixture-dep"].Version != "1.0.0" {
		t.Fatalf("fixture did not retain the v1 lockfile: %s, %v", lockBytes, err)
	}

	writeReadinessFile(t, repo, "validate-env.cjs", `const lock = require('./package-lock.json');
const actual = require('fixture-dep/package.json').version;
if (actual !== lock.packages['node_modules/fixture-dep'].version) process.exit(1);
`)
	writeReadinessFile(t, repo, ".cambium.toml", `version = 1
[[path]]
path = "node_modules"
policy = "clone"
inputs = ["package.json", "package-lock.json", "validate-env.cjs"]
required = true
validate = ["node", "validate-env.cjs"]
`)
	commitReadinessFiles(t, repo)
	manager = readinessManager(t, repo, true)
	if _, err := manager.Create(ctx, CreateSpec{Name: "reject-stale", Materializer: model.MaterializerGit}); err == nil {
		t.Fatal("configured validator accepted npm v2 under a v1 lockfile")
	}
	install("1.0.0")
	valid, err := manager.Create(ctx, CreateSpec{Name: "correct-install", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if !valid.EnvironmentReady || len(valid.EnvironmentUnverified) != 0 {
		t.Fatalf("correctly installed and validated npm environment was not ready: %#v", valid)
	}
	if actual := strings.TrimSpace(runReadinessTool(t, valid.Path, "node", "-p", `require("fixture-dep")`)); actual != "1.0.0" {
		t.Fatalf("validated worktree ran wrong dependency: %q", actual)
	}
}

func readinessRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is unavailable")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "init", "-b", "main")
	runGitTest(t, repo, "config", "user.name", "Cambium Readiness Test")
	runGitTest(t, repo, "config", "user.email", "readiness@example.test")
	writeReadinessFile(t, repo, ".gitignore", "node_modules/\n")
	return repo
}

func readinessManager(t *testing.T, repo string, trusted bool) *Manager {
	t.Helper()
	if trusted {
		if _, err := config.WriteLocal(filepath.Join(repo, ".git"), []config.Setting{config.BoolSetting("allow_policy_commands", true)}, true); err != nil {
			t.Fatal(err)
		}
	}
	p, err := project.Open(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(p)
}

func writeReadinessFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitReadinessFiles(t *testing.T, repo string) {
	t.Helper()
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-m", "environment fixture")
}

func runReadinessTool(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
	return string(output)
}
