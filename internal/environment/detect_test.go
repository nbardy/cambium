package environment

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/gitx"
)

func TestDetectFindsOnlyExistingIgnoredKnownEnvironments(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n.venv/\n.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".node-version"), []byte("22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"node_modules", ".venv", ".env", "target"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := gitx.Discover(context.Background(), root, execx.OSRunner{})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := Detect(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Path != ".venv" || rules[1].Path != "node_modules" {
		t.Fatalf("unexpected detected rules: %#v", rules)
	}
	if rules[0].Mode != "recreate" || rules[1].Mode != "clone" {
		t.Fatalf("unsafe default policies: %#v", rules)
	}
	if got := strings.Join(rules[1].EffectiveInputs(), ","); !strings.Contains(got, "package-lock.json") || !strings.Contains(got, ".node-version") {
		t.Fatalf("lockfile and toolchain markers are absent: %#v", rules[1])
	}
	for _, rule := range rules {
		if rule.Path == ".env" || rule.Path == "target" {
			t.Fatalf("unsafe or unignored path was inferred: %#v", rule)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestBuiltinsCoverPopularEcosystemsThroughGenericPathPolicies(t *testing.T) {
	rules := Builtins()
	byPath := map[string]config.LayerRule{}
	for _, rule := range rules {
		if !rule.Builtin || rule.Origin != "builtin" {
			t.Fatalf("built-in provenance missing: %#v", rule)
		}
		byPath[rule.Path] = rule
	}
	checks := []struct {
		path  string
		mode  config.LayerMode
		input string
	}{
		{"node_modules", config.LayerClone, "yarn.lock"},
		{".pnp.cjs", config.LayerClone, "package.json"},
		{".venv", config.LayerRecreate, "uv.lock"},
		{"target", config.LayerSeed, "Cargo.lock"},
		{".shadow-cljs", config.LayerSeed, "shadow-cljs.edn"},
		{".cpcache", config.LayerSeed, "deps.edn"},
		{"vendor", config.LayerClone, "go.mod"},
		{".gradle", config.LayerSeed, "build.gradle"},
		{"deps", config.LayerClone, "mix.lock"},
		{".dart_tool", config.LayerSeed, "pubspec.lock"},
		{".build", config.LayerSeed, "Package.swift"},
	}
	for _, check := range checks {
		rule, ok := byPath[check.path]
		if !ok {
			t.Fatalf("missing built-in path %q", check.path)
		}
		if rule.Mode != check.mode {
			t.Fatalf("%s mode=%s want=%s", check.path, rule.Mode, check.mode)
		}
		found := false
		for _, input := range rule.EffectiveInputs() {
			if input == check.input {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s missing input %q: %#v", check.path, check.input, rule.EffectiveInputs())
		}
	}
	if byPath[".venv"].Mode == config.LayerClone {
		t.Fatal("Python virtual environments must never default to clone")
	}
}
