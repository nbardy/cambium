package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/model"
)

// Git submodules are not populated by a checkout and make `git worktree
// remove` refuse. With the `submodules` setting, a new workspace gets each
// submodule cloned from the invoking checkout's local copy (no network), and
// removal first proves the workspace's copy holds nothing unique, then empties
// it. Git's `submodule deinit` is never used: it unregisters the submodule in
// the repository config shared by every worktree.

type submodule struct{ Name, Path string }

func (m *Manager) listSubmodules(ctx context.Context, worktree string) ([]submodule, error) {
	if _, err := os.Stat(filepath.Join(worktree, ".gitmodules")); err != nil {
		return nil, nil
	}
	result, err := m.Project.Repository.RunAt(ctx, worktree, nil, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		return nil, fmt.Errorf("read .gitmodules: %w", err)
	}
	var out []submodule
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		key, path, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		out = append(out, submodule{Name: name, Path: path})
	}
	return out, nil
}

func populated(worktree string, s submodule) bool {
	_, err := os.Lstat(filepath.Join(worktree, s.Path, ".git"))
	return err == nil
}

// PopulateSubmodules clones every submodule of created from the same path in
// source (the invoking checkout). It returns the paths source had no copy of;
// those stay empty gitlinks.
func (m *Manager) PopulateSubmodules(ctx context.Context, created model.Workspace, source string) (missing []string, err error) {
	repo := m.Project.Repository
	subs, err := m.listSubmodules(ctx, created.Path)
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		from := filepath.Join(source, s.Path)
		if _, err := os.Lstat(filepath.Join(from, ".git")); err != nil {
			missing = append(missing, s.Path)
			continue
		}
		// -c overrides leave the shared repository config untouched.
		if _, err := repo.RunAt(ctx, created.Path, nil, "-c", "protocol.file.allow=always", "-c", "submodule."+s.Name+".url="+from,
			"submodule", "update", "--init", "--", s.Path); err != nil {
			return missing, fmt.Errorf("populate submodule %s: %w", s.Path, err)
		}
		url, err := repo.RunAt(ctx, created.Path, nil, "config", "-f", ".gitmodules", "submodule."+s.Name+".url")
		if err != nil {
			return missing, err
		}
		inside := filepath.Join(created.Path, s.Path)
		if _, err := repo.RunAt(ctx, inside, nil, "remote", "set-url", "origin", strings.TrimSpace(url.Stdout)); err != nil {
			return missing, err
		}
		// A clone copies the source's local branches as remote-tracking refs, which
		// makes fresh work look unpushed. Copy what the source knows is on the
		// remote instead.
		if _, err := repo.RunAt(ctx, inside, nil, "fetch", "-q", from, "+refs/remotes/origin/*:refs/remotes/origin/*"); err != nil {
			return missing, err
		}
	}
	return missing, nil
}

// checkSubmodules refuses a workspace whose populated submodule has
// uncommitted changes or commits no remote-tracking ref contains.
func (m *Manager) checkSubmodules(ctx context.Context, worktree string) error {
	subs, err := m.listSubmodules(ctx, worktree)
	if err != nil {
		return refuse(RefuseUnreadable, "%v", err)
	}
	repo := m.Project.Repository
	for _, s := range subs {
		if !populated(worktree, s) {
			continue
		}
		inside := filepath.Join(worktree, s.Path)
		status, err := repo.RunAt(ctx, inside, nil, "status", "--porcelain")
		if err != nil {
			return refuse(RefuseUnreadable, "git status in submodule %s: %v", s.Path, err)
		}
		if strings.TrimSpace(status.Stdout) != "" {
			return refuse(RefuseDirty, "submodule %s has uncommitted changes:\n%s", s.Path, status.Stdout)
		}
		unpushed, err := repo.RunAt(ctx, inside, nil, "rev-list", "HEAD", "--branches", "--not", "--remotes")
		if err != nil {
			return refuse(RefuseUnreadable, "rev-list in submodule %s: %v", s.Path, err)
		}
		if strings.TrimSpace(unpushed.Stdout) != "" {
			return refuse(RefuseSubmoduleUnpushed, "submodule %s has commits that are on no remote; push or discard them first", s.Path)
		}
	}
	return nil
}

// dropSubmodules empties each populated submodule (an unpopulated gitlink
// directory) and its workspace-private module store, so the worktree can be
// removed. Call only after checkSubmodules.
func (m *Manager) dropSubmodules(ctx context.Context, worktree string) error {
	subs, err := m.listSubmodules(ctx, worktree)
	if err != nil {
		return err
	}
	var store string
	for _, s := range subs {
		if !populated(worktree, s) {
			continue
		}
		if store == "" {
			result, err := m.Project.Repository.RunAt(ctx, worktree, nil, "rev-parse", "--path-format=absolute", "--git-path", "modules")
			if err != nil {
				return err
			}
			store = strings.TrimSpace(result.Stdout)
		}
		inside := filepath.Join(worktree, s.Path)
		if err := os.RemoveAll(inside); err != nil {
			return err
		}
		if err := os.MkdirAll(inside, 0o755); err != nil {
			return err
		}
	}
	if store != "" {
		// Only this worktree's own store: it lives under .git/worktrees/<id>/modules.
		if !strings.Contains(store, string(filepath.Separator)+"worktrees"+string(filepath.Separator)) {
			return fmt.Errorf("refusing to drop module store %s: not worktree-private", store)
		}
		return os.RemoveAll(store)
	}
	return nil
}
