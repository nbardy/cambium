package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/execx"
)

type Repository struct {
	Root         string
	CommonGitDir string
	Runner       execx.Runner
}

type Worktree struct {
	Path     string
	HEAD     string
	Branch   string
	Bare     bool
	Locked   bool
	Prunable bool
}

func Discover(ctx context.Context, path string, runner execx.Runner) (Repository, error) {
	if runner == nil {
		runner = execx.OSRunner{}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Repository{}, err
	}
	rootResult, err := runner.Run(ctx, execx.Command{Dir: absolute, Name: "git", Args: []string{"rev-parse", "--show-toplevel"}})
	if err != nil {
		return Repository{}, fmt.Errorf("discover Git repository from %s: %w", absolute, err)
	}
	root := gitPathOutput(rootResult.Stdout)
	if root == "" {
		return Repository{}, errors.New("git returned an empty repository root")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Repository{}, err
	}
	commonResult, err := runner.Run(ctx, execx.Command{Dir: root, Name: "git", Args: []string{"rev-parse", "--git-common-dir"}})
	if err != nil {
		return Repository{}, fmt.Errorf("discover common Git directory: %w", err)
	}
	common := gitPathOutput(commonResult.Stdout)
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return Repository{}, err
	}
	return Repository{Root: filepath.Clean(root), CommonGitDir: filepath.Clean(common), Runner: runner}, nil
}

// Git's single-path commands append one LF record terminator. Any preceding
// whitespace, including CR or another LF, belongs to the path on Unix.
func gitPathOutput(output string) string {
	return strings.TrimSuffix(output, "\n")
}

func (r Repository) ResolveCommit(ctx context.Context, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		ref = "HEAD"
	}
	// HEAD and other worktree-local revision expressions belong to the
	// invoking checkout. Passing --git-dir=<common dir> here silently resolves
	// HEAD against the primary checkout when invoked from a linked worktree.
	result, err := r.runAt(ctx, r.Root, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	commit := strings.TrimSpace(result.Stdout)
	if commit == "" {
		return "", fmt.Errorf("ref %q resolved to an empty commit", ref)
	}
	return commit, nil
}

func (r Repository) ResolveCommitAt(ctx context.Context, worktree, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		ref = "HEAD"
	}
	result, err := r.runAt(ctx, worktree, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %q in %s: %w", ref, worktree, err)
	}
	commit := strings.TrimSpace(result.Stdout)
	if commit == "" {
		return "", fmt.Errorf("ref %q resolved to an empty commit", ref)
	}
	return commit, nil
}

func (r Repository) ValidateBranch(ctx context.Context, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("branch cannot be empty")
	}
	if _, err := r.runCommon(ctx, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid branch %q: %w", branch, err)
	}
	return nil
}

