package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

// These regressions use real linked worktrees and checkpoint receipts. Only
// the payload-copy failure/pause is injected, after the real validator runs.
func TestFailedMissingEnvironmentUpgradePreservesExistingReceipts(t *testing.T) {
	for _, failure := range []string{"validation", "copy"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			repo, manager, first := missingUpgradeFixture(t)
			checkpoint, err := manager.Checkpoint(ctx, first.Name, CheckpointOptions{RefName: "before-install"})
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(manager.Project.LayersDir(), first.LayerIDs[0], "metadata.json")
			before, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			installed := "v1\n"
			if failure == "validation" {
				installed = "wrong-version\n"
			} else {
				manager.Native.Cloner.Runner = &upgradeCopyRunner{source: filepath.Join(manager.Project.Repository.Root, "deps"), fail: true}
			}
			writeReadinessFile(t, repo, "deps/state", installed)
			if _, err := manager.Create(ctx, CreateSpec{Name: "failed-upgrade", Materializer: model.MaterializerGit}); err == nil {
				t.Fatalf("%s failure unexpectedly succeeded", failure)
			}
			after, err := os.ReadFile(metadataPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed upgrade did not preserve the existing receipt: %v", err)
			}
			for _, ids := range [][]string{first.LayerIDs, checkpoint.EnvironmentIDs} {
				layers, err := native.LoadLayers(manager.Project, ids)
				if err != nil || len(layers) != 1 || layers[0].Present {
					t.Fatalf("failed upgrade invalidated an existing workspace/checkpoint reference: %#v, %v", layers, err)
				}
			}
			explanation, err := manager.ExplainEnvironment(ctx, first.Name)
			if err != nil || explanation.Ready || !slices.Contains(explanation.Missing, "deps") {
				t.Fatalf("existing workspace cannot explain its still-missing environment: %#v, %v", explanation, err)
			}

			// A crash after staging a payload must not make the next retry fail
			// or publish stale bytes. The old metadata still describes absence.
			writeReadinessFile(t, filepath.Dir(metadataPath), "payload/state", "orphan from an interrupted attempt\n")
			writeReadinessFile(t, repo, "deps/state", "v1\n")
			manager.Native.Cloner = native.Cloner{}
			second, err := manager.Create(ctx, CreateSpec{Name: "successful-upgrade", Materializer: model.MaterializerGit})
			if err != nil || !second.EnvironmentReady {
				t.Fatalf("a valid retry could not upgrade the preserved receipt: %#v, %v", second, err)
			}
			assertFile(t, filepath.Join(second.Path, "deps", "state"), "v1\n")
			if _, err := native.LoadLayers(manager.Project, checkpoint.EnvironmentIDs); err != nil {
				t.Fatalf("checkpoint references became invalid after successful upgrade: %v", err)
			}
		})
	}
}

func TestMissingEnvironmentReceiptRemainsReadableDuringUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo, manager, first := missingUpgradeFixture(t)
	writeReadinessFile(t, repo, "deps/state", "v1\n")
	runner := &upgradeCopyRunner{source: filepath.Join(manager.Project.Repository.Root, "deps"), entered: make(chan struct{}), resume: make(chan struct{})}
	manager.Native.Cloner.Runner = runner
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(runner.resume) }) }
	done := make(chan struct{})
	var createErr error
	go func() {
		defer close(done)
		_, createErr = manager.Create(ctx, CreateSpec{Name: "upgrading", Materializer: model.MaterializerGit})
	}()
	t.Cleanup(func() {
		release()
		cancel()
		<-done
	})
	select {
	case <-runner.entered:
	case <-ctx.Done():
		t.Fatal("upgrade did not reach the payload-copy pause")
	}
	layers, err := native.LoadLayers(manager.Project, first.LayerIDs)
	if err != nil || len(layers) != 1 || layers[0].Present {
		t.Fatalf("in-flight upgrade hid the existing missing receipt: %#v, %v", layers, err)
	}
	release()
	for {
		if _, err := native.LoadLayers(manager.Project, first.LayerIDs); err != nil {
			t.Fatalf("reader observed an incomplete receipt during publication: %v", err)
		}
		select {
		case <-done:
			if createErr != nil {
				t.Fatal(createErr)
			}
			return
		default:
		}
	}
}

func missingUpgradeFixture(t *testing.T) (string, *Manager, model.Workspace) {
	t.Helper()
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
	manager := readinessManager(t, repo, true)
	first, err := manager.Create(context.Background(), CreateSpec{Name: "before-install", Materializer: model.MaterializerGit})
	if err != nil || first.EnvironmentReady || len(first.LayerIDs) != 1 {
		t.Fatalf("could not create the missing-environment fixture: %#v, %v", first, err)
	}
	return repo, manager, first
}

type upgradeCopyRunner struct {
	execx.OSRunner
	source  string
	fail    bool
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *upgradeCopyRunner) Run(ctx context.Context, command execx.Command) (execx.Result, error) {
	if command.Name == "cp" && len(command.Args) > 1 {
		source := filepath.Clean(command.Args[len(command.Args)-2])
		destination := command.Args[len(command.Args)-1]
		if source == r.source && filepath.Base(destination) == "payload" && strings.Contains(destination, ".layer-") {
			if r.entered != nil {
				r.once.Do(func() { close(r.entered) })
				select {
				case <-r.resume:
				case <-ctx.Done():
					return execx.Result{}, ctx.Err()
				}
			}
			if r.fail {
				if err := os.WriteFile(filepath.Join(destination, "partial"), []byte("partial copy"), 0o644); err != nil {
					return execx.Result{}, err
				}
				return execx.Result{}, errors.New("injected payload-copy failure")
			}
		}
	}
	return r.OSRunner.Run(ctx, command)
}
