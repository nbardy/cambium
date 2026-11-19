// Package registry is the user-level list of repositories that hold Cambium
// state, wherever they live. `cambium path NAME`, `remove NAME` and
// `maintain --all-repos` consult it, so a workspace is found from any
// directory even when its repository is outside repos_root.
//
// The file is append-only, one absolute primary-checkout path per line, so two
// processes registering at once can at worst both append the same line.
package registry

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Path is $CAMBIUM_STATE_DIR/repositories, else
// $XDG_STATE_HOME/cambium/repositories, else ~/.local/state/cambium/repositories.
// An empty result means no home directory could be determined.
func Path() string {
	if dir := os.Getenv("CAMBIUM_STATE_DIR"); dir != "" {
		return filepath.Join(dir, "repositories")
	}
	if testing.Testing() {
		// Test binaries must never write the developer's real registry; a test
		// that exercises it sets CAMBIUM_STATE_DIR.
		return ""
	}
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "cambium", "repositories")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "cambium", "repositories")
}

// Register records a primary checkout. It is idempotent and best effort: a
// registry that cannot be written never fails the command that triggered it.
func Register(root string) {
	file := Path()
	if file == "" || root == "" {
		return
	}
	root = filepath.Clean(root)
	for _, known := range List() {
		if known == root {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return
	}
	handle, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer handle.Close()
	_, _ = handle.WriteString(root + "\n")
}

// List returns the registered repositories, sorted and de-duplicated. Entries
// whose directory has vanished are skipped, not deleted.
func List() []string {
	file := Path()
	if file == "" {
		return nil
	}
	handle, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer handle.Close()
	seen := map[string]bool{}
	var out []string
	scanner := bufio.NewScanner(handle)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		if info, err := os.Stat(filepath.Join(line, ".git", "cambium")); err == nil && info.IsDir() {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}
