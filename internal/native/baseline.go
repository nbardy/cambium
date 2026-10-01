package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/project"
)

type Baseline struct {
	Version      int       `json:"version"`
	Commit       string    `json:"commit"`
	Tree         string    `json:"tree"`
	Index        string    `json:"index"`
	TrackedFiles int64     `json:"tracked_files"`
	LogicalBytes int64     `json:"logical_bytes"`
	CreatedAt    time.Time `json:"created_at"`
}

func EnsureBaseline(ctx context.Context, project *project.Project, commit string) (Baseline, error) {
	if !isHexObjectID(commit) {
		return Baseline{}, fmt.Errorf("invalid commit id %q", commit)
	}
	finalRoot := filepath.Join(project.BaselinesDir(), commit)
	if baseline, ok := loadBaseline(finalRoot); ok {
		if err := touchCacheEntry(finalRoot); err != nil {
			return Baseline{}, err
		}
		return baseline, nil
	}
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(project.LocksDir(), "baseline-"+commit+".lock"))
	if err != nil {
		return Baseline{}, err
	}
	defer lock.Release()
	if baseline, ok := loadBaseline(finalRoot); ok {
		if err := touchCacheEntry(finalRoot); err != nil {
			return Baseline{}, err
		}
		return baseline, nil
	}
	if _, err := os.Stat(finalRoot); err == nil {
		return Baseline{}, fmt.Errorf("incomplete baseline exists at %s; run `cambium doctor --repair`", finalRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Baseline{}, err
	}

	temporary, err := os.MkdirTemp(project.BaselinesDir(), ".baseline-"+commit+"-*")
	if err != nil {
		return Baseline{}, err
	}
	defer os.RemoveAll(temporary)
	tree := filepath.Join(temporary, "tree")
	index := filepath.Join(temporary, "index")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		return Baseline{}, err
	}
	env := map[string]string{"GIT_INDEX_FILE": index}
	if _, err := project.Repository.RunCommon(ctx, env, "read-tree", commit); err != nil {
		return Baseline{}, fmt.Errorf("initialize baseline index: %w", err)
	}
	args := []string{"--git-dir=" + project.Repository.CommonGitDir, "--work-tree=" + tree, "checkout-index", "--all", "--force"}
	if _, err := project.Runner.Run(ctx, execx.Command{Dir: project.Repository.Root, Env: env, Name: "git", Args: args}); err != nil {
		return Baseline{}, fmt.Errorf("materialize immutable baseline: %w", err)
	}
	// Persist stat information after checkout. Installing this prepared index is
	// optional; Git status remains the final correctness check in every case.
	if _, err := project.Runner.Run(ctx, execx.Command{
		Dir: tree, Env: env, Name: "git",
		Args: []string{"--git-dir=" + project.Repository.CommonGitDir, "--work-tree=" + tree, "update-index", "--refresh"},
	}); err != nil {
		return Baseline{}, fmt.Errorf("refresh baseline index: %w", err)
	}
	files, bytes, err := treeStats(tree)
	if err != nil {
		return Baseline{}, err
	}
	baseline := Baseline{
		Version:      2,
		Commit:       commit,
		Tree:         filepath.Join(finalRoot, "tree"),
		Index:        filepath.Join(finalRoot, "index"),
		TrackedFiles: files,
		LogicalBytes: bytes,
		CreatedAt:    time.Now().UTC(),
	}
	metadata, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return Baseline{}, err
	}
	metadata = append(metadata, '\n')
	if err := os.WriteFile(filepath.Join(temporary, "metadata.json"), metadata, 0o644); err != nil {
		return Baseline{}, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "ready"), []byte(commit+"\n"), 0o444); err != nil {
		return Baseline{}, err
	}
	if err := fsx.PublishDir(temporary, finalRoot); err != nil {
		if existing, ok := loadBaseline(finalRoot); ok {
			return existing, nil
		}
		return Baseline{}, err
	}
	return baseline, nil
}

func InstallPreparedIndex(ctx context.Context, project *project.Project, baseline Baseline, worktree string) error {
	if baseline.Index == "" {
		return errors.New("baseline has no prepared index")
	}
	indexPath, err := project.Repository.GitPath(ctx, worktree, "index")
	if err != nil {
		return fmt.Errorf("resolve worktree index: %w", err)
	}
	if err := fsx.CopyFileAtomic(baseline.Index, indexPath, 0o600); err != nil {
		return fmt.Errorf("install prepared index: %w", err)
	}
	return nil
}

func loadBaseline(root string) (Baseline, bool) {
	if _, err := os.Stat(filepath.Join(root, "ready")); err != nil {
		return Baseline{}, false
	}
	bytes, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	if err != nil {
		return Baseline{}, false
	}
	var baseline Baseline
	if json.Unmarshal(bytes, &baseline) != nil || baseline.Version != 2 {
		return Baseline{}, false
	}
	if info, err := os.Stat(filepath.Join(root, "tree")); err != nil || !info.IsDir() {
		return Baseline{}, false
	}
	if info, err := os.Stat(filepath.Join(root, "index")); err != nil || !info.Mode().IsRegular() {
		return Baseline{}, false
	}
	return baseline, true
}

func treeStats(root string) (files, bytes int64, err error) {
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}

func isHexObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range strings.ToLower(value) {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}
