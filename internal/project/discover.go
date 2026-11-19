package project

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// maxScanDepth bounds how far below the scan root a primary checkout may sit
// (~/git/repo, ~/git/group/repo, ~/git/group/repo/sub).
const maxScanDepth = 3

// skipDirs are never primary checkouts and can hold millions of files.
var skipDirs = map[string]bool{"node_modules": true, "target": true, ".git": true, "_wt": true, ".venv": true, "vendor": true}

// FindRepositories returns every primary checkout under root that has Cambium
// state (<repo>/.git/cambium). Linked worktrees have a .git file, not a
// directory, so they are never returned. Result paths are sorted.
func FindRepositories(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			return fs.SkipDir
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root && skipDirs[entry.Name()] {
			return fs.SkipDir
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return fs.SkipDir
		}
		depth := 0
		if relative != "." {
			depth = len(splitPath(relative))
		}
		if info, err := os.Stat(filepath.Join(path, ".git", "cambium")); err == nil && info.IsDir() {
			found = append(found, path)
			return fs.SkipDir // nested checkouts inside a primary are not scanned
		}
		if depth >= maxScanDepth {
			return fs.SkipDir
		}
		return nil
	})
	sort.Strings(found)
	return found, err
}

func splitPath(path string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(path); i++ {
		if i == len(path) || os.IsPathSeparator(path[i]) {
			if i > start {
				parts = append(parts, path[start:i])
			}
			start = i + 1
		}
	}
	return parts
}
