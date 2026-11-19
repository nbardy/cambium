//go:build darwin || linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
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

// fakeRustc puts a `rustc` on PATH that reports a fixed compiler, so the seed
// is keyed by a known release without needing a Rust toolchain.
func fakeRustc(t *testing.T, release string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'rustc " + release + "\\nhost: test\\nrelease: " + release + "\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "rustc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	if len(cold.DependencySeeds) != 1 || cold.DependencySeeds[0] != "target: unseeded: no seed for rustc 1.93.0; `cambium seed refresh` publishes one" {
		t.Fatalf("an absent seed must be recorded, got %v", cold.DependencySeeds)
	}

	published, err := native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, repo)
	if err != nil {
		t.Fatal(err)
	}
	if published.Release != "1.93.0" || len(published.Profiles) != 1 || published.Profiles[0].Units != 1 || published.Profiles[0].DroppedLocalUnits != 1 {
		t.Fatalf("unexpected publication: %+v", published)
	}
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), true, "debug/.fingerprint/serde-0123456789abcdef/lib-serde.json")
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), false, "debug/.fingerprint/app-1111111111111111")
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), false, "debug/build/app-1111111111111111")
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), false, "debug/deps/libapp-1111111111111111.rlib")
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), false, "debug/deps/unidentified-ffffffffffffffff.rlib")
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), false, "debug/app")
	assertPresent(t, filepath.Join(seedRoot, "1.93.0", "target"), false, "frames")

	warm, err := manager.Create(ctx, CreateSpec{Name: "warm", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, warm.Path, true, "target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json")
	assertPresent(t, warm.Path, true, "target/debug/deps/libserde-0123456789abcdef.rlib")
	assertPresent(t, warm.Path, true, "target/debug/build/serde-0123456789abcdef/out")
	assertPresent(t, warm.Path, false, "target/debug/.fingerprint/app-1111111111111111")
	assertPresent(t, warm.Path, false, "target/cambium-seed.json")
	if len(warm.DependencySeeds) != 1 || warm.DependencySeeds[0] != "target: seeded from rustc 1.93.0" {
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

	// Another compiler never sees this seed.
	fakeRustc(t, "1.94.0")
	other, err := manager.Create(ctx, CreateSpec{Name: "other", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, other.Path, false, "target")
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

	writeReadinessFile(t, repo, "Cargo.lock", cargoTargetLock("source = \"registry+x\"\n")+"\n[[package]]\nname = \"app\"\nversion = \"0.2.0\"\nsource = \"registry+x\"\n")
	if _, err := native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, repo); err == nil {
		t.Fatal("refresh must refuse a package name that is both local and registry")
	}
}
