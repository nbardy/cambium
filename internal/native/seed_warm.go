package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
)

func (b *Backend) selectDepSeedRule(ctx context.Context, worktree, commit, path string) (config.LayerRule, error) {
	rules, err := b.seedRules(ctx, worktree, commit)
	if err != nil {
		return config.LayerRule{}, err
	}
	for _, rule := range rules {
		if rule.IsDepSeed() && (path == "" || path == rule.Path) {
			return rule, nil
		}
	}
	return config.LayerRule{}, fmt.Errorf("no active cargo-deps seed rule for %q", path)
}

// RefreshDepSeed publishes the reusable units of a workspace's existing build.
func RefreshDepSeed(ctx context.Context, project *project.Project, cloner Cloner, worktree string) (SeedPublication, error) {
	b := New(project)
	b.Cloner = cloner
	commit, err := project.Repository.ResolveCommitAt(ctx, worktree, "HEAD")
	if err != nil {
		return SeedPublication{}, err
	}
	rule, err := b.selectDepSeedRule(ctx, worktree, commit, "")
	if err != nil {
		return SeedPublication{}, err
	}
	if err := validateLayerSafetyAt(ctx, project, worktree, rule); err != nil {
		return SeedPublication{}, err
	}
	seed, _, err := resolveDepSeed(ctx, cloner, filepath.Join(worktree, filepath.FromSlash(rule.Path)))
	if err != nil {
		return SeedPublication{}, err
	}
	return seed.capture(ctx, cloner, false)
}

// WarmDepSeed builds the invoking worktree's committed Cargo graph in a
// private linked worktree. Using the real graph preserves feature resolution,
// build scripts, and profile settings. Capture only publishes sourced units.
func (b *Backend) WarmDepSeed(ctx context.Context, worktree, path string) (publication SeedPublication, err error) {
	commit, err := b.Project.Repository.ResolveCommitAt(ctx, worktree, "HEAD")
	if err != nil {
		return publication, err
	}
	rule, err := b.selectDepSeedRule(ctx, worktree, commit, path)
	if err != nil {
		return publication, err
	}
	// Keep the scratch checkout outside the primary tree so Cargo and rustup
	// cannot pick up uncommitted configuration from its ancestor directories.
	parent, err := os.MkdirTemp("", "cambium-seed-warm-")
	if err != nil {
		return publication, err
	}
	defer os.Remove(parent)
	name := filepath.Base(parent)
	created, err := b.Create(ctx, CreateSpec{
		Name: name, Branch: "cambium/" + name, Ref: commit, Path: filepath.Join(parent, "worktree"),
		Materializer: model.MaterializerGit, Ephemeral: true, sourceOnly: true,
	})
	if err != nil {
		return publication, err
	}
	defer func() {
		if cleanup := b.RemoveCreated(context.WithoutCancel(ctx), created); cleanup != nil {
			err = errors.Join(err, fmt.Errorf("remove warm worktree %s: %w", created.Path, cleanup))
		}
	}()
	workspaceLock, err := b.LockWorkspace(ctx, created.Name)
	if err != nil {
		return publication, err
	}
	defer workspaceLock.Release()
	if err := validateLayerSafetyAt(ctx, b.Project, created.Path, rule); err != nil {
		return publication, err
	}
	target := filepath.Join(created.Path, filepath.FromSlash(rule.Path))
	seed, _, err := resolveDepSeed(ctx, b.Cloner, target)
	if err != nil {
		return publication, err
	}
	// Repeated warms can reuse compatible dependency units. The scratch
	// worktree still builds the committed graph and publishes fresh records.
	if _, err := applyDepSeed(ctx, b.Cloner, created.Path, rule, true); err != nil {
		return publication, err
	}
	if err = seed.build(ctx, b.Cloner.runner()); err != nil {
		if lock, lockErr := seed.lock(ctx); lockErr == nil {
			seed.failure(err)
			_ = lock.Release()
		}
		return publication, err
	}
	return seed.capture(ctx, b.Cloner, false)
}

func (s depSeed) build(ctx context.Context, runner execx.Runner) error {
	dir := filepath.Dir(s.target)
	cargo := "cargo"
	rustup, err := pinnedRustup(runner, dir)
	if err != nil {
		return err
	}
	if rustup != "" {
		result, err := runner.Run(ctx, execx.Command{Dir: dir, Name: rustup, Args: []string{"which", "cargo"}})
		if err != nil {
			return err
		}
		cargo = strings.TrimSpace(result.Stdout)
	}
	env := map[string]string{"RUSTC": s.toolchain.Rustc, "CARGO_TARGET_DIR": s.target, "CARGO_BUILD_BUILD_DIR": s.target}
	// Cargo check and build use distinct units for normal libraries.
	for _, command := range []string{"check", "build"} {
		if _, err := runner.Run(ctx, execx.Command{Dir: dir, Env: env, Name: cargo, Args: []string{command, "--locked", "--workspace"}}); err != nil {
			return err
		}
	}
	return nil
}
