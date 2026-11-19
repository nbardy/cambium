package fsx

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// TreeSize is the disk cost of a directory tree.
//
// Logical is the sum of file sizes. Exclusive is the bytes that deleting the
// tree would actually return to the volume: on APFS it excludes extents shared
// with another clone (macOS ATTR_CMNEXT_PRIVATESIZE), which is the number that
// matters for a CoW workspace and that `du` cannot report. Where the volume
// cannot say (Exact is false) Exclusive falls back to allocated blocks, an
// upper bound.
type TreeSize struct {
	Logical   int64 `json:"logical_bytes"`
	Exclusive int64 `json:"exclusive_bytes"`
	Exact     bool  `json:"exclusive_exact"`
	Files     int   `json:"files"`
}

// MeasureTree walks root without following symlinks. Unreadable entries are
// skipped and counted in neither total; a vanished root is an error.
func MeasureTree(root string) (TreeSize, error) {
	if _, err := os.Lstat(root); err != nil {
		return TreeSize{}, err
	}
	size := TreeSize{Exact: true}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		size.Files++
		size.Logical += info.Size()
		if private, ok := privateSize(path); ok {
			size.Exclusive += private
			return nil
		}
		size.Exact = false
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			size.Exclusive += stat.Blocks * 512
		} else {
			size.Exclusive += info.Size()
		}
		return nil
	})
	return size, err
}
