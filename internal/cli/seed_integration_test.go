package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/workspace"
)

func TestColdSeedShownOnAddAndStatus(t *testing.T) {
	f := newSafeFixture(t)
	t.Setenv(native.SeedRootEnv, t.TempDir())
	rustc := filepath.Join(t.TempDir(), "rustc")
	if err := os.WriteFile(rustc, []byte("#!/bin/sh\nprintf 'rustc 1.93.0\\nhost: fixture\\nrelease: 1.93.0\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUSTC", rustc)
	writeFile(t, filepath.Join(f.root, "Cargo.lock"), "[[package]]\nname = \"seed-app\"\nversion = \"0.1.0\"\n")
	writeFile(t, filepath.Join(f.root, ".gitignore"), "target/\n")
	writeFile(t, filepath.Join(f.root, ".cambium.toml"), "version = 1\n[[path]]\npath = \"target\"\npolicy = \"seed\"\nseed = \"cargo-deps\"\n")
	gitCLI(t, f.root, "add", ".")
	gitCLI(t, f.root, "commit", "-qm", "Cargo policy")

	out := f.mustRun("add", "--materializer", "git", "--name", "text")
	if !strings.Contains(out, "target: cold (no_source_build)") {
		t.Fatalf("add output omitted cold seed: %s", out)
	}
	out = f.mustRun("add", "--materializer", "git", "--name", "json", "--json")
	var added model.Workspace
	if err := json.Unmarshal([]byte(out), &added); err != nil {
		t.Fatal(err)
	}
	check := func(seeds []model.SeedResult) {
		t.Helper()
		if len(seeds) != 1 || seeds[0].Path != "target" || seeds[0].Reason != model.SeedNoSourceBuild {
			t.Fatalf("missing typed cold seed: %v", seeds)
		}
	}
	check(added.DependencySeeds)
	f.mustRun("status", "--no-bytes", "--merged-into", "HEAD")
	if !strings.Contains(f.stderr.String(), "text seed: target: cold (no_source_build)") {
		t.Fatalf("status output omitted cold seed: %s", f.stderr.String())
	}
	out = f.mustRun("status", "--no-bytes", "--merged-into", "HEAD", "--json")
	var rows []workspace.Status
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected both workspaces, got %+v", rows)
	}
	for _, row := range rows {
		check(row.DependencySeeds)
	}
}
