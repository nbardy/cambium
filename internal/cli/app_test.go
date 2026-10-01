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
	"time"

	"github.com/nbardy/cambium/internal/bench"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/speculative"
	"github.com/nbardy/cambium/internal/workspace"
)

func TestCLIWorkspaceLifecycleAndRun(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	ctx := context.Background()

	if code := app.Run(ctx, []string{"init"}); code != 0 {
		t.Fatalf("init failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"create", "--materializer", "git", "agent"}); code != 0 {
		t.Fatalf("create failed: %s", stderr.String())
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"inspect", "--refresh", "--json", "agent"}); code != 0 {
		t.Fatalf("inspect failed: %s", stderr.String())
	}
	var workspace model.Workspace
	if err := json.Unmarshal(stdout.Bytes(), &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Status != model.WorkspaceReady || workspace.CloneMode != model.CloneModeGitCheckout {
		t.Fatalf("unexpected workspace: %#v", workspace)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"run", "agent", "--", "sh", "-c", "printf run-ok > run-result.txt; printf stdout-ok"}); code != 0 {
		t.Fatalf("run failed: code=%d stderr=%s", code, stderr.String())
	}
	if stdout.String() != "stdout-ok" {
		t.Fatalf("unexpected command stdout %q", stdout.String())
	}
	if data, err := os.ReadFile(filepath.Join(workspace.Path, "run-result.txt")); err != nil || string(data) != "run-ok" {
		t.Fatalf("command did not run in the workspace: %q, %v", data, err)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"list", "--json"}); code != 0 {
		t.Fatalf("list failed: %s", stderr.String())
	}
	var workspaces []model.Workspace
	if err := json.Unmarshal(stdout.Bytes(), &workspaces); err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0].Name != "agent" {
		t.Fatalf("unexpected workspaces: %#v", workspaces)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"remove", "agent"}); code == 0 {
		t.Fatal("remove unexpectedly discarded an untracked command result without --force")
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"remove", "--force", "--delete-branch", "agent"}); code != 0 {
		t.Fatalf("force remove failed: %s", stderr.String())
	}
}

