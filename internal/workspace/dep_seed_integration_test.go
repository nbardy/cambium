//go:build darwin || linux

package workspace

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

const depSeedPolicy = `version = 1

[[path]]
path = "target"
policy = "seed"
seed = "cargo-deps"
`

// fakeRustc selects a `rustc` that reports a fixed compiler, so the seed
// is keyed by a known release without needing a Rust toolchain.
func fakeRustc(t *testing.T, release string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'rustc " + release + "\\nhost: test\\nrelease: " + release + "\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "rustc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUSTC", filepath.Join(bin, "rustc"))
}

func depSeedRepository(t *testing.T) string {
	t.Helper()
	repo := cargoTargetRepository(t)
	writeReadinessFile(t, repo, ".cambium.toml", depSeedPolicy)
	// A workspace member's build script directory and a second profile.
	writeReadinessFile(t, repo, "target/debug/build/app-1111111111111111/out", "member")
	writeReadinessFile(t, repo, "target/debug/build/serde-0123456789abcdef/out", "registry")
	writeReadinessFile(t, repo, "target/debug/deps/libapp-1111111111111111.rlib", "member")
	writeReadinessFile(t, repo, "target/debug/deps/unidentified-ffffffffffffffff.rlib", "no fingerprint names it")
	commitReadinessFiles(t, repo)
	return repo
}