func (r Repository) BranchExists(ctx context.Context, branch string) (bool, error) {
	result, err := r.runCommonAllowExit(ctx, []int{0, 1}, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (r Repository) BranchCommit(ctx context.Context, branch string) (string, error) {
	return r.ResolveCommit(ctx, "refs/heads/"+branch)
}

func (r Repository) WorktreeAddNoCheckout(ctx context.Context, branch, path, commit string) error {
	_, err := r.runCommon(ctx, "worktree", "add", "--no-checkout", "-b", branch, path, commit)
	return err
}

func (r Repository) WorktreeAddCheckout(ctx context.Context, branch, path, commit string) error {
	_, err := r.runCommon(ctx, "worktree", "add", "-b", branch, path, commit)
	return err
}

func (r Repository) WorktreeRemove(ctx context.Context, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := r.runCommon(ctx, args...)
	return err
}

func (r Repository) WorktreePrune(ctx context.Context) error {
	_, err := r.runCommon(ctx, "worktree", "prune")
	return err
}

func (r Repository) ListWorktrees(ctx context.Context) ([]Worktree, error) {
	result, err := r.runCommon(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var values []Worktree
	var current *Worktree
	// Git emits paths verbatim. A newline may belong to a path or lock reason,
	// so use its NUL-delimited porcelain format for fields and record boundaries.
	for _, field := range strings.Split(result.Stdout, "\x00") {
		if field == "" {
			if current != nil {
				values = append(values, *current)
				current = nil
			}
			continue
		}
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "worktree":
			if current != nil {
				values = append(values, *current)
			}
			absolute, absErr := filepath.Abs(value)
			if absErr != nil {
				return nil, absErr
			}
			current = &Worktree{Path: filepath.Clean(absolute)}
		case "HEAD":
			if current != nil {
				current.HEAD = value
			}
		case "branch":
			if current != nil {
				current.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		case "bare":
			if current != nil {
				current.Bare = true
			}
		case "locked":
			if current != nil {
				current.Locked = true
			}
		case "prunable":
			if current != nil {
				current.Prunable = true
			}
		}
	}
	if current != nil {
		values = append(values, *current)
	}
	return values, nil
}

func (r Repository) IsWorktreeRegistered(ctx context.Context, path string) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	absolute = filepath.Clean(absolute)
	values, err := r.ListWorktrees(ctx)
	if err != nil {
		return false, err
	}
	for _, value := range values {
		if value.Path == absolute {
			return true, nil
		}
	}
	return false, nil
}

func (r Repository) DeleteBranch(ctx context.Context, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := r.runCommon(ctx, "branch", flag, branch)
	return err
}

// DeleteBranchIfUnchanged is used only for recovery. Its compare-and-delete
// protects commits made by ordinary Git clients, which do not take Cambium's
// administration lock, between our inspection and the ref update.
func (r Repository) DeleteBranchIfUnchanged(ctx context.Context, branch, expected string) error {
	worktrees, err := r.ListWorktrees(ctx)
	if err != nil {
		return err
	}
	for _, worktree := range worktrees {
		if worktree.Branch == branch {
			return fmt.Errorf("branch %q is still checked out at %s", branch, worktree.Path)
		}
	}
	_, err = r.runCommon(ctx, "update-ref", "-d", "refs/heads/"+branch, expected)
	return err
}

func (r Repository) ReadTree(ctx context.Context, worktree string) error {
	_, err := r.runAt(ctx, worktree, nil, "read-tree", "HEAD")
	return err
}

func (r Repository) IsTrackedClean(ctx context.Context, worktree string) (bool, string, error) {
	result, err := r.runAt(ctx, worktree, nil, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return false, "", err
	}
	status := strings.TrimSpace(result.Stdout)
	return status == "", status, nil
}

func (r Repository) IsClean(ctx context.Context, worktree string) (bool, string, error) {
	result, err := r.runAt(ctx, worktree, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return false, "", err
	}
	status := strings.TrimSpace(result.Stdout)
	return status == "", status, nil
}

func (r Repository) GitPath(ctx context.Context, worktree, name string) (string, error) {
	result, err := r.runAt(ctx, worktree, nil, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	path := gitPathOutput(result.Stdout)
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktree, path)
	}
	return filepath.Clean(path), nil
}

func (r Repository) IsIgnored(ctx context.Context, relative string) (bool, error) {
	result, err := r.runAtAllowExit(ctx, r.Root, nil, []int{0, 1}, "check-ignore", "--no-index", "--quiet", "--", relative)
	if err != nil {
		return false, err
	}
	if result.ExitCode == 0 {
		return true, nil
	}
	// A directory-only rule such as "node_modules/" does not match the bare
	// path when the directory is currently absent. Probe a synthetic child so
	// layer validation follows the same semantics Git will use once populated.
	probe := filepath.ToSlash(filepath.Join(relative, ".cambium-ignore-probe"))
	result, err = r.runAtAllowExit(ctx, r.Root, nil, []int{0, 1}, "check-ignore", "--no-index", "--quiet", "--", probe)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (r Repository) IsTracked(ctx context.Context, relative string) (bool, error) {
	result, err := r.runAtAllowExit(ctx, r.Root, nil, []int{0, 1}, "ls-files", "--error-unmatch", "--", relative)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (r Repository) StatusPorcelain(ctx context.Context, worktree string) (string, error) {
	result, err := r.runAt(ctx, worktree, nil, "status", "--porcelain=v1")
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

func (r Repository) RunAt(ctx context.Context, worktree string, env map[string]string, args ...string) (execx.Result, error) {
	return r.runAt(ctx, worktree, env, args...)
}

func (r Repository) RunCommon(ctx context.Context, env map[string]string, args ...string) (execx.Result, error) {
	commandArgs := append([]string{"--git-dir=" + r.CommonGitDir}, args...)
	// Common administration can outlive the invoking worktree, for example
	// while deleting its branch after worktree remove has deleted that cwd.
	// Commands needing worktree context use RunAt/ResolveCommit instead.
	return r.Runner.Run(ctx, execx.Command{Dir: r.CommonGitDir, Env: env, Name: "git", Args: commandArgs})
}

func (r Repository) runCommon(ctx context.Context, args ...string) (execx.Result, error) {
	return r.RunCommon(ctx, nil, args...)
}

func (r Repository) runCommonAllowExit(ctx context.Context, allowed []int, args ...string) (execx.Result, error) {
	result, err := r.runCommon(ctx, args...)
	return allowExit(result, err, allowed)
}

func (r Repository) runAt(ctx context.Context, worktree string, env map[string]string, args ...string) (execx.Result, error) {
	commandArgs := append([]string{"-C", worktree}, args...)
	return r.Runner.Run(ctx, execx.Command{Dir: worktree, Env: env, Name: "git", Args: commandArgs})
}

func (r Repository) runAtAllowExit(ctx context.Context, worktree string, env map[string]string, allowed []int, args ...string) (execx.Result, error) {
	result, err := r.runAt(ctx, worktree, env, args...)
	return allowExit(result, err, allowed)
}

func allowExit(result execx.Result, err error, allowed []int) (execx.Result, error) {
	if err == nil {
		return result, nil
	}
	for _, code := range allowed {
		if result.ExitCode == code {
			return result, nil
		}
	}
	return result, err
}

func EnsureDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}
