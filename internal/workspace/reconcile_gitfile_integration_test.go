package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/project"
)

func TestReconcileContinuesPastBrokenCheckoutGitFiles(t *testing.T) {
	for _, managed := range []bool{true, false} {
		for _, damage := range []string{"missing", "malformed", "missing-target", "empty-target", "directory"} {
			name := "unmanaged/" + damage
			if managed {
				name = "managed/" + damage
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				repo := initSpecRepo(t)
				brokenPath := filepath.Join(canonicalTempDir(t), "broken")
				runGitTest(t, repo, "worktree", "add", "-b", "external", brokenPath, "main")
				p, err := project.Open(ctx, repo, nil)
				if err != nil {
					t.Fatal(err)
				}
				manager := New(p)
				var broken model.Workspace
				if managed {
					broken, err = manager.Adopt(ctx, "broken", brokenPath)
					if err != nil {
						t.Fatal(err)
					}
				}
				healthy, err := manager.Create(ctx, CreateSpec{Name: "healthy", Materializer: model.MaterializerGit})
				if err != nil {
					t.Fatal(err)
				}
				healthy.Status = model.WorkspaceBroken
				if err := manager.Registry.Save(healthy); err != nil {
					t.Fatal(err)
				}
				gitfile := filepath.Join(brokenPath, ".git")
				if err := os.Rename(gitfile, gitfile+".saved"); err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "malformed":
					err = os.WriteFile(gitfile, []byte("not a gitfile\n"), 0o600)
				case "missing-target":
					err = os.WriteFile(gitfile, []byte("gitdir: "+filepath.Join(brokenPath, "missing-admin")+"\n"), 0o600)
				case "empty-target":
					empty := filepath.Join(brokenPath, "empty-admin")
					if err = os.Mkdir(empty, 0o755); err == nil {
						err = os.WriteFile(gitfile, []byte("gitdir: "+empty+"\n"), 0o600)
					}
				case "directory":
					err = os.Mkdir(gitfile, 0o755)
				}
				if err != nil {
					t.Fatal(err)
				}
				before, err := manager.Registry.List()
				if err != nil {
					t.Fatal(err)
				}
				dry, err := manager.Reconcile(ctx, true)
				if err != nil {
					t.Fatalf("broken checkout metadata aborted reconciliation of its healthy neighbor: %v", err)
				}
				unchanged, err := manager.Registry.List()
				if err != nil || !reflect.DeepEqual(unchanged, before) {
					t.Fatalf("dry run changed workspace metadata: before=%#v after=%#v err=%v", before, unchanged, err)
				}
				actual, err := manager.Reconcile(ctx, false)
				if err != nil || !reflect.DeepEqual(actual.Changes, dry.Changes) || !slices.Equal(actual.Unmanaged, dry.Unmanaged) {
					t.Fatalf("actual reconciliation differs from dry run: dry=%#v actual=%#v err=%v", dry, actual, err)
				}
				updated, err := manager.Registry.Load(healthy.Name)
				if err != nil || updated.Status != model.WorkspaceReady || updated.ID != healthy.ID {
					t.Fatalf("healthy neighbor was not repaired: %#v, %v", updated, err)
				}
				if managed {
					updated, err := manager.Registry.Load(broken.Name)
					if err != nil || updated.Status != model.WorkspaceBroken || updated.ID != broken.ID || updated.Path != broken.Path {
						t.Fatalf("damaged managed checkout was not marked broken: %#v, %v", updated, err)
					}
					if err := manager.Remove(ctx, broken.Name, true, true); !errors.Is(err, native.ErrWorktreeOwnership) {
						t.Fatalf("removal did not reject the damaged checkout as unverified ownership: %v", err)
					}
				} else if !slices.Contains(actual.Unmanaged, brokenPath) {
					t.Fatalf("damaged unmanaged registration disappeared from the result: %#v", actual)
				}
				if _, err := os.Stat(filepath.Join(brokenPath, "tracked.txt")); err != nil {
					t.Fatalf("damaged checkout contents were changed: %v", err)
				}
			})
		}
	}
}

func TestOwnershipCheckAcceptsRelativeGitFiles(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	value, err := manager.Create(ctx, CreateSpec{Name: "relative", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(value.Path, value.WorktreeGitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(value.Path, ".git"), []byte("gitdir: "+relative+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitDir, err := manager.Native.VerifyWorkspaceOwnership(ctx, value)
	if err != nil || gitDir != value.WorktreeGitDir {
		t.Fatalf("valid relative gitfile lost its ownership: gitDir=%q err=%v", gitDir, err)
	}
}

func TestOwnershipCheckPreservesGitConfigurationErrors(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	value, err := manager.Create(ctx, CreateSpec{Name: "config-error", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "config", "extensions.worktreeConfig", "true")
	if err := os.WriteFile(filepath.Join(value.WorktreeGitDir, "config.worktree"), []byte("[invalid config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = manager.Native.ReadWorktreeOwner(ctx, value.Path)
	var commandErr *execx.CommandError
	if err == nil || errors.Is(err, native.ErrWorktreeOwnership) || !errors.As(err, &commandErr) || commandErr.Result.ExitCode != 128 {
		t.Fatalf("Git configuration failure was hidden as broken ownership: %v", err)
	}
	if _, err := manager.Reconcile(ctx, false); err == nil || errors.Is(err, native.ErrWorktreeOwnership) {
		t.Fatalf("reconciliation hid an unrelated Git failure: %v", err)
	}
}

func TestOwnershipCheckPreservesCancellationAndExecutionErrors(t *testing.T) {
	ctx := context.Background()
	repo := initSpecRepo(t)
	p, err := project.Open(ctx, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(p)
	value, err := manager.Create(ctx, CreateSpec{Name: "operational-error", Materializer: model.MaterializerGit})
	if err != nil {
		t.Fatal(err)
	}
	failure := &os.PathError{Op: "fork/exec", Path: "git", Err: os.ErrPermission}
	faulty, err := project.Open(ctx, repo, ownershipExecutionFailure{failure: failure})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(faulty).Native.ReadWorktreeOwner(ctx, value.Path); !errors.Is(err, failure) || errors.Is(err, native.ErrWorktreeOwnership) {
		t.Fatalf("Git execution failure was hidden as broken ownership: %v", err)
	}
	// Cancellation must also take priority over a checkout that is itself
	// damaged, so a cancelled refresh does not publish a broken observation.
	if err := os.Remove(filepath.Join(value.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := manager.Native.ReadWorktreeOwner(cancelled, value.Path); !errors.Is(err, context.Canceled) || errors.Is(err, native.ErrWorktreeOwnership) {
		t.Fatalf("cancellation was hidden as broken ownership: %v", err)
	}
}

type ownershipExecutionFailure struct {
	execx.OSRunner
	failure error
}

func (r ownershipExecutionFailure) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	if command.Name == "git" && slices.Contains(command.Args, "--git-path") && slices.Contains(command.Args, "cambium-owner") {
		return execx.Result{}, r.failure
	}
	return r.OSRunner.Run(ctx, command)
}
