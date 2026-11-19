package inuse

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func kinds(holders []Holder) map[Kind]bool {
	out := map[Kind]bool{}
	for _, holder := range holders {
		out[holder.Kind] = true
	}
	return out
}

func TestIdleWorkspaceIsNotInUse(t *testing.T) {
	holders, err := Check(t.TempDir(), "")
	if err != nil || len(holders) != 0 {
		t.Fatalf("idle workspace reported in use: %v %v", holders, err)
	}
}

func TestLiveProcessCwdMarksWorkspaceInUse(t *testing.T) {
	dir := t.TempDir()
	child := exec.Command("sleep", "30")
	child.Dir = dir
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	holders, err := Check(dir, "")
	if err != nil || !kinds(holders)[KindProcess] {
		t.Fatalf("want a process holder, got %v %v", holders, err)
	}
}

func TestHeldCargoLockMarksWorkspaceInUse(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "target", "debug", ".cargo-lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	holders, _ := Check(dir, "")
	if !kinds(holders)[KindLock] {
		t.Fatalf("want a lock holder, got %v", holders)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	// A flock is per open file description: releasing it must clear the signal.
	if holders, _ := Check(dir, ""); kinds(holders)[KindLock] {
		t.Fatalf("released lock still reported: %v", holders)
	}
}

func TestMarkerOfDeadProcessIsIgnored(t *testing.T) {
	markers := t.TempDir()
	release, err := Mark(markers)
	if err != nil {
		t.Fatal(err)
	}
	if holders, _ := Check(t.TempDir(), markers); !kinds(holders)[KindMarker] {
		t.Fatalf("live marker not reported: %v", holders)
	}
	release()
	if err := os.WriteFile(filepath.Join(markers, "999999"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if holders, _ := Check(t.TempDir(), markers); kinds(holders)[KindMarker] {
		t.Fatalf("dead marker reported: %v", holders)
	}
}
