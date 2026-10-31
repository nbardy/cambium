package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/failpoint"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/project"
)

func TestReconcileAndRemoveRejectReusedGitAdministrationDirectory(t *testing.T) {
	for _, origin := range []string{"created", "adopted"} {
		for _, mode := range []string{"reconcile-moved-replacement", "remove-same-path-replacement"} {
			t.Run(origin+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				repo := initSpecRepo(t)
				p, err := project.Open(ctx, repo, nil)
				if err != nil {
					t.Fatal(err)
				}
				manager := New(p)
				parent := canonicalTempDir(t)
				originalPath := filepath.Join(parent, "original", "checkout")
				var original model.Workspace
				if origin == "created" {
					original, err = manager.Create(ctx, CreateSpec{Name: "agent", Path: originalPath, Materializer: model.MaterializerGit})
				} else {
					runGitTest(t, repo, "worktree", "add", "-b", "original", originalPath, "main")
					original, err = manager.Adopt(ctx, "agent", originalPath)
				}
				if err != nil {
					t.Fatal(err)
				}
				runGitTest(t, repo, "worktree", "remove", original.Path)
				replacementPath := original.Path
				replacementBranch := original.Branch
				if mode == "reconcile-moved-replacement" {
					replacementPath = filepath.Join(parent, "replacement", "checkout")
					replacementBranch = "replacement"
					runGitTest(t, repo, "worktree", "add", "-b", replacementBranch, replacementPath, "main")
				} else {
					runGitTest(t, repo, "worktree", "add", replacementPath, replacementBranch)
				}
				head, err := p.Repository.GitPath(ctx, replacementPath, "HEAD")
				if err != nil || filepath.Dir(head) != original.WorktreeGitDir {
					t.Fatalf("fixture did not reuse Git's administration slot: before=%s after=%s err=%v", original.WorktreeGitDir, filepath.Dir(head), err)
				}
				if mode == "reconcile-moved-replacement" {
					if _, err := manager.Reconcile(ctx, false); err != nil {
						t.Fatal(err)
					}
					updated, err := manager.Registry.Load(original.Name)
					if err != nil {
						t.Fatal(err)
					}
					if updated.Path != original.Path || updated.Branch != original.Branch || updated.Status != model.WorkspaceBroken {
						t.Errorf("reconcile rebound the old identity to an unrelated replacement: %#v", updated)
					}
				}
				if err := manager.Remove(ctx, original.Name, true, true); err == nil {
					t.Error("old workspace metadata authorized removal of a replacement registration")
				}
				if _, err := os.Stat(replacementPath); err != nil {
					t.Fatalf("replacement working tree was deleted: %v", err)
				}
				if exists, err := p.Repository.BranchExists(ctx, replacementBranch); err != nil || !exists {
					t.Fatalf("replacement branch was deleted: exists=%t err=%v", exists, err)
				}
			})
		}
	}
}

