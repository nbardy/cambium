package gitx_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/gitx"
)

func TestListWorktreesPreservesPathsAndState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "source")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeGit(t, root, "init", "-q", "-b", "main")
	worktreeGit(t, root, "config", "user.name", "Cambium test")
	worktreeGit(t, root, "config", "user.email", "test@example.invalid")
	worktreeGit(t, root, "config", "core.hooksPath", os.DevNull)
	worktreeGit(t, root, "config", "commit.gpgSign", "false")
	worktreeGit(t, root, "commit", "--allow-empty", "-qm", "initial")
	head := strings.TrimSpace(worktreeGit(t, root, "rev-parse", "HEAD"))
	repository, err := gitx.Discover(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]gitx.Worktree{
		root: {Path: root, HEAD: head, Branch: "main"},
	}
	for i, name := range []string{"has space", "has\ttab", "has\nnewline", "東京の作業"} {
		if runtime.GOOS == "windows" && strings.ContainsAny(name, "\t\n") {
			continue // Windows filenames cannot contain these control characters.
		}
		path := filepath.Join(base, name)
		branch := fmt.Sprintf("linked-%d", i)
		worktreeGit(t, root, "worktree", "add", "-b", branch, path, "HEAD")
		want[path] = gitx.Worktree{Path: path, HEAD: head, Branch: branch}
	}
	locked := filepath.Join(base, "has space")
	worktreeGit(t, root, "worktree", "lock", "--reason", "portable\nmedia", locked)
	entry := want[locked]
	entry.Locked = true
	want[locked] = entry

	missing := filepath.Join(base, "東京の作業")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	entry = want[missing]
	entry.Prunable = true
	want[missing] = entry

	detached := filepath.Join(base, "detached")
	worktreeGit(t, root, "worktree", "add", "--detach", detached, "HEAD")
	want[detached] = gitx.Worktree{Path: detached, HEAD: head}

	got, err := repository.ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("worktree count = %d, want %d: %#v", len(got), len(want), got)
	}
	for _, item := range got {
		expected, ok := want[item.Path]
		if !ok || item != expected {
			t.Errorf("worktree = %#v, want %#v (known path: %t)", item, expected, ok)
		}
	}
	for path := range want {
		registered, err := repository.IsWorktreeRegistered(context.Background(), path)
		if err != nil || !registered {
			t.Errorf("worktree %q lost its registration: %t, %v", path, registered, err)
		}
	}

	bare := filepath.Join(base, "bare.git")
	worktreeGit(t, root, "clone", "--bare", "--quiet", root, bare)
	bareRepository := gitx.Repository{Root: bare, CommonGitDir: bare, Runner: execx.OSRunner{}}
	got, err = bareRepository.ListWorktrees(context.Background())
	if err != nil || len(got) != 1 || !got[0].Bare || got[0].Path != bare {
		t.Fatalf("bare worktree state was lost: %#v, %v", got, err)
	}
}

func TestDiscoverAndGitPathPreserveTrailingWhitespace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows path rules do not preserve these trailing characters")
	}
	for _, suffix := range []string{" ", "\t", "\n", "\r", "\u00a0"} {
		t.Run(fmt.Sprintf("suffix-%q", suffix), func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "source"+suffix)
			// Git's gitfile format cannot represent an administration path
			// ending in CR/LF. A trailing space exercises a valid separate
			// common directory for every checkout-path case.
			admin := filepath.Join(base, "git-dir ")
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
			worktreeGit(t, root, "init", "-q", "-b", "main", "--separate-git-dir", admin)
			worktreeGit(t, root, "config", "user.name", "Cambium test")
			worktreeGit(t, root, "config", "user.email", "test@example.invalid")
			worktreeGit(t, root, "config", "core.hooksPath", os.DevNull)
			worktreeGit(t, root, "config", "commit.gpgSign", "false")
			worktreeGit(t, root, "commit", "--allow-empty", "-qm", "initial")
			linked := filepath.Join(base, "linked"+suffix)
			worktreeGit(t, root, "worktree", "add", "-b", "linked", linked, "HEAD")
			for _, path := range []string{root, linked} {
				repository, err := gitx.Discover(context.Background(), path, nil)
				if err != nil {
					t.Fatalf("discover %q: %v", path, err)
				}
				if repository.Root != path || repository.CommonGitDir != admin {
					t.Fatalf("discovery changed a path: root=%q common=%q; want %q and %q", repository.Root, repository.CommonGitDir, path, admin)
				}
				name := "marker" + suffix
				got, err := repository.GitPath(context.Background(), path, name)
				// Locate the common or per-worktree administration directory
				// using a name whose final character is unambiguous.
				control := strings.TrimSpace(worktreeGit(t, path, "rev-parse", "--git-path", "marker"))
				if !filepath.IsAbs(control) {
					control = filepath.Join(path, control)
				}
				want := control + suffix
				if err != nil || got != want {
					t.Fatalf("GitPath changed a path: got %q, want %q, error %v", got, want, err)
				}
			}
		})
	}
}

func worktreeGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, output)
	}
	return string(output)
}
