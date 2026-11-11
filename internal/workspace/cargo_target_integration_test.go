//go:build darwin || linux

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nbardy/cambium/internal/model"
)

const cargoTargetPolicy = `version = 1

[[path]]
path = "target"
policy = "cargo-target"
inputs = ["rust-toolchain.toml"]
`

func cargoTargetLock(serdeSource string) string {
	return `version = 4

[[package]]
name = "app"
version = "0.1.0"

[[package]]
name = "serde"
version = "1.0.0"
` + serdeSource
}

func cargoTargetRepository(t *testing.T) string {
	t.Helper()
	repo := readinessRepository(t)
	writeReadinessFile(t, repo, ".gitignore", "target/\n")
	writeReadinessFile(t, repo, ".cambium.toml", cargoTargetPolicy)
	writeReadinessFile(t, repo, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.93.0\"\n")
	writeReadinessFile(t, repo, "Cargo.lock", cargoTargetLock("source = \"registry+https://github.com/rust-lang/crates.io-index\"\n"))
	writeReadinessFile(t, repo, "src/main.rs", "fn main() {}\n")
	writeReadinessFile(t, repo, "target/debug/.cargo-lock", "")
	writeReadinessFile(t, repo, "target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json", "{}")
	writeReadinessFile(t, repo, "target/debug/.fingerprint/app-1111111111111111/bin-app.json", "{}")
	writeReadinessFile(t, repo, "target/debug/deps/libserde-0123456789abcdef.rlib", "serde")
	writeReadinessFile(t, repo, "target/debug/app", "binary built in the source checkout")
	writeReadinessFile(t, repo, "target/frames/evidence.png", "project output")
	commitReadinessFiles(t, repo)
	return repo
}

func assertPresent(t *testing.T, root string, present bool, relative string) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
	if present && err != nil {
		t.Errorf("%s is missing: %v", relative, err)
	}
	if !present && err == nil {
		t.Errorf("%s was carried into the workspace", relative)
	}
}

// The seed must reach a workspace at a later commit (that is the reuse), must
// never carry a local package's freshness record or project output, and must
// judge "local" by the lockfile of the workspace it lands in.
func TestCargoTargetLayerSharesThirdPartyOutputOnly(t *testing.T) {
	ctx := context.Background()
	repo := cargoTargetRepository(t)
	manager := readinessManager(t, repo, false)
	first, err := manager.Create(ctx, CreateSpec{Name: "first", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, first.Path, true, "target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json")
	assertPresent(t, first.Path, true, "target/debug/deps/libserde-0123456789abcdef.rlib")
	assertPresent(t, first.Path, false, "target/debug/.fingerprint/app-1111111111111111")
	assertPresent(t, first.Path, false, "target/debug/app")
	assertPresent(t, first.Path, false, "target/frames")

	// A later commit with other sources still receives the seed.
	writeReadinessFile(t, repo, "src/main.rs", "fn main() { println!(\"changed\"); }\n")
	commitReadinessFiles(t, repo)
	second, err := manager.Create(ctx, CreateSpec{Name: "second", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, second.Path, true, "target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json")

	// serde becomes a path dependency. Its record in the cached seed was made
	// from registry sources and no longer describes this workspace.
	writeReadinessFile(t, repo, "Cargo.lock", cargoTargetLock(""))
	commitReadinessFiles(t, repo)
	third, err := manager.Create(ctx, CreateSpec{Name: "third", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, third.Path, false, "target/debug/.fingerprint/serde-0123456789abcdef")
	assertPresent(t, third.Path, true, "target/debug/deps/libserde-0123456789abcdef.rlib")
}

// A target directory is not snapshotted while Cargo builds in it; the next
// workspace created after the build picks the seed up.
func TestCargoTargetLayerWaitsForARunningBuild(t *testing.T) {
	ctx := context.Background()
	repo := cargoTargetRepository(t)
	manager := readinessManager(t, repo, false)
	build, err := os.Open(filepath.Join(repo, "target", "debug", ".cargo-lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer build.Close()
	if err := syscall.Flock(int(build.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	during, err := manager.Create(ctx, CreateSpec{Name: "during", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, during.Path, false, "target")
	if err := syscall.Flock(int(build.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	after, err := manager.Create(ctx, CreateSpec{Name: "after", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	assertPresent(t, after.Path, true, "target/debug/.fingerprint/serde-0123456789abcdef/lib-serde.json")
}
