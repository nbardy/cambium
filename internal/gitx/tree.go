package gitx

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type TreeEntry struct {
	Mode   string `json:"mode"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Path   string `json:"path"`
}

func (r Repository) TreeEntries(ctx context.Context, commit string) ([]TreeEntry, error) {
	result, err := r.RunCommon(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("list tree %s: %w", commit, err)
	}
	records := strings.Split(result.Stdout, "\x00")
	entries := make([]TreeEntry, 0, len(records))
	for _, record := range records {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		if !ok {
			return nil, fmt.Errorf("malformed git ls-tree record %q", record)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed git ls-tree header %q", header)
		}
		entries = append(entries, TreeEntry{Mode: fields[0], Type: fields[1], Object: fields[2], Path: filepath.ToSlash(path)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (r Repository) IsIgnoredAt(ctx context.Context, worktree, relative string) (bool, error) {
	result, err := r.runAtAllowExit(ctx, worktree, nil, []int{0, 1}, "check-ignore", "--no-index", "--quiet", "--", relative)
	if err != nil {
		return false, err
	}
	if result.ExitCode == 0 {
		return true, nil
	}
	probe := filepath.ToSlash(filepath.Join(relative, ".cambium-ignore-probe"))
	result, err = r.runAtAllowExit(ctx, worktree, nil, []int{0, 1}, "check-ignore", "--no-index", "--quiet", "--", probe)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (r Repository) IsTrackedAt(ctx context.Context, worktree, relative string) (bool, error) {
	result, err := r.runAtAllowExit(ctx, worktree, nil, []int{0, 1}, "ls-files", "--error-unmatch", "--", relative)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

// FileAt returns a tracked file exactly as stored in commit. present is false
// when the path does not exist in that tree. It is intended for small control
// files such as .cambium.toml, not arbitrary blob hydration.
func (r Repository) FileAt(ctx context.Context, commit, path string) (content string, present bool, err error) {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if path == "." || path == "" || strings.HasPrefix(path, "../") {
		return "", false, fmt.Errorf("invalid tree path %q", path)
	}
	spec := commit + ":" + path
	probe, err := r.runCommonAllowExit(ctx, []int{0, 1, 128}, "cat-file", "-e", spec)
	if err != nil {
		return "", false, err
	}
	if probe.ExitCode != 0 {
		return "", false, nil
	}
	result, err := r.runCommon(ctx, "show", spec)
	if err != nil {
		return "", false, fmt.Errorf("read %s at %s: %w", path, commit, err)
	}
	return result.Stdout, true, nil
}

// UnignoredUntrackedAt returns untracked files beneath relative that Git does
// not ignore. A managed directory must not silently absorb such files merely
// because a synthetic child happens to match a broader ignore rule.
func (r Repository) UnignoredUntrackedAt(ctx context.Context, worktree, relative string) ([]string, error) {
	result, err := r.runAt(ctx, worktree, nil, "ls-files", "-z", "--others", "--exclude-standard", "--", relative)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(result.Stdout, "\x00") {
		if path != "" {
			paths = append(paths, filepath.ToSlash(path))
		}
	}
	sort.Strings(paths)
	return paths, nil
}