func TestCLICreatePrintPathAndPruneCaches(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	if code := app.Run(ctx, []string{"init", "--detect-layers=false"}); code != 0 {
		t.Fatalf("init failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"create", "--materializer", "copy", "--print-path", "cached"}); code != 0 {
		t.Fatalf("create failed: %s", stderr.String())
	}
	workspacePath := strings.TrimSpace(stdout.String())
	if workspacePath == "" || !filepath.IsAbs(workspacePath) {
		t.Fatalf("--print-path output = %q", stdout.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"remove", "--force", "--delete-branch", "cached"}); code != 0 {
		t.Fatalf("remove failed: %s", stderr.String())
	}
	readyFiles, err := filepath.Glob(filepath.Join(root, ".git", "cambium", "baselines", "*", "ready"))
	if err != nil || len(readyFiles) != 1 {
		t.Fatalf("expected one baseline ready marker: %v, %v", readyFiles, err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(readyFiles[0], old, old); err != nil {
		t.Fatal(err)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"prune", "--older-than", "1h", "--dry-run", "--json"}); code != 0 {
		t.Fatalf("prune dry-run failed: %s", stderr.String())
	}
	var dryRun struct {
		Selected []any `json:"selected"`
		Removed  []any `json:"removed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &dryRun); err != nil {
		t.Fatal(err)
	}
	if len(dryRun.Selected) != 1 || len(dryRun.Removed) != 0 {
		t.Fatalf("unexpected prune dry-run: %#v", dryRun)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"prune", "--older-than", "1h"}); code != 0 {
		t.Fatalf("prune failed: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Dir(readyFiles[0])); !os.IsNotExist(err) {
		t.Fatalf("old cache survived prune: %v", err)
	}
}

func TestCLIRunCanCreateAndDiscardEphemeralWorkspace(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	if code := app.Run(ctx, []string{"init"}); code != 0 {
		t.Fatalf("init failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"run", "--materializer", "git", "--discard-on-exit", "--delete-branch", "throwaway", "--", "sh", "-c", "test -n \"$CAMBIUM_WORKSPACE_PATH\""}); code != 0 {
		t.Fatalf("run-create-discard failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"inspect", "throwaway"}); code == 0 {
		t.Fatal("discarded workspace remained in registry")
	}
}

func TestCLIReapsEphemeralWorkspace(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	if code := app.Run(ctx, []string{"init"}); code != 0 {
		t.Fatalf("init failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"create", "--materializer", "git", "--ephemeral", "temporary"}); code != 0 {
		t.Fatalf("create failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"gc", "--ephemeral", "--older-than", "0s", "--dry-run"}); code != 0 {
		t.Fatalf("gc dry-run failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "temporary") {
		t.Fatalf("gc did not select workspace: %q", stdout.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"gc", "--ephemeral", "--older-than", "0s", "--force", "--delete-branches"}); code != 0 {
		t.Fatalf("gc failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"inspect", "temporary"}); code == 0 {
		t.Fatal("removed workspace remained in registry")
	}
}

func TestCLIBenchmarkProducesComparableResults(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	scratch := filepath.Join(t.TempDir(), "benchmark")
	code := app.Run(context.Background(), []string{
		"benchmark", "--methods", "git,cambium-auto", "--count", "2",
		"--files", "24", "--file-bytes", "64", "--env-files", "8",
		"--env-file-bytes", "64", "--scratch", scratch, "--json",
	})
	if code != 0 {
		t.Fatalf("benchmark failed: %s", stderr.String())
	}
	var report bench.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected two methods, got %#v", report.Results)
	}
	if report.Results[0].Method != "git" || report.Results[1].Method != "cambium-auto" {
		t.Fatalf("unexpected methods: %#v", report.Results)
	}
	if report.Results[0].EnvironmentReady != 0 {
		t.Fatalf("plain Git unexpectedly had prepared ignored environment: %#v", report.Results[0])
	}
	if report.Results[1].EnvironmentReady != 2 {
		t.Fatalf("Cambium did not prepare both environments: %#v", report.Results[1])
	}
}

func testApp(root string) (*App, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &App{Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader(""), Cwd: root}, stdout, stderr
}

func resetBuffers(buffers ...*bytes.Buffer) {
	for _, buffer := range buffers {
		buffer.Reset()
	}
}

func initializeCLIRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitCLI(t, root, "init", "-q", "-b", "main")
	gitCLI(t, root, "config", "user.name", "CLI Test")
	gitCLI(t, root, "config", "user.email", "cli@example.invalid")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-q", "-m", "initial")
	return root
}

func requireCLIGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
}

func gitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestCLISpeculativeCheckpointForkAndRootCommands(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	if code := app.Run(ctx, []string{"init", "--detect-layers=false"}); code != 0 {
		t.Fatalf("init failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"create", "--materializer", "git", "agent"}); code != 0 {
		t.Fatalf("create failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"inspect", "--json", "agent"}); code != 0 {
		t.Fatalf("inspect failed: %s", stderr.String())
	}
	var source model.Workspace
	if err := json.Unmarshal(stdout.Bytes(), &source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Path, "src", "a.txt"), []byte("speculative\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Path, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"checkpoint", "--as", "idea/one", "--json", "agent"}); code != 0 {
		t.Fatalf("checkpoint failed: %s", stderr.String())
	}
	var checkpoint workspace.CheckpointResult
	if err := json.Unmarshal(stdout.Bytes(), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Root.IsZero() || len(checkpoint.ChangedPaths) != 2 {
		t.Fatalf("unexpected checkpoint: %#v", checkpoint)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"checkpoint", "--detach", "--parent", "idea/one", "agent"}); code == 0 || !strings.Contains(stderr.String(), "cannot be used together") {
		t.Fatalf("checkpoint accepted conflicting lineage flags: code=%d stderr=%q", code, stderr.String())
	}

	if err := os.WriteFile(filepath.Join(source.Path, "src", "a.txt"), []byte("detached\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"checkpoint", "--detach", "--as", "idea/detached", "--json", "agent"}); code != 0 {
		t.Fatalf("detached checkpoint failed: %s", stderr.String())
	}
	var detached workspace.CheckpointResult
	if err := json.Unmarshal(stdout.Bytes(), &detached); err != nil {
		t.Fatal(err)
	}
	if detached.Root == checkpoint.Root || !detached.Ref.Parent.IsZero() {
		t.Fatalf("CLI --detach retained lineage: first=%s detached=%#v", checkpoint.Root, detached.Ref)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"root", "list", "--json"}); code != 0 {
		t.Fatalf("root list failed: %s", stderr.String())
	}
	var refs []speculative.Ref
	if err := json.Unmarshal(stdout.Bytes(), &refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("unexpected refs: %#v", refs)
	}
	names := map[string]bool{}
	for _, ref := range refs {
		names[ref.Name] = true
	}
	if !names["idea/one"] || !names["idea/detached"] {
		t.Fatalf("missing speculative refs: %#v", refs)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"fork", "--materializer", "git", "--json", "idea/one", "forked"}); code != 0 {
		t.Fatalf("fork failed: %s", stderr.String())
	}
	var forked workspace.ForkResult
	if err := json.Unmarshal(stdout.Bytes(), &forked); err != nil {
		t.Fatal(err)
	}
	if forked.Workspace.SpeculativeRoot != checkpoint.Root.String() {
		t.Fatalf("fork did not record root provenance: %#v", forked)
	}
	if data, err := os.ReadFile(filepath.Join(forked.Workspace.Path, "src", "a.txt")); err != nil || string(data) != "speculative\n" {
		t.Fatalf("forked content = %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(forked.Workspace.Path, "new.txt")); err != nil || string(data) != "new\n" {
		t.Fatalf("forked untracked content = %q err=%v", data, err)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"root", "show", "--json", "idea/one"}); code != 0 {
		t.Fatalf("root show failed: %s", stderr.String())
	}
	var shown map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil || shown["id"] == nil {
		t.Fatalf("root show output: %#v err=%v", shown, err)
	}

	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"remove", "--force", "--delete-branch", "forked"}); code != 0 {
		t.Fatalf("remove forked failed: %s", stderr.String())
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"remove", "--force", "--delete-branch", "agent"}); code != 0 {
		t.Fatalf("remove agent failed: %s", stderr.String())
	}
}
