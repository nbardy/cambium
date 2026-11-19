package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nbardy/cambium/internal/native"
)

// runSeed implements `cambium seed refresh [NAME]`: publish the dependency
// units of a built workspace as the seed for its compiler release.
func (a *App) runSeed(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "refresh" {
		return errors.New("usage: cambium seed refresh [--json] [NAME]")
	}
	set := a.flagSet("seed refresh")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() > 1 {
		return errors.New("usage: cambium seed refresh [--json] [NAME]")
	}
	var (
		path string
		err  error
	)
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	if set.NArg() == 1 {
		if manager, err = a.managerFor(ctx, set.Arg(0)); err != nil {
			return err
		}
		if path, err = manager.Path(set.Arg(0)); err != nil {
			return err
		}
	} else {
		result, err := manager.Project.Repository.RunAt(ctx, a.Cwd, nil, "rev-parse", "--show-toplevel")
		if err != nil {
			return err
		}
		path = strings.TrimSpace(result.Stdout)
	}
	published, err := native.RefreshDepSeed(ctx, manager.Project, manager.Native.Cloner, path)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, published)
	}
	fmt.Fprintf(a.Stdout, "seed: %s\nrustc: %s\nsnapshot: %s\n", published.Seed, published.Release, published.Snapshot)
	for _, profile := range published.Profiles {
		fmt.Fprintf(a.Stdout, "  %s\tunits=%d files=%d dropped_local_units=%d\n", profile.Profile, profile.Units, profile.Files, profile.DroppedLocalUnits)
	}
	return nil
}
