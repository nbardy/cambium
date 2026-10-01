package lockfile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireIsExclusiveAndReleaseAllowsReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire = %v, want ErrBusy", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireContextWaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		lock, err := AcquireContext(context.Background(), path)
		if err == nil {
			err = lock.Release()
		}
		result <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireContext did not wake after release")
	}
}

func TestAcquireContextHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := AcquireContext(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireContext = %v, want deadline exceeded", err)
	}
}

func TestAcquireReclaimsLockFromDeadProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dead.lock")
	payload, err := json.Marshal(metadata{PID: 1 << 30, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("dead process lock was not reclaimed: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireNeverReclaimsOldLockOwnedByLiveProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live-old.lock")
	payload, err := json.Marshal(metadata{PID: os.Getpid(), Token: "live-token", CreatedAt: time.Now().Add(-2 * staleAfter)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("Acquire = %v, want ErrBusy for a live owner", err)
	}
}

func TestReleaseRefusesToRemoveReplacementLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacement.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := json.Marshal(metadata{PID: os.Getpid(), Token: "replacement-token", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err == nil {
		t.Fatal("Release unexpectedly removed a replacement lock")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replacement lock was removed: %v", err)
	}
}
