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
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

type Baseline struct {
	Version      int       `json:"version"`
	Commit       string    `json:"commit"`
	Tree         string    `json:"tree"`
	Index        string    `json:"index"`
	Source       string    `json:"source"` // "checkout" or "derived:<parent commit>"
	TrackedFiles int64     `json:"tracked_files"`
	LogicalBytes int64     `json:"logical_bytes"`
	CreatedAt    time.Time `json:"created_at"`
}

// EnsureBaseline returns the immutable baseline for commit, building it once.
//
// A full checkout of a large repository costs its whole tracked size (about
// 0.8 GB for a 761 MB tree measured on 2026-10-01), and agent swarms start from
// a new commit almost every time, so baselines used to dominate disk. When CoW
// is available, a new baseline is instead derived from the closest ready
// baseline: clone it (shared blocks, ~free) and rewrite only changed paths.
// The same repository then paid 18 MB for an 876-file diff instead of 812 MB.
func EnsureBaseline(ctx context.Context, project *project.Project, cloner Cloner, commit string) (Baseline, error) {
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
	source, err := chooseBaselineSource(ctx, project, cloner, commit)
	if err != nil {
		return Baseline{}, err
	}
	if err := source.materialize(ctx, project, cloner, commit, tree, index); err != nil {
		return Baseline{}, fmt.Errorf("materialize immutable baseline (%s): %w", source.describe(), err)
	}
	// Persist stat information after materializing. Installing this prepared
	// index is optional; Git status remains the final correctness check.
	if err := baselineGit(ctx, project, tree, index, "update-index", "--refresh"); err != nil {
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
		Source:       source.describe(),
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

// baselineSource is how a new baseline tree is produced. Choosing the source is
// the only decision; each source then has one straight path.
type baselineSource interface {
	materialize(ctx context.Context, project *project.Project, cloner Cloner, commit, tree, index string) error
	describe() string
}

// fullCheckout writes every tracked file. It costs the full tracked size.
type fullCheckout struct{}

// derivedBaseline CoW-clones a ready parent baseline and rewrites only the
// paths that differ between parent and commit. Unchanged files keep sharing
// physical blocks with the parent, so cost is proportional to the diff.
type derivedBaseline struct {
	parent  Baseline
	changed int
}

func (fullCheckout) describe() string      { return "checkout" }
func (d derivedBaseline) describe() string { return "derived:" + d.parent.Commit }

func (fullCheckout) materialize(ctx context.Context, project *project.Project, _ Cloner, commit, tree, index string) error {
	if err := os.MkdirAll(tree, 0o755); err != nil {
		return err
	}
	if _, err := project.Repository.RunCommon(ctx, map[string]string{"GIT_INDEX_FILE": index}, "read-tree", commit); err != nil {
		return fmt.Errorf("initialize baseline index: %w", err)
	}
	return baselineGit(ctx, project, tree, index, "checkout-index", "--all", "--force")
}

func (d derivedBaseline) materialize(ctx context.Context, project *project.Project, cloner Cloner, commit, tree, index string) error {
	if _, err := cloner.CloneTree(ctx, d.parent.Tree, tree, model.MaterializerCoW); err != nil {
		return fmt.Errorf("clone parent baseline %s: %w", d.parent.Commit, err)
	}
	if _, err := project.Repository.RunCommon(ctx, map[string]string{"GIT_INDEX_FILE": index}, "read-tree", d.parent.Commit); err != nil {
		return fmt.Errorf("initialize parent index: %w", err)
	}
	// Clones get new inodes and ctimes, so the fresh index's stat data does not
	// match them. Without this refresh, read-tree -m refuses every path with
	// "not uptodate. Cannot merge".
	if err := baselineGit(ctx, project, tree, index, "update-index", "--refresh"); err != nil {
		return fmt.Errorf("refresh parent index: %w", err)
	}
	if err := baselineGit(ctx, project, tree, index, "read-tree", "-m", "-u", d.parent.Commit, commit); err != nil {
		return fmt.Errorf("apply %s..%s: %w", d.parent.Commit, commit, err)
	}
	if err := baselineGit(ctx, project, tree, index, "update-index", "--refresh"); err != nil {
		return fmt.Errorf("refresh derived index: %w", err)
	}
	// A derived tree must be byte-identical to a fresh checkout of commit.
	if err := baselineGit(ctx, project, tree, index, "diff-index", "--quiet", commit, "--"); err != nil {
		return fmt.Errorf("derived baseline does not match %s: %w", commit, err)
	}
	return nil
}

// chooseBaselineSource derives from the ready baseline with the fewest changed
// paths when CoW is available and the diff is under half the tree; otherwise
// a full checkout is as cheap and simpler.
func chooseBaselineSource(ctx context.Context, project *project.Project, cloner Cloner, commit string) (baselineSource, error) {
	if !cloner.Probe(ctx, project.BaselinesDir()).Supported {
		return fullCheckout{}, nil
	}
	entries, err := os.ReadDir(project.BaselinesDir())
	if err != nil {
		return nil, err
	}
	var best derivedBaseline
	found := false
	for _, entry := range entries {
		parent, ok := loadBaseline(filepath.Join(project.BaselinesDir(), entry.Name()))
		if !ok || parent.Commit == commit {
			continue
		}
		result, err := project.Repository.RunCommon(ctx, nil, "diff-tree", "-r", "--no-renames", "--name-only", parent.Commit, commit)
		if err != nil {
			return nil, fmt.Errorf("diff baseline %s against %s: %w", parent.Commit, commit, err)
		}
		changed := len(strings.Fields(result.Stdout))
		if !found || changed < best.changed {
			best = derivedBaseline{parent: parent, changed: changed}
			found = true
		}
	}
	if !found || int64(best.changed)*2 > best.parent.TrackedFiles {
		return fullCheckout{}, nil
	}
	return best, nil
}

func baselineGit(ctx context.Context, project *project.Project, tree, index string, args ...string) error {
	command := append([]string{"--git-dir=" + project.Repository.CommonGitDir, "--work-tree=" + tree}, args...)
	_, err := project.Runner.Run(ctx, execx.Command{Dir: project.Repository.Root, Env: map[string]string{"GIT_INDEX_FILE": index}, Name: "git", Args: command})
	return err
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
