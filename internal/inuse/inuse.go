// Package inuse checks workspace use markers and Cargo build locks without
// querying the process table.
package inuse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type Kind string

const (
	KindMarker Kind = "marker"
	KindLock   Kind = "lock"
)

// Holder is one reason a workspace is in use.
type Holder struct {
	Kind   Kind   `json:"kind"`
	PID    int    `json:"pid,omitempty"`
	Detail string `json:"detail"`
}

func (h Holder) String() string {
	if h.PID > 0 {
		return fmt.Sprintf("%s pid %d (%s)", h.Kind, h.PID, h.Detail)
	}
	return fmt.Sprintf("%s (%s)", h.Kind, h.Detail)
}

// UnknownError means the available records could not establish workspace use.
type UnknownError struct {
	Cause error
}

func (e *UnknownError) Error() string { return "workspace use is unknown: " + e.Cause.Error() }
func (e *UnknownError) Unwrap() error { return e.Cause }

// Check reports holders of a workspace and its receipt-scoped use directory.
func Check(path, markerDir string) ([]Holder, error) {
	if markerDir == "" {
		return nil, &UnknownError{Cause: errors.New("workspace has no recorded identity")}
	}
	markers, err := heldMarkers(markerDir)
	if err != nil {
		return nil, &UnknownError{Cause: err}
	}
	builds, err := heldBuildLocks(path)
	if err != nil {
		return nil, &UnknownError{Cause: err}
	}
	return append(markers, builds...), nil
}

const pendingPrefix = ".pending-"
const markerPrefix = "run-"

// Mark registers this invocation until release. The returned file can also
// be passed to the child so its lock survives an abrupt exit of the caller.
func Mark(markerDir string) (*os.File, func(), error) {
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(markerDir, pendingPrefix)
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(file.Name())
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, nil, err
	}

	suffix := strings.TrimPrefix(filepath.Base(file.Name()), pendingPrefix)
	path := filepath.Join(markerDir, fmt.Sprintf("%s%d-%s", markerPrefix, os.Getpid(), suffix))
	// Publish only after locking so a reader never sees an unopened marker.
	// Link also refuses to replace another invocation's marker.
	if err := os.Link(file.Name(), path); err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	release := sync.OnceFunc(func() {
		_ = os.Remove(path)
		_ = file.Close()
	})
	return file, release, nil
}

func heldMarkers(dir string) ([]Holder, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var holders []Holder
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, pendingPrefix) {
			continue
		}
		path := filepath.Join(dir, name)
		if pid, err := strconv.Atoi(name); err == nil && pid > 0 {
			return nil, fmt.Errorf("legacy use marker %s has no open-handle record", path)
		}
		owner, marked := strings.CutPrefix(name, markerPrefix)
		pidText, suffix, _ := strings.Cut(owner, "-")
		pid, err := strconv.Atoi(pidText)
		if !marked || pid <= 0 || err != nil || suffix == "" {
			return nil, fmt.Errorf("unrecognized use marker %s", path)
		}
		held, err := lockHeld(path)
		if err != nil {
			return nil, err
		}
		if held {
			holders = append(holders, Holder{Kind: KindMarker, PID: pid, Detail: "cambium run"})
		}
	}
	return holders, nil
}

// heldBuildLocks checks target directories at the root and one level down,
// including Cargo's profiled build directories.
func heldBuildLocks(root string) ([]Holder, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	targets := []string{filepath.Join(root, "target")}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			targets = append(targets, filepath.Join(root, entry.Name(), "target"))
		}
	}

	var holders []Holder
	for _, target := range targets {
		profiles, err := os.ReadDir(target)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		candidates := []string{filepath.Join(target, ".cargo-lock")}
		for _, profile := range profiles {
			if profile.IsDir() || profile.Type()&os.ModeSymlink != 0 {
				candidates = append(candidates, filepath.Join(target, profile.Name(), ".cargo-lock"))
			}
		}
		for _, path := range candidates {
			held, err := lockHeld(path)
			if err != nil {
				return nil, err
			}
			if held {
				holders = append(holders, Holder{Kind: KindLock, Detail: "held " + path})
			}
		}
	}
	return holders, nil
}

// lockHeld probes without blocking, even if a lock path was replaced by a FIFO.
func lockHeld(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("lock %s is not a regular file", path)
	}
	// Cargo and run markers take exclusive locks. Closing drops our shared
	// probe, while a crashed owner's abandoned marker is already unlocked.
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe lock %s: %w", path, err)
	}
	return false, nil
}
