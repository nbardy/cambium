package inuse

import (
	"errors"
	"os"
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
	holders, err := Check(t.TempDir(), t.TempDir())
	if err != nil || len(holders) != 0 {
		t.Fatalf("idle workspace reported in use: %v %v", holders, err)
	}
}

func TestCheckDoesNotSpawnLsof(t *testing.T) {
	bin := t.TempDir()
	called := filepath.Join(bin, "called")
	t.Setenv("PATH", bin)
	t.Setenv("CAMBIUM_TEST_LSOF_CALLED", called)
	if err := os.WriteFile(filepath.Join(bin, "lsof"), []byte("#!/bin/sh\nprintf called > \"$CAMBIUM_TEST_LSOF_CALLED\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, markers := t.TempDir(), t.TempDir()
	_, release, err := Mark(markers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	holders, checkErr := Check(root, markers)
	if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check invoked lsof or could not verify the trap: %v", err)
	}
	if checkErr != nil || len(holders) != 1 || holders[0].Kind != KindMarker || holders[0].PID != os.Getpid() {
		t.Fatalf("want the recorded marker owner, got %v %v", holders, checkErr)
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
	markers := t.TempDir()
	holders, err := Check(dir, markers)
	if err != nil || !kinds(holders)[KindLock] {
		t.Fatalf("want a lock holder, got %v %v", holders, err)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	// A flock is per open file description: releasing it must clear the signal.
	if holders, err := Check(dir, markers); err != nil || kinds(holders)[KindLock] {
		t.Fatalf("released lock still reported: %v %v", holders, err)
	}
}

func TestOnlyOpenMarkersCount(t *testing.T) {
	root, markers := t.TempDir(), t.TempDir()
	abandoned, releaseAbandoned, err := Mark(markers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseAbandoned)
	_, release, err := Mark(markers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if holders, err := Check(root, markers); err != nil || len(holders) != 2 {
		t.Fatalf("want two independent registrations from the same owner, got %v %v", holders, err)
	}
	// Closing without removing the marker simulates the kernel closing a crashed owner's handle.
	if err := abandoned.Close(); err != nil {
		t.Fatal(err)
	}
	if holders, err := Check(root, markers); err != nil || len(holders) != 1 {
		t.Fatalf("want only the still-open marker, got %v %v", holders, err)
	}
	release()
	if holders, err := Check(root, markers); err != nil || len(holders) != 0 {
		t.Fatalf("want no active markers, got %v %v", holders, err)
	}
}
