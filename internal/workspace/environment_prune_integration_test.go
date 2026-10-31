package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

func TestEnvironmentPublicationExcludesConcurrentCachePrune(t *testing.T) {
	for _, operation := range []string{"capture", "checkpoint", "adopt"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			repo := readinessRepository(t)
			writeReadinessFile(t, repo, ".gitignore", "deps/\n")
			writeReadinessFile(t, repo, "deps.lock", "v1\n")
			writeReadinessFile(t, repo, ".cambium.toml", `version = 1
[[path]]
path = "deps"
policy = "clone"
inputs = ["deps.lock"]
required = true
validate = ["cmp", "deps.lock", "deps/state"]
`)
			commitReadinessFiles(t, repo)
			writeReadinessFile(t, repo, "deps/state", "v1\n")
			manager := readinessManager(t, repo, true)
			var workspacePath string
			if operation == "adopt" {
				workspacePath = filepath.Join(canonicalTempDir(t), "external")
				runGitTest(t, repo, "worktree", "add", "-b", "external", workspacePath, "HEAD")
			} else {
				value, err := manager.Create(ctx, CreateSpec{Name: "subject", Materializer: model.MaterializerGit})
				if err != nil {
					t.Fatal(err)
				}
				workspacePath = value.Path
			}
			writeReadinessFile(t, workspacePath, "deps.lock", "v2\n")
			writeReadinessFile(t, workspacePath, "deps/state", "v2\n")
			if operation != "checkpoint" {
				commitReadinessFiles(t, workspacePath)
			}

			runner := &upgradeCopyRunner{source: filepath.Join(workspacePath, "deps"), entered: make(chan struct{}), resume: make(chan struct{})}
			manager.Native.Cloner.Runner = runner
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
			done := make(chan struct{})
			var operationErr error
			var capturedIDs []string
			go func() {
				defer close(done)
				switch operation {
				case "capture":
					value, err := manager.CaptureEnvironment(ctx, "subject", "")
					operationErr = err
					for _, path := range value.Paths {
						capturedIDs = append(capturedIDs, path.ID)
					}
				case "checkpoint":
					value, err := manager.Checkpoint(ctx, "subject", CheckpointOptions{RefName: "captured"})
					operationErr, capturedIDs = err, value.EnvironmentIDs
				case "adopt":
					value, err := manager.Adopt(ctx, "subject", workspacePath)
					operationErr, capturedIDs = err, value.LayerIDs
				}
			}()
			t.Cleanup(func() {
				release()
				cancel()
				<-done
			})
			select {
			case <-runner.entered:
			case <-ctx.Done():
				t.Fatal("environment publication did not reach the payload-copy pause")
			}

			// Prune must wait for publication and the durable reference, even
			// when the user explicitly selects every old-enough cache entry.
			pruneCtx, pruneCancel := context.WithTimeout(ctx, 150*time.Millisecond)
			pruned, pruneErr := manager.PruneCaches(pruneCtx, time.Nanosecond, false)
			pruneCancel()
			release()
			<-done
			if !errors.Is(pruneErr, context.DeadlineExceeded) {
				t.Fatalf("prune was not excluded during %s: removed=%#v prune error=%v operation error=%v", operation, pruned.Removed, pruneErr, operationErr)
			}
			if operationErr != nil {
				t.Fatalf("environment publication failed: %v", operationErr)
			}
			if len(capturedIDs) != 1 {
				t.Fatalf("expected one published environment receipt, got %v", capturedIDs)
			}
			if _, err := manager.PruneCaches(ctx, time.Nanosecond, false); err != nil {
				t.Fatal(err)
			}
			layers, err := native.LoadLayers(manager.Project, capturedIDs)
			if err != nil || len(layers) != 1 || !layers[0].Present {
				t.Fatalf("completed publication was not protected by its durable reference: %#v, %v", layers, err)
			}
			assertFile(t, filepath.Join(layers[0].Payload, "state"), "v2\n")
		})
	}
}
