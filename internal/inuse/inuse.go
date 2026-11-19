// Package inuse answers one question for prune, maintain and remove: is
// anything still using this workspace? Three independent signals, any of which
// means "in use":
//
//   - a live process whose current directory is inside the workspace;
//   - a use marker (a file named by PID under the project's use directory)
//     written by `cambium run` for the duration of its child;
//   - a held build lock: an flock on target/.cargo-lock or
//     target/<profile>/.cargo-lock, which Cargo holds while it builds.
//
// The check is advisory by design. It never kills, signals or waits.
package inuse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

type Kind string

const (
	KindProcess Kind = "process" // a live process has its cwd inside the workspace
	KindMarker  Kind = "marker"  // a live `cambium run` registered use
	KindLock    Kind = "lock"    // a held flock on a build lock file
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

// Check reports every holder of the workspace at path. markerDir is the
// per-workspace directory used by Mark ("" disables the marker signal).
func Check(path, markerDir string) ([]Holder, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing on disk can be in use
		}
		return nil, err
	}
	var holders []Holder
	cwds, err := processCwds()
	if err != nil {
		return nil, fmt.Errorf("list process working directories: %w", err)
	}
	self := os.Getpid()
	for _, entry := range cwds {
		if entry.pid != self && within(entry.cwd, resolved) {
			holders = append(holders, Holder{Kind: KindProcess, PID: entry.pid, Detail: "cwd " + entry.cwd})
		}
	}
	if markerDir != "" {
		markers, err := liveMarkers(markerDir)
		if err != nil {
			return nil, err
		}
		holders = append(holders, markers...)
	}
	holders = append(holders, heldBuildLocks(resolved)...)
	return holders, nil
}

// Mark registers the current process as using a workspace until release is
// called. A crashed marker owner is ignored (its PID is dead) and swept later.
func Mark(markerDir string) (release func(), err error) {
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(markerDir, strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(path, []byte("cambium run\n"), 0o644); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func liveMarkers(dir string) ([]Holder, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var holders []Holder
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid <= 0 {
			continue
		}
		if ProcessAlive(pid) {
			holders = append(holders, Holder{Kind: KindMarker, PID: pid, Detail: "cambium run"})
		}
	}
	return holders, nil
}

// ProcessAlive reports whether pid exists (EPERM means it exists, owned by another user).
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func within(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// heldBuildLocks probes Cargo's lock files without blocking. A shared flock is
// taken and dropped immediately: it succeeds unless an exclusive holder (a
// running cargo build) exists, and it never disturbs a holder.
func heldBuildLocks(root string) []Holder {
	var candidates []string
	candidates = append(candidates, filepath.Join(root, "target", ".cargo-lock"))
	profiles, _ := filepath.Glob(filepath.Join(root, "target", "*", ".cargo-lock"))
	candidates = append(candidates, profiles...)
	nested, _ := filepath.Glob(filepath.Join(root, "*", "target", "*", ".cargo-lock"))
	candidates = append(candidates, nested...)
	var holders []Holder
	for _, path := range candidates {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		} else if errors.Is(err, syscall.EWOULDBLOCK) {
			holders = append(holders, Holder{Kind: KindLock, Detail: "held " + path})
		}
		_ = file.Close()
	}
	return holders
}

type cwdEntry struct {
	pid int
	cwd string
}

// processCwds lists (pid, cwd) for every process this user may inspect.
func processCwds() ([]cwdEntry, error) {
	if runtime.GOOS == "linux" {
		return procCwds()
	}
	return lsofCwds()
}

func procCwds() ([]cwdEntry, error) {
	dirs, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil, err
	}
	var out []cwdEntry
	for _, dir := range dirs {
		pid, convErr := strconv.Atoi(filepath.Base(dir))
		if convErr != nil {
			continue
		}
		cwd, linkErr := os.Readlink(filepath.Join(dir, "cwd"))
		if linkErr != nil {
			continue // exited, or another user's process
		}
		out = append(out, cwdEntry{pid, cwd})
	}
	return out, nil
}

// lsofCwds parses `lsof -Fpn` field output: "p<pid>" then "n<path>" lines.
// lsof exits 1 when it could not inspect some processes; that is normal, so only
// a failure with no output is an error.
func lsofCwds() ([]cwdEntry, error) {
	command := exec.Command("lsof", "-a", "-d", "cwd", "-Fpn")
	command.Dir = string(filepath.Separator) // our own cwd must not look like a holder
	var stdout bytes.Buffer
	command.Stdout = &stdout
	runErr := command.Run()
	if stdout.Len() == 0 && runErr != nil {
		return nil, fmt.Errorf("lsof: %w", runErr)
	}
	var out []cwdEntry
	pid := 0
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0:
			out = append(out, cwdEntry{pid, line[1:]})
		}
	}
	return out, scanner.Err()
}