// The seed holds registry/git units only (a whitelist by fingerprint hash),
// reaches workspaces of other commits, is keyed by rustc release, and is
// pruned against the receiving workspace's own lockfile.
func TestDepSeedCarriesDependencyUnitsOnly(t *testing.T) {
	ctx := context.Background()
	seedRoot := t.TempDir()
	t.Setenv(native.SeedRootEnv, seedRoot)
	fakeRustc(t, "1.93.0")
	repo := depSeedRepository(t)
	manager := readinessManager(t, repo, false)

	// Before any refresh a workspace starts empty, and says so.
	cold, err := manager.Create(ctx, CreateSpec{Name: "cold", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, cold.Path, false, "target")
	if len(cold.DependencySeeds) != 1 || cold.DependencySeeds[0].Reason != model.SeedNoSourceBuild {
		t.Fatalf("an absent seed must be recorded, got %v", cold.DependencySeeds)
	}

	published, err := native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, repo)
	if err != nil {
		t.Fatal(err)
	}
	if published.Release != "1.93.0" || len(published.Profiles) != 1 || published.Profiles[0].Units != 1 || published.Profiles[0].DroppedLocalUnits != 1 {
		t.Fatalf("unexpected publication: %+v", published)
	}

	warm, err := manager.Create(ctx, CreateSpec{Name: "warm", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, warm.Path, true, "target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json")
	assertPresent(t, warm.Path, true, "target/debug/deps/libserde-0123456789abcdef.rlib")
	assertPresent(t, warm.Path, true, "target/debug/build/serde-0123456789abcdef/out")
	assertPresent(t, warm.Path, false, "target/debug/.fingerprint/app-1111111111111111")
	assertPresent(t, warm.Path, false, "target/debug/build/app-1111111111111111")
	assertPresent(t, warm.Path, false, "target/debug/deps/libapp-1111111111111111.rlib")
	assertPresent(t, warm.Path, false, "target/debug/deps/unidentified-ffffffffffffffff.rlib")
	assertPresent(t, warm.Path, false, "target/debug/app")
	assertPresent(t, warm.Path, false, "target/frames")
	if len(warm.DependencySeeds) != 1 || warm.DependencySeeds[0].Reason != model.SeedCloned {
		t.Fatalf("got %v", warm.DependencySeeds)
	}

	// serde turns into a path dependency here: its records are withheld.
	writeReadinessFile(t, repo, "Cargo.lock", cargoTargetLock(""))
	commitReadinessFiles(t, repo)
	local, err := manager.Create(ctx, CreateSpec{Name: "local", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, local.Path, false, "target/debug/.fingerprint/serde-0123456789abcdef")
	if local.DependencySeeds[0].Reason != model.SeedNoSourceBuild {
		t.Fatalf("got %v for a different lock", local.DependencySeeds)
	}

	// Another compiler never sees this seed.
	fakeRustc(t, "1.94.0")
	other, err := manager.Create(ctx, CreateSpec{Name: "other", Ref: "HEAD^", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, other.Path, false, "target")
	if other.DependencySeeds[0].Reason != model.SeedToolchainMismatch {
		t.Fatalf("got %v for a different toolchain", other.DependencySeeds)
	}
}

// A refresh is refused while Cargo builds, and when Cargo.lock cannot tell a
// local package from a registry one.
func TestDepSeedRefreshRefusals(t *testing.T) {
	ctx := context.Background()
	t.Setenv(native.SeedRootEnv, t.TempDir())
	fakeRustc(t, "1.93.0")
	repo := depSeedRepository(t)
	manager := readinessManager(t, repo, false)

	lock, err := os.Open(filepath.Join(repo, "target", "debug", ".cargo-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, repo); err == nil {
		t.Fatal("refresh must refuse while a build holds the target")
	}
	_ = lock.Close()
	cold, err := manager.Create(ctx, CreateSpec{Name: "failed-capture", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	if len(cold.DependencySeeds) != 1 || cold.DependencySeeds[0].Reason != model.SeedCaptureFailed {
		t.Fatalf("failed capture was not reported: %v", cold.DependencySeeds)
	}

	writeReadinessFile(t, repo, "Cargo.lock", cargoTargetLock("source = \"registry+x\"\n")+"\n[[package]]\nname = \"app\"\nversion = \"0.2.0\"\nsource = \"registry+x\"\n")
	if _, err := native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, repo); err == nil {
		t.Fatal("refresh must refuse a package name that is both local and registry")
	}
}

// A local Git dependency exercises real Cargo freshness without relying on an
// installed registry cache or internet access.
func TestCargoSeedWarmAndHarvest(t *testing.T) {
	ctx := context.Background()
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("Cargo is unavailable")
	}
	t.Setenv("CARGO_HOME", t.TempDir())
	for _, name := range []string{"RUSTC", "RUSTC_WRAPPER", "RUSTC_WORKSPACE_WRAPPER", "CARGO_TARGET_DIR", "CARGO_BUILD_BUILD_DIR", "CARGO_BUILD_TARGET"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	dep := readinessRepository(t)
	writeReadinessFile(t, dep, "Cargo.toml", "[package]\nname = \"seed-dep\"\nversion = \"0.1.0\"\nedition = \"2021\"\n")
	writeReadinessFile(t, dep, "src/lib.rs", "pub fn n() -> u32 { 42 }\n")
	commitReadinessFiles(t, dep)
	repo := readinessRepository(t)
	depURL := (&url.URL{Scheme: "file", Path: dep}).String()
	writeReadinessFile(t, repo, "Cargo.toml", "[package]\nname = \"seed-app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n[dependencies]\nseed-dep = { git = \""+depURL+"\" }\n")
	writeReadinessFile(t, repo, "src/main.rs", "fn main() { assert_eq!(seed_dep::n(), 42); }\n")
	writeReadinessFile(t, repo, ".gitignore", "target/\n")
	writeReadinessFile(t, repo, ".cambium.toml", depSeedPolicy)
	runReadinessTool(t, repo, "cargo", "generate-lockfile")
	commitReadinessFiles(t, repo)
	manager := readinessManager(t, repo, false)

	assertFresh := func(t *testing.T, name string) {
		t.Helper()
		value, err := manager.Create(ctx, CreateSpec{Name: name, Path: filepath.Join(t.TempDir(), name), Materializer: model.MaterializerGit})
		if err != nil {
			t.Fatal(err)
		}
		if len(value.DependencySeeds) != 1 || value.DependencySeeds[0].Reason != model.SeedCloned {
			t.Fatalf("expected a warm seed, got %v", value.DependencySeeds)
		}
		if local, err := filepath.Glob(filepath.Join(value.Path, "target", "debug", ".fingerprint", "seed-app-*")); err != nil || len(local) != 0 {
			t.Fatalf("workspace units travelled: %v (%v)", local, err)
		}
		output := runReadinessTool(t, value.Path, "cargo", "build", "--offline", "--locked", "-v")
		if !strings.Contains(output, "Fresh seed-dep ") || !strings.Contains(output, "Compiling seed-app ") {
			t.Fatalf("expected Cargo to reuse only the dependency:\n%s", output)
		}
	}

	t.Run("warm", func(t *testing.T) {
		t.Setenv(native.SeedRootEnv, t.TempDir())
		// Warm uses the committed manifest even if the primary checkout is dirty.
		writeReadinessFile(t, repo, "Cargo.toml", "not a valid manifest")
		t.Cleanup(func() { runGitTest(t, repo, "restore", "Cargo.toml") })
		if _, err := manager.Native.WarmDepSeed(ctx, repo, "target"); err != nil {
			t.Fatal(err)
		}
		assertPresent(t, repo, false, "target")
		assertFresh(t, "warmed")
	})

	t.Run("remove", func(t *testing.T) {
		t.Setenv(native.SeedRootEnv, t.TempDir())
		value, err := manager.Create(ctx, CreateSpec{Name: "to-remove", Path: filepath.Join(t.TempDir(), "to-remove"), Materializer: model.MaterializerGit})
		if err != nil {
			t.Fatal(err)
		}
		runReadinessTool(t, value.Path, "cargo", "build", "--offline", "--locked")
		if _, err := manager.SafeRemove(ctx, value.Name, SafeRemoveOptions{DeleteBranch: true}); err != nil {
			t.Fatal(err)
		}
		assertFresh(t, "harvested")

		writeReadinessFile(t, repo, ".cambium.toml", "version = 1\n[[path]]\npath = \"target\"\npolicy = \"skip\"\n")
		commitReadinessFiles(t, repo)
		skip, err := manager.Create(ctx, CreateSpec{Name: "skip", Materializer: model.MaterializerGit})
		if err != nil {
			t.Fatal(err)
		}
		assertPresent(t, skip.Path, false, "target")
		if len(skip.DependencySeeds) != 0 {
			t.Fatalf("skip was overridden: %v", skip.DependencySeeds)
		}

		if manager.Native.Cloner.Probe(ctx, repo).Supported {
			runGitTest(t, repo, "rm", ".cambium.toml")
			commitReadinessFiles(t, repo)
			assertFresh(t, "rust-default")
		}
	})
}
