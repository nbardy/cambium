package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nbardy/cambium/internal/native"
)

func (a *App) runSeed(ctx context.Context, args []string) error {
	const usage = "usage: cambium seed warm [--json] [PATH] or cambium seed refresh [--json] [NAME]"
	if len(args) == 0 || (args[0] != "refresh" && args[0] != "warm") {
		return errors.New(usage)
	}
	command := args[0]
	set := a.flagSet("seed " + command)
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() > 1 {
		return errors.New(usage)
	}
	var worktree string
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	if command == "refresh" && set.NArg() == 1 {
		if manager, err = a.managerFor(ctx, set.Arg(0)); err != nil {
			return err
		}
		if worktree, err = manager.Path(set.Arg(0)); err != nil {
			return err
		}
	} else {
		result, err := manager.Project.Repository.RunAt(ctx, a.Cwd, nil, "rev-parse", "--show-toplevel")
		if err != nil {
			return err
		}
		worktree = strings.TrimSpace(result.Stdout)
	}
	var published native.SeedPublication
	switch command {
	case "warm":
		path := set.Arg(0)
		if path != "" {
			absolute := path
			if !filepath.IsAbs(absolute) {
				absolute = filepath.Join(a.Cwd, path)
			}
			relative, err := filepath.Rel(worktree, absolute)
			if err != nil || !filepath.IsLocal(relative) || relative == "." {
				return fmt.Errorf("seed path %q must be within %s", path, worktree)
			}
			path = filepath.ToSlash(relative)
		}
		published, err = manager.Native.WarmDepSeed(ctx, worktree, path)
	case "refresh":
		published, err = native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, worktree)
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, published)
	}
	fmt.Fprintf(a.Stdout, "seed: %s\nrustc: %s\nsnapshot: %s\n", published.Seed, published.Release, published.Snapshot)
	for _, profile := range published.Profiles {
		fmt.Fprintf(a.Stdout, "  %s\ttarget=%s units=%d files=%d dropped_local_units=%d\n", profile.Profile, profile.Target, profile.Units, profile.Files, profile.DroppedLocalUnits)
	}
	return nil
}
