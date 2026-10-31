package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/workspace"
)

func TestCLIRunPreservesSameNameReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this regression removes a running child's working directory")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is required")
	}
	for _, test := range []struct {
		name          string
		cleanup       string
		createWithRun bool
		childExit     int
	}{
		{name: "discard", cleanup: "--discard-on-exit"},
		{name: "clean", cleanup: "--cleanup"},
		{name: "newly-created-discard", cleanup: "--discard-on-exit", createWithRun: true},
		{name: "touch-after-success"},
		{name: "touch-after-failure", childExit: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireCLIGit(t)
			root := initializeCLIRepository(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			control, _, controlStderr := testApp(root)
			manager, err := control.manager(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !test.createWithRun {
				if _, err := manager.Create(ctx, workspace.CreateSpec{Name: "subject", Materializer: model.MaterializerGit}); err != nil {
					t.Fatal(err)
				}
			}
			childInput, releaseInput, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer childInput.Close()
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() {
					_, _ = fmt.Fprintln(releaseInput, "continue")
					_ = releaseInput.Close()
				})
			}
			app, _, stderr := testApp(root)
			ready := &childReadyWriter{ready: make(chan struct{})}
			app.Stdout, app.Stdin = ready, childInput
			args := []string{"run", "--materializer", "git", "--delete-branch"}
			if test.cleanup != "" {
				args = append(args, test.cleanup)
			}
			args = append(args, "subject", "--", "sh", "-c", "printf 'child-ready\\n'; read token; exit \"$1\"", "barrier", strconv.Itoa(test.childExit))
			done := make(chan struct{})
			var code int
			go func() {
				defer close(done)
				code = app.Run(ctx, args)
			}()
			t.Cleanup(func() {
				release()
				cancel()
				<-done
			})
			select {
			case <-ready.ready:
			case <-done:
				t.Fatalf("child exited before the barrier: code=%d stderr=%s", code, stderr)
			case <-ctx.Done():
				t.Fatal("child did not reach the barrier")
			}
			original, err := manager.Registry.Load("subject")
			if err != nil {
				t.Fatal(err)
			}
			// Explicitly remove the original while its child is blocked, then
			// reuse both its name and path for an independent workspace.
			if code := control.Run(ctx, []string{"remove", "--force", "--delete-branch", "subject"}); code != 0 {
				t.Fatalf("remove original: %s", controlStderr)
			}
			replacement, err := manager.Create(ctx, workspace.CreateSpec{Name: "subject", Path: original.Path, Materializer: model.MaterializerGit})
			if err != nil || replacement.ID == original.ID {
				t.Fatalf("could not create independent replacement: %#v, %v", replacement, err)
			}
			replacement.LastUsedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := manager.Registry.Save(replacement); err != nil {
				t.Fatal(err)
			}
			if test.cleanup == "--discard-on-exit" {
				if err := os.WriteFile(filepath.Join(replacement.Path, "replacement-only.txt"), []byte("new work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			release()
			<-done
			current, err := manager.Registry.Load("subject")
			if err != nil || current.ID != replacement.ID {
				t.Fatalf("child cleanup removed the replacement: current=%#v error=%v code=%d stderr=%s", current, err, code, stderr)
			}
			if !current.LastUsedAt.Equal(replacement.LastUsedAt) {
				t.Fatalf("child bookkeeping touched a different workspace: got %s, want %s", current.LastUsedAt, replacement.LastUsedAt)
			}
			if data, err := os.ReadFile(filepath.Join(current.Path, "src", "a.txt")); err != nil || string(data) != "alpha\n" {
				t.Fatalf("replacement worktree was damaged: %q, %v", data, err)
			}
			if test.cleanup == "--discard-on-exit" {
				if data, err := os.ReadFile(filepath.Join(current.Path, "replacement-only.txt")); err != nil || string(data) != "new work\n" {
					t.Fatalf("replacement's new work was lost: %q, %v", data, err)
				}
			}
			if exists, err := manager.Project.Repository.BranchExists(ctx, replacement.Branch); err != nil || !exists {
				t.Fatalf("replacement branch was deleted: %t, %v", exists, err)
			}
			if test.cleanup != "" {
				if code == 0 || !strings.Contains(stderr.String(), "replaced") {
					t.Fatalf("cleanup did not explain the identity mismatch: code=%d stderr=%s", code, stderr)
				}
			} else if code != test.childExit {
				t.Fatalf("bookkeeping changed the child's exit code: got %d, want %d; %s", code, test.childExit, stderr)
			}
		})
	}
}

type childReadyWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	once   sync.Once
	ready  chan struct{}
}

func (w *childReadyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if bytes.Contains(w.buffer.Bytes(), []byte("child-ready\n")) {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}
