package lockfile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nbardy/cambium/internal/fsx"
)

const (
	staleAfter   = 30 * time.Minute
	retryInitial = 10 * time.Millisecond
	retryMaximum = 100 * time.Millisecond
)

type metadata struct {
	PID       int       `json:"pid"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

// Lock is a deliberately small cross-platform process lock. The lock file is
// created with O_EXCL, so ownership is decided by the filesystem rather than
// an in-process mutex. Callers must Release the lock.
type Lock struct {
	path  string
	file  *os.File
	token string
}

// Acquire makes one logical attempt to acquire a lock. It preserves the
// original non-blocking API for callers that want an immediate "busy" result.
func Acquire(path string) (*Lock, error) {
	return acquire(path)
}

// AcquireContext waits until the lock is available, the context is cancelled,
// or an unrecoverable filesystem error occurs. This is used for Git's shared
// worktree administration files, where concurrent agent startup is normal and
// "retry the command" is the wrong user experience.
func AcquireContext(ctx context.Context, path string) (*Lock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	delay := retryInitial
	for {
		lock, err := acquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrBusy) {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, fmt.Errorf("wait for lock %s: %w", path, ctx.Err())
		case <-timer.C:
		}
		if delay < retryMaximum {
			delay *= 2
			if delay > retryMaximum {
				delay = retryMaximum
			}
		}
	}
}

var ErrBusy = errors.New("operation is already locked")

func acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := newToken()
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			payload, marshalErr := json.Marshal(metadata{PID: os.Getpid(), Token: token, CreatedAt: time.Now().UTC()})
			if marshalErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, marshalErr
			}
			if _, writeErr := file.Write(payload); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, writeErr
			}
			if syncErr := file.Sync(); syncErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, syncErr
			}
			return &Lock{path: path, file: file, token: token}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil, statErr
		}

		payload, readErr := os.ReadFile(path)
		var owner metadata
		parsedOwner := readErr == nil && json.Unmarshal(payload, &owner) == nil && owner.PID > 0
		if parsedOwner {
			// A valid live owner is never reclaimed solely because the operation
			// is long-running. Doing so would let two processes enter Git's shared
			// administration path and would let the first process later remove the
			// second process's replacement lock.
			if processAlive(owner.PID) {
				return nil, fmt.Errorf("%w: %s", ErrBusy, path)
			}
		} else if time.Since(info.ModTime()) < staleAfter {
			// A partially written or legacy lock has no trustworthy owner. Give a
			// recent creator time to finish; old malformed records are recoverable.
			return nil, fmt.Errorf("%w: %s", ErrBusy, path)
		}
		if removeErr := fsx.RemoveFile(path); removeErr != nil {
			return nil, fmt.Errorf("remove stale lock: %w", removeErr)
		}
	}
	return nil, fmt.Errorf("could not acquire lock %s", path)
}

func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	var first error
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			first = err
		}
		l.file = nil
	}
	payload, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return first
	}
	if err != nil {
		if first != nil {
			return first
		}
		return err
	}
	var current metadata
	if err := json.Unmarshal(payload, &current); err != nil {
		if first != nil {
			return first
		}
		return fmt.Errorf("read lock ownership: %w", err)
	}
	if current.Token != l.token {
		if first != nil {
			return first
		}
		return fmt.Errorf("lock ownership changed for %s; refusing to remove another process's lock", l.path)
	}
	if err := fsx.RemoveFile(l.path); err != nil && first == nil {
		first = err
	}
	return first
}

func newToken() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
