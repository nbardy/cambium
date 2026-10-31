package workspace

import (
	"context"
	"path/filepath"

	"github.com/nbardy/cambium/internal/lockfile"
)

// lockEnvironmentCache protects both preparation and the publication of its
// durable references. Releasing it after copying but before saving workspace
// or checkpoint metadata would let pruning remove the new, unreferenced layer.
func (m *Manager) lockEnvironmentCache(ctx context.Context) (*lockfile.Lock, error) {
	return lockfile.AcquireContext(ctx, filepath.Join(m.Project.LocksDir(), "cache-admin.lock"))
}
