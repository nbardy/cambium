package cli

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/workspace"
)

// runStatus answers, for every workspace: is it dirty, unpushed, merged, in
// use, and how many bytes would deleting it actually return.
func (a *App) runStatus(ctx context.Context, args []string) error {
	set := a.flagSet("status")
	all := set.Bool("all", false, "every repository under repos_root that has Cambium state, not only this one")
	mergedInto := set.String("merged-into", "", "ref the merged column is judged against (default: origin's default branch)")
	noBytes := set.Bool("no-bytes", false, "skip measuring exclusive bytes (the slow part)")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium status [--all] [--merged-into REF] [--no-bytes] [--json]")
	}
	var managers []*workspace.Manager
	if *all {
		var err error
		if managers, err = a.allRepositories(ctx); err != nil {
			return err
		}
	} else {
		manager, err := a.manager(ctx)
		if err != nil {
			return err
		}
		managers = []*workspace.Manager{manager}
	}
	var rows []workspace.Status
	for _, manager := range managers {
		statuses, err := manager.Statuses(ctx, *mergedInto, !*noBytes)
		if err != nil {
			if *all {
				fmt.Fprintf(a.Stderr, "warning: %s: %v\n", manager.Project.PrimaryRoot(), err)
				continue
			}
			return err
		}
		rows = append(rows, statuses...)
	}
	if *jsonOutput {
		if rows == nil {
			rows = []workspace.Status{}
		}
		return writeJSON(a.Stdout, rows)
	}
	table := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tREPO\tDIRTY\tUNPUSHED\tMERGED\tIN-USE\tEXCLUSIVE\tPATH")
	for _, row := range rows {
		state := yesNo(len(row.InUse) > 0)
		if row.Missing {
			state = "missing"
		}
		exclusive := "-"
		if row.Size != nil {
			exclusive = humanBytes(row.Size.Exclusive)
			if !row.Size.Exact {
				exclusive = "<=" + exclusive
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n", row.Name, repoLabel(row.Repository), row.Dirty, row.Unpushed, yesNo(row.Merged), state, exclusive, row.Path)
		for _, problem := range row.Errors {
			fmt.Fprintf(a.Stderr, "warning: %s: %s\n", row.Name, problem)
		}
	}
	return table.Flush()
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func repoLabel(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// MaintainReport is one repository's maintenance outcome.
type MaintainReport struct {
	Repository string                       `json:"repository"`
	Reconcile  workspace.ReconcileResult    `json:"reconcile"`
	Forget     workspace.ForgetResult       `json:"forget"`
	Caches     native.CachePruneResult      `json:"caches"`
	Merged     *workspace.PruneMergedResult `json:"merged,omitempty"`
	Error      string                       `json:"error,omitempty"`
}

// runMaintain is the one-command housekeeping pass that wrapper scripts used
// to loop over: reconcile, forget records whose worktree is gone, prune idle
// caches, and (only when --merged names a ref) remove merged workspaces. Every
// removal goes through the guarded path, so dirty, unpushed-unmerged and
// in-use workspaces are kept.
func (a *App) runMaintain(ctx context.Context, args []string) (int, error) {
	set := a.flagSet("maintain")
	all := set.Bool("all-repos", false, "every repository under repos_root that has Cambium state")
	merged := set.String("merged", "", "also remove workspaces whose HEAD is already in REF")
	deleteBranch := set.Bool("delete-branch", false, "with --merged: delete the merged branches too")
	olderThanValue := set.String("older-than", "168h", "minimum cache idle age; with --merged also the idle age for workspaces with no commits")
	dryRun := set.Bool("dry-run", false, "report without changing anything")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return 1, err
	}
	if set.NArg() != 0 {
		return 1, errors.New("usage: cambium maintain [--all-repos] [--merged REF [--delete-branch]] [--older-than DURATION] [--dry-run] [--json]")
	}
	olderThan, err := time.ParseDuration(*olderThanValue)
	if err != nil || olderThan <= 0 {
		return 1, fmt.Errorf("parse --older-than: want a positive duration, got %q", *olderThanValue)
	}
	var managers []*workspace.Manager
	if *all {
		if managers, err = a.allRepositories(ctx); err != nil {
			return 1, err
		}
	} else {
		manager, err := a.manager(ctx)
		if err != nil {
			return 1, err
		}
		managers = []*workspace.Manager{manager}
	}
	failed := false
	var reports []MaintainReport
	for _, manager := range managers {
		report := MaintainReport{Repository: manager.Project.PrimaryRoot()}
		if err := maintainOne(ctx, manager, &report, *merged, *deleteBranch, olderThan, *dryRun); err != nil {
			report.Error = err.Error()
			failed = true
		}
		reports = append(reports, report)
	}
	code := 0
	if failed {
		code = 1
	}
	if *jsonOutput {
		return code, writeJSON(a.Stdout, reports)
	}
	for _, report := range reports {
		fmt.Fprintf(a.Stdout, "%s\n", report.Repository)
		for _, change := range report.Reconcile.Changes {
			fmt.Fprintf(a.Stdout, "  reconcile\t%s\t%s\n", change.Action, change.Name)
		}
		for _, change := range report.Forget.Forgotten {
			fmt.Fprintf(a.Stdout, "  forget\t%s\n", change.Name)
		}
		fmt.Fprintf(a.Stdout, "  caches\tselected=%d removed_bytes=%d\n", len(report.Caches.Selected), report.Caches.RemovedBytes)
		if report.Merged != nil {
			for _, name := range report.Merged.Selected {
				fmt.Fprintf(a.Stdout, "  merged\t%s\n", name)
			}
			for _, skip := range report.Merged.Skipped {
				fmt.Fprintf(a.Stdout, "  kept\t%s\t%s\n", skip.Name, firstLine(skip.Reason))
			}
		}
		if report.Error != "" {
			fmt.Fprintf(a.Stdout, "  error\t%s\n", report.Error)
		}
	}
	return code, nil
}

func maintainOne(ctx context.Context, manager *workspace.Manager, report *MaintainReport, mergedRef string, deleteBranch bool, olderThan time.Duration, dryRun bool) error {
	var err error
	if report.Reconcile, err = manager.Reconcile(ctx, dryRun); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	if report.Forget, err = manager.ForgetMissing(ctx, dryRun); err != nil {
		return fmt.Errorf("forget --missing: %w", err)
	}
	if mergedRef != "" {
		result, err := manager.PruneMerged(ctx, workspace.PruneMergedOptions{Ref: mergedRef, DryRun: dryRun, DeleteBranch: deleteBranch, MinIdle: olderThan})
		if err != nil {
			return fmt.Errorf("prune --merged: %w", err)
		}
		report.Merged = &result
	}
	if report.Caches, err = manager.PruneCachesWithOptions(ctx, workspace.CachePruneOptions{DryRun: dryRun, OlderThan: olderThan}); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}

func firstLine(text string) string {
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			return text[:i]
		}
	}
	return text
}
