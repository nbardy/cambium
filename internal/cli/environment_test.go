package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/workspace"
)

func TestCLIReportsCopiedDependenciesAsUnverified(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	for path, content := range map[string]string{
		".gitignore": "node_modules/\n", "package.json": "{\"name\":\"fixture\",\"version\":\"1.0.0\"}\n",
		"node_modules/pkg/index.js": "module.exports = 1;\n",
	} {
		writeEnvironmentFixture(t, root, path, content)
	}
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-qm", "dependency fixture")
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	if code := app.Run(ctx, []string{"create", "--materializer", "git", "--json", "candidate"}); code != 0 {
		t.Fatalf("create: %s", stderr)
	}
	var created model.Workspace
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.EnvironmentReady || strings.Join(created.EnvironmentUnverified, ",") != "node_modules" {
		t.Fatalf("unvalidated copy reported ready: %#v", created)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"env", "validate", "--json", "candidate"}); code == 0 {
		t.Fatal("validate succeeded without a validator")
	}
	var result workspace.EnvironmentExplanation
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("not-ready validation lost its structured result: %s: %v", stdout, err)
	}
	if result.Ready || len(result.Unverified) != 1 {
		t.Fatalf("unexpected validation result: %#v", result)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"env", "explain", "candidate"}); code != 0 || !strings.Contains(stdout.String(), "unverified: node_modules") {
		t.Fatalf("explain did not identify the warm candidate: %s %s", stdout, stderr)
	}
}

func TestCLIExplicitValidationUpdatesReadinessWithoutRunningOnExplain(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	for path, content := range map[string]string{
		".gitignore": "deps/\n", "deps.lock": "one\n", "deps/state": "one\n",
		".cambium.toml": "version = 1\n[[path]]\npath = \"deps\"\npolicy = \"clone\"\ninputs = [\"deps.lock\"]\nrequired = true\nvalidate = [\"sh\", \"-c\", \"cmp deps.lock deps/state && printf checked > deps/validation-marker\"]\n",
	} {
		writeEnvironmentFixture(t, root, path, content)
	}
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-qm", "validated environment fixture")
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	if code := app.Run(ctx, []string{"init", "--allow-policy-commands"}); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"create", "--materializer", "git", "--json", "checked"}); code != 0 {
		t.Fatalf("create: %s", stderr)
	}
	var created model.Workspace
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.EnvironmentReady {
		t.Fatalf("successful validator was not recorded: %#v", created)
	}
	marker := filepath.Join(created.Path, "deps", "validation-marker")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"env", "explain", "checked"}); code != 0 {
		t.Fatalf("explain: %s", stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("explain executed the validator: %v", err)
	}
	writeEnvironmentFixture(t, created.Path, "deps/state", "wrong\n")
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"env", "validate", "--json", "checked"}); code == 0 {
		t.Fatal("validate accepted changed dependency state")
	}
	var result workspace.EnvironmentExplanation
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed validation lost its structured result: %s: %v", stdout, err)
	}
	if result.Ready || len(result.Unverified) != 1 {
		t.Fatalf("failed validation retained readiness: %#v", result)
	}
	resetBuffers(stdout, stderr)
	if code := app.Run(ctx, []string{"inspect", "--json", "checked"}); code != 0 {
		t.Fatalf("inspect: %s", stderr)
	}
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil || created.EnvironmentReady {
		t.Fatalf("failed validation was not persisted: %s: %v", stdout, err)
	}
}

func writeEnvironmentFixture(t *testing.T, root, path, content string) {
	t.Helper()
	name := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
