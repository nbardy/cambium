// Package fsx contains the small set of durable filesystem primitives used by
// Cambium's control plane. Workspace contents remain ordinary native files;
// these helpers are only for metadata and immutable-cache publication.
package fsx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temporary sibling, fsyncs it, atomically
// renames it into place, and fsyncs the parent directory so the rename is
// durable across a sudden process or machine failure on filesystems that
// implement directory fsync.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".cambium-atomic-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return SyncDir(parent)
}

// CopyFileAtomic copies source into a durable atomic replacement at
// destination. It is used for prepared Git indexes, which must never be
// observed partially written.
func CopyFileAtomic(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".cambium-copy-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	removeTemporary = false
	return SyncDir(parent)
}

// PublishDir atomically publishes a fully prepared temporary directory and
// fsyncs its parent. source and destination must be on the same filesystem.
func PublishDir(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(destination))
}

// RemoveFile removes a metadata file and fsyncs the containing directory. A
// missing file is treated as success.
func RemoveFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// RemoveAll removes a file or directory tree and fsyncs its parent directory.
// A missing path is treated as success.
func RemoveAll(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir makes prior directory-entry changes durable. Cambium supports
// macOS and Linux, both of which permit syncing an opened directory.
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
