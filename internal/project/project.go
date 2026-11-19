package project

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/environment"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/registry"
)

type Project struct {
	Repository gitx.Repository
	Config     config.Config      // defaults < committed .cambium.toml [settings] < local config.toml
	Rules      []config.LayerRule // effective rules for the primary checkout
	StateDir   string
	Runner     execx.Runner
}

func Open(ctx context.Context, path string, runner execx.Runner) (*Project, error) {
	if runner == nil {
		runner = execx.OSRunner{}
	}
	repository, err := gitx.Discover(ctx, path, runner)
	if err != nil {
		return nil, err
	}
	value, err := config.Load(repository.Root, repository.CommonGitDir)
	if err != nil {
		return nil, err
	}
	if value.ScrubEnv {
		if _, plain := runner.(execx.OSRunner); plain {
			// Rediscover through the scrubbing runner so every later Git call is scrubbed too.
			runner = execx.OSRunner{Scrub: true}
			if repository, err = gitx.Discover(ctx, path, runner); err != nil {
				return nil, err
			}
		}
	}
	rules, err := environment.ResolveRules(repository.Root)
	if err != nil {
		return nil, err
	}
	stateDir := filepath.Join(repository.CommonGitDir, "cambium")
	for _, child := range []string{"baselines", "layers", "worktrees", "workspace-meta", "use", "locks", "operations", "benchmarks", "speculative"} {
		if err := os.MkdirAll(filepath.Join(stateDir, child), 0o755); err != nil {
			return nil, err
		}
	}
	project := &Project{Repository: repository, Config: value, Rules: rules, StateDir: stateDir, Runner: runner}
	registry.Register(project.PrimaryRoot())
	return project, nil
}

func (p *Project) BaselinesDir() string { return filepath.Join(p.StateDir, "baselines") }
func (p *Project) LayersDir() string    { return filepath.Join(p.StateDir, "layers") }
func (p *Project) WorktreesDir() string { return filepath.Join(p.StateDir, "worktrees") }
func (p *Project) MetadataDir() string  { return filepath.Join(p.StateDir, "workspace-meta") }
func (p *Project) LocksDir() string     { return filepath.Join(p.StateDir, "locks") }
func (p *Project) OperationsDir() string {
	return filepath.Join(p.StateDir, "operations")
}
func (p *Project) BenchmarksDir() string  { return filepath.Join(p.StateDir, "benchmarks") }
func (p *Project) SpeculativeDir() string { return filepath.Join(p.StateDir, "speculative") }

// PrimaryRoot is the primary checkout, derived from the common Git dir so it is
// the same whether cambium runs in the primary or in a linked worktree.
func (p *Project) PrimaryRoot() string { return filepath.Dir(p.Repository.CommonGitDir) }

// UseDir holds the use markers of one workspace instance (keyed by its ID, so
// a same-name replacement never inherits a predecessor's markers).
func (p *Project) UseDir(workspaceID string) string {
	return filepath.Join(p.StateDir, "use", workspaceID)
}
