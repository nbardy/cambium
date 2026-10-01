package speculative

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nbardy/cambium/internal/gitx"
)

func BenchmarkGitMerkleCheckpointOneChangeInFiveHundredTwelvePaths(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		b.Fatal(err)
	}
	benchGit(b, repoPath, "init", "-q", "-b", "main")
	benchGit(b, repoPath, "config", "user.email", "bench@example.com")
	benchGit(b, repoPath, "config", "user.name", "Cambium Bench")
	for index := 0; index < 512; index++ {
		path := filepath.Join(repoPath, "src", fmt.Sprintf("d%02d", index/32), fmt.Sprintf("f%04d.txt", index))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	benchGit(b, repoPath, "add", ".")
	benchGit(b, repoPath, "commit", "-q", "-m", "initial")
	repository, err := gitx.Discover(ctx, repoPath, nil)
	if err != nil {
		b.Fatal(err)
	}
	store, err := Open(repository, filepath.Join(repository.CommonGitDir, "cambium", "speculative"), filepath.Join(repository.CommonGitDir, "cambium", "locks"))
	if err != nil {
		b.Fatal(err)
	}
	worktree := filepath.Join(root, "agent")
	benchGit(b, repoPath, "worktree", "add", "-q", "-b", "agent", worktree, "HEAD")
	if err := os.WriteFile(filepath.Join(worktree, "src", "d00", "f0001.txt"), []byte("changed\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.Capture(ctx, worktree, Ref{Name: "benchmark/root"}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGit(b *testing.B, dir string, args ...string) {
	b.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		b.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