func TestLegacyWorkspaceOwnershipRequiresExistingReceipt(t *testing.T) {
	for _, receipt := range []string{"matching", "missing", "mismatched", "empty", "symlink"} {
		t.Run(receipt, func(t *testing.T) {
			ctx := context.Background()
			repo := initSpecRepo(t)
			p, err := project.Open(ctx, repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(p)
			original, err := manager.Create(ctx, CreateSpec{Name: "legacy", Materializer: model.MaterializerGit})
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(original.WorktreeGitDir, "cambium-owner")
			original.WorktreeGitDir = "" // Metadata written before this locator existed.
			if err := manager.Registry.Save(original); err != nil {
				t.Fatal(err)
			}
			path := original.Path
			if receipt == "matching" {
				path = filepath.Join(canonicalTempDir(t), "moved")
				runGitTest(t, repo, "worktree", "move", original.Path, path)
				runGitTest(t, path, "branch", "-m", "renamed")
			} else {
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				switch receipt {
				case "mismatched":
					err = os.WriteFile(marker, []byte("another-workspace\n"), 0o600)
				case "empty":
					err = os.WriteFile(marker, nil, 0o600)
				case "symlink":
					target := filepath.Join(canonicalTempDir(t), "untrusted-owner")
					if err = os.WriteFile(target, []byte(original.ID+"\n"), 0o600); err == nil {
						err = os.Symlink(target, marker)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := manager.Registry.Load(original.Name)
			if err != nil {
				t.Fatal(err)
			}
			dry, err := manager.Reconcile(ctx, true)
			if err != nil {
				t.Fatal(err)
			}
			unchanged, err := manager.Registry.Load(original.Name)
			if err != nil || !reflect.DeepEqual(unchanged, before) {
				t.Fatalf("dry run changed legacy metadata: %#v, %v", unchanged, err)
			}
			actual, err := manager.Reconcile(ctx, false)
			if err != nil || !reflect.DeepEqual(actual.Changes, dry.Changes) {
				t.Fatalf("ownership repair differs from dry run: dry=%#v actual=%#v err=%v", dry, actual, err)
			}
			value, err := manager.Registry.Load(original.Name)
			if err != nil {
				t.Fatal(err)
			}
			if receipt == "matching" {
				if value.Status != model.WorkspaceReady || value.ID != original.ID || value.Path != path || value.Branch != "renamed" || value.WorktreeGitDir == "" {
					t.Fatalf("matching receipt did not preserve legacy identity across move and rename: %#v", value)
				}
				if err := manager.Remove(ctx, original.Name, false, true); err != nil {
					t.Fatalf("verified legacy workspace cannot be removed: %v", err)
				}
				return
			}
			if value.Status != model.WorkspaceBroken || value.Path != original.Path || value.Branch != original.Branch || value.WorktreeGitDir != "" {
				t.Fatalf("unverified legacy workspace was silently migrated: %#v", value)
			}
			if err := manager.Remove(ctx, original.Name, true, true); !errors.Is(err, native.ErrWorktreeOwnership) {
				t.Fatalf("removal did not reject missing/invalid ownership proof: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("unverified legacy worktree was removed: %v", err)
			}
			if receipt == "missing" {
				if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("reconcile/removal created a missing ownership receipt: %v", err)
				}
			} else if owner, err := os.ReadFile(marker); err != nil || (receipt == "mismatched" && string(owner) != "another-workspace\n") || (receipt == "empty" && len(owner) != 0) {
				t.Fatalf("reconcile/removal rewrote the existing receipt: %q, %v", owner, err)
			}
		})
	}
}

func TestAdoptionPublishesReceiptBeforeMetadata(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		name := "complete"
		if interrupt {
			name = "interrupted"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo := initSpecRepo(t)
			path := filepath.Join(canonicalTempDir(t), "external")
			runGitTest(t, repo, "worktree", "add", "-b", "external", path, "main")
			p, err := project.Open(ctx, repo, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := New(p)
			if interrupt {
				t.Setenv("CAMBIUM_FAILPOINT", "after-adopt-claim")
			}
			value, err := manager.Adopt(ctx, "adopted", path)
			if interrupt {
				if !errors.Is(err, failpoint.ErrInjectedCrash) {
					t.Fatalf("adoption did not reach receipt publication: %v", err)
				}
				t.Setenv("CAMBIUM_FAILPOINT", "")
			} else if err != nil {
				t.Fatal(err)
			}
			gitDir, owner, err := manager.Native.ReadWorktreeOwner(ctx, path)
			if err != nil || owner == "" {
				t.Fatalf("adoption did not persist its owner receipt: gitDir=%q owner=%q err=%v", gitDir, owner, err)
			}
			if !interrupt {
				if owner != value.ID || gitDir != value.WorktreeGitDir {
					t.Fatalf("adoption published metadata without matching ownership: %#v owner=%s gitDir=%s", value, owner, gitDir)
				}
				if err := manager.Remove(ctx, value.Name, false, true); err != nil {
					t.Fatal(err)
				}
				return
			}
			if values, err := manager.Registry.List(); err != nil || len(values) != 0 {
				t.Fatalf("interrupted adoption published registry metadata: %#v, %v", values, err)
			}
			if _, err := manager.Adopt(ctx, "retry", path); err == nil || !strings.Contains(err.Error(), "ownership receipt") {
				t.Fatalf("retry claimed an interrupted adoption's receipt: %v", err)
			}
			if _, err := manager.Recover(ctx, false); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("interrupted adoption deleted its pre-existing worktree: %v", err)
			}
			_, retainedOwner, err := manager.Native.ReadWorktreeOwner(ctx, path)
			if err != nil || retainedOwner != owner {
				t.Fatalf("failure handling replaced the retained ownership receipt: owner=%q err=%v", retainedOwner, err)
			}
		})
	}
}
