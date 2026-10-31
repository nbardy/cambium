package native

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/nbardy/cambium/internal/lockfile"
)

func (b *Backend) workspaceLockPath(name string) string {
	return filepath.Join(b.Project.LocksDir(), "workspace-"+slug(name)+".lock")
}

// LockWorkspaces takes distinct workspace locks in a stable order. Callers
// release them in reverse order and must acquire any Git lock only afterward.
// Different names may have the same slug, so deduplication uses lock paths.
func (b *Backend) LockWorkspaces(ctx context.Context, names []string) ([]*lockfile.Lock, error) {
	paths := make([]string, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		path := b.workspaceLockPath(name)
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	locks := make([]*lockfile.Lock, 0, len(paths))
	for _, path := range paths {
		lock, err := lockfile.AcquireContext(ctx, path)
		if err != nil {
			for i := len(locks) - 1; i >= 0; i-- {
				_ = locks[i].Release()
			}
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}
