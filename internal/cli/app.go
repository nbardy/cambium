package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nbardy/cambium/internal/bench"
	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/environment"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/state"
	"github.com/nbardy/cambium/internal/workspace"
)

var Version = "0.5.0"

type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Cwd    string
	Runner execx.Runner
}

func New() *App {
	cwd, _ := os.Getwd()
	return &App{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Cwd: cwd, Runner: execx.OSRunner{}}
}

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printUsage()
		return 0
	}
	var err error
	var code int
	switch args[0] {
	case "help", "-h", "--help":
		a.printUsage()
		return 0
	case "version", "--version":
		fmt.Fprintf(a.Stdout, "cambium %s\n", Version)
		return 0
	case "init":
		err = a.runInit(ctx, args[1:])
	case "prepare":
		err = a.runPrepare(ctx, args[1:])
	case "create":
		err = a.runCreate(ctx, args[1:])
	case "add":
		err = a.runAdd(ctx, args[1:])
	case "checkpoint":
		err = a.runCheckpoint(ctx, args[1:])
	case "fork":
		err = a.runFork(ctx, args[1:])
	case "root", "roots":
		err = a.runRoot(ctx, args[1:])
	case "list", "ls":
		err = a.runList(ctx, args[1:])
	case "inspect", "show":
		err = a.runInspect(ctx, args[1:])
	case "path":
		err = a.runPath(ctx, args[1:])
	case "adopt":
		err = a.runAdopt(ctx, args[1:])
	case "reconcile":
		err = a.runReconcile(ctx, args[1:])
	case "env", "environment":
		err = a.runEnvironment(ctx, args[1:])
	case "remove", "rm":
		err = a.runRemove(ctx, args[1:])
	case "run":
		code, err = a.runCommand(ctx, args[1:])
	case "gc":
		err = a.runGC(ctx, args[1:])
	case "recover":
		err = a.runRecover(ctx, args[1:])
	case "prune":
		err = a.runPrune(ctx, args[1:])
	case "doctor":
		code, err = a.runDoctor(ctx, args[1:])
	case "benchmark", "bench":
		err = a.runBenchmark(ctx, args[1:])
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func (a *App) runInit(ctx context.Context, args []string) error {
	set := a.flagSet("init")
	force := set.Bool("force", false, "replace an existing .cambium.json")
	materializerValue := set.String("materializer", string(model.MaterializerAuto), "auto, cow, git, or copy")
	branchPrefix := set.String("branch-prefix", "cambium/", "prefix for generated workspace branches")
	requireCoW := set.Bool("require-cow", false, "refuse non-CoW workspaces")
	preparedIndex := set.Bool("prepared-index", true, "install the prepared baseline index")
	allowPolicyCommands := set.Bool("allow-policy-commands", false, "allow reviewed .cambium.toml prepare/validate commands")
	detectLayers := set.Bool("detect-layers", true, "detect existing ignored dependency environments")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium init [flags]")
	}
	repository, err := gitx.Discover(ctx, a.Cwd, a.runner())
	if err != nil {
		return err
	}
	materializer, err := parseMaterializer(*materializerValue)
	if err != nil {
		return err
	}
	value := config.Default()
	value.Materializer = materializer
	value.BranchPrefix = *branchPrefix
	value.RequireCoW = *requireCoW
	value.PreparedIndex = *preparedIndex
	value.AllowPolicyCommands = *allowPolicyCommands
	var detected []config.LayerRule
	if *detectLayers {
		detected, err = environment.Detect(ctx, repository)
		if err != nil {
			return err
		}
	}
	path, err := config.Write(repository.Root, value, *force)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, path)
	if len(detected) > 0 {
		fmt.Fprintf(a.Stderr, "detected %d active built-in filesystem policy path(s); no per-project rules were required\n", len(detected))
	}
	return nil
}

func (a *App) runPrepare(ctx context.Context, args []string) error {
	set := a.flagSet("prepare")
	ref := set.String("ref", "HEAD", "commit-ish to prepare")
	materializerValue := set.String("materializer", "", "override auto, cow, git, or copy")
	requireCoW := set.Bool("require-cow", false, "require copy-on-write support")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium prepare [flags]")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	materializer := model.Materializer("")
	if *materializerValue != "" {
		materializer, err = parseMaterializer(*materializerValue)
		if err != nil {
			return err
		}
	}
	result, err := manager.Prepare(ctx, *ref, materializer, *requireCoW)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, result)
	}
	fmt.Fprintf(a.Stdout, "commit: %s\nmaterializer: %s\nclone mode: %s\nlayers: %d\n", result.Commit, result.Plan.Resolved, result.Plan.CloneMode, len(result.Layers))
	if result.Baseline != nil {
		fmt.Fprintf(a.Stdout, "baseline: %s\ntracked files: %d\nlogical bytes: %d\n", result.Baseline.Tree, result.Baseline.TrackedFiles, result.Baseline.LogicalBytes)
	}
	return nil
}

func (a *App) runCreate(ctx context.Context, args []string) error {
	set := a.flagSet("create")
	materializerValue := set.String("materializer", "", "override auto, cow, git, or copy")
	ref := set.String("ref", "HEAD", "commit-ish to start from")
	branch := set.String("branch", "", "workspace branch")
	path := set.String("path", "", "workspace path")
	ephemeral := set.Bool("ephemeral", false, "mark the workspace disposable")
	requireCoW := set.Bool("require-cow", false, "require copy-on-write materialization")
	preparedIndex := set.Bool("prepared-index", false, "force the prepared-index path")
	noPreparedIndex := set.Bool("no-prepared-index", false, "force a fresh Git index")
	jsonOutput := set.Bool("json", false, "emit JSON")
	printPath := set.Bool("print-path", false, "print only the workspace path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: cambium create [flags] NAME")
	}
	if *preparedIndex && *noPreparedIndex {
		return errors.New("--prepared-index and --no-prepared-index conflict")
	}
	if *jsonOutput && *printPath {
		return errors.New("--json and --print-path conflict")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	materializer := model.Materializer("")
	if *materializerValue != "" {
		materializer, err = parseMaterializer(*materializerValue)
		if err != nil {
			return err
		}
	}
	var indexOverride *bool
	if *preparedIndex || *noPreparedIndex {
		value := *preparedIndex
		indexOverride = &value
	}
	created, err := manager.Create(ctx, workspace.CreateSpec{
		Name: set.Arg(0), Ref: *ref, Branch: *branch, Path: *path,
		Ephemeral: *ephemeral, RequireCoW: *requireCoW,
		Materializer: materializer, PreparedIndex: indexOverride,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, created)
	}
	if *printPath {
		fmt.Fprintln(a.Stdout, created.Path)
		return nil
	}
	printWorkspace(a.Stdout, created)
	return nil
}

func (a *App) runAdd(ctx context.Context, args []string) error {
	set := a.flagSet("add")
	branch := set.String("b", "", "new workspace branch")
	set.StringVar(branch, "branch", "", "new workspace branch")
	name := set.String("name", "", "Cambium workspace name; defaults to branch or path basename")
	materializerValue := set.String("materializer", "", "override auto, cow, git, or copy")
	ephemeral := set.Bool("ephemeral", false, "mark the workspace disposable")
	requireCoW := set.Bool("require-cow", false, "require copy-on-write materialization")
	jsonOutput := set.Bool("json", false, "emit JSON")
	printPath := set.Bool("print-path", false, "print only the workspace path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() < 1 || set.NArg() > 2 {
		return errors.New("usage: git cambium add [flags] PATH [COMMIT]")
	}
	path := set.Arg(0)
	ref := "HEAD"
	if set.NArg() == 2 {
		ref = set.Arg(1)
	}
	if *branch == "" {
		*branch = filepath.Base(filepath.Clean(path))
	}
	if *name == "" {
		*name = *branch
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	materializer := model.Materializer("")
	if *materializerValue != "" {
		materializer, err = parseMaterializer(*materializerValue)
		if err != nil {
			return err
		}
	}
	created, err := manager.Create(ctx, workspace.CreateSpec{
		Name: *name, Ref: ref, Branch: *branch, Path: path,
		Ephemeral: *ephemeral, RequireCoW: *requireCoW, Materializer: materializer,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, created)
	}
	if *printPath {
		fmt.Fprintln(a.Stdout, created.Path)
		return nil
	}
	printWorkspace(a.Stdout, created)
	return nil
}

func (a *App) runPath(ctx context.Context, args []string) error {
	set := a.flagSet("path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: cambium path NAME")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	path, err := manager.Path(set.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, path)
	return nil
}

func (a *App) runAdopt(ctx context.Context, args []string) error {
	set := a.flagSet("adopt")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 2 {
		return errors.New("usage: cambium adopt [--json] NAME PATH")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	value, err := manager.Adopt(ctx, set.Arg(0), set.Arg(1))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, value)
	}
	printWorkspace(a.Stdout, value)
	return nil
}

func (a *App) runReconcile(ctx context.Context, args []string) error {
	set := a.flagSet("reconcile")
	dryRun := set.Bool("dry-run", false, "report without updating Cambium metadata")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium reconcile [--dry-run] [--json]")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	result, err := manager.Reconcile(ctx, *dryRun)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, result)
	}
	for _, change := range result.Changes {
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%s\n", change.Action, change.Name, change.From, change.To)
	}
	for _, path := range result.Unmanaged {
		fmt.Fprintf(a.Stdout, "unmanaged\t%s\n", path)
	}
	return nil
}

func (a *App) runEnvironment(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cambium env <explain|capture> WORKSPACE")
	}
	set := a.flagSet("env " + args[0])
	jsonOutput := set.Bool("json", false, "emit JSON")
	source := set.String("source", "", "source commit or speculative root for capture")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: cambium env <explain|capture> [flags] WORKSPACE")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	var value workspace.EnvironmentExplanation
	switch args[0] {
	case "explain", "show":
		value, err = manager.ExplainEnvironment(ctx, set.Arg(0))
	case "capture", "publish":
		value, err = manager.CaptureEnvironment(ctx, set.Arg(0), *source)
	default:
		return fmt.Errorf("unknown env command %q", args[0])
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, value)
	}
	fmt.Fprintf(a.Stdout, "workspace: %s\nsource: %s\nready: %t\n", value.Workspace, value.SourceRef, value.Ready)
	for _, item := range value.Paths {
		fmt.Fprintf(a.Stdout, "%s\t%s\tpresent=%t\tvalidated=%t\t%s\n", item.Policy, item.Path, item.Present, item.Validated, item.ID)
	}
	if len(value.Missing) > 0 {
		fmt.Fprintf(a.Stdout, "missing: %s\n", strings.Join(value.Missing, ", "))
	}
	return nil
}

func (a *App) runCheckpoint(ctx context.Context, args []string) error {
	set := a.flagSet("checkpoint")
	refName := set.String("as", "", "speculative ref name; defaults to the workspace name")
	message := set.String("message", "", "checkpoint description")
	parent := set.String("parent", "", "override logical parent root/ref (default: workspace current root)")
	detach := set.Bool("detach", false, "start a new speculative lineage with no logical parent")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: cambium checkpoint [flags] WORKSPACE")
	}
	if *detach && strings.TrimSpace(*parent) != "" {
		return errors.New("--detach and --parent cannot be used together")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	result, err := manager.Checkpoint(ctx, set.Arg(0), workspace.CheckpointOptions{RefName: *refName, Message: *message, Parent: *parent, Detach: *detach})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, result)
	}
	fmt.Fprintf(a.Stdout, "root: %s\nref: %s\nbase: %s\ntree: %s\nchanged paths: %d\nenvironments: %d\nattempts: %d\n", result.Root, result.Ref.Name, result.BaseCommit, result.Tree, len(result.ChangedPaths), len(result.EnvironmentIDs), result.Attempts)
	return nil
}

func (a *App) runFork(ctx context.Context, args []string) error {
	set := a.flagSet("fork")
	materializerValue := set.String("materializer", "", "override auto, cow, git, or copy")
	branch := set.String("branch", "", "workspace branch")
	path := set.String("path", "", "workspace path")
	ephemeral := set.Bool("ephemeral", false, "mark the workspace disposable")
	requireCoW := set.Bool("require-cow", false, "require copy-on-write materialization")
	preparedIndex := set.Bool("prepared-index", false, "force the prepared-index path")
	noPreparedIndex := set.Bool("no-prepared-index", false, "force a fresh Git index")
	jsonOutput := set.Bool("json", false, "emit JSON")
	printPath := set.Bool("print-path", false, "print only the workspace path")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 2 {
		return errors.New("usage: cambium fork [flags] ROOT_OR_REF NAME")
	}
	if *preparedIndex && *noPreparedIndex {
		return errors.New("--prepared-index and --no-prepared-index conflict")
	}
	if *jsonOutput && *printPath {
		return errors.New("--json and --print-path conflict")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	materializer := model.Materializer("")
	if *materializerValue != "" {
		materializer, err = parseMaterializer(*materializerValue)
		if err != nil {
			return err
		}
	}
	var indexOverride *bool
	if *preparedIndex || *noPreparedIndex {
		value := *preparedIndex
		indexOverride = &value
	}
	result, err := manager.Fork(ctx, set.Arg(0), workspace.CreateSpec{
		Name: set.Arg(1), Branch: *branch, Path: *path, Ephemeral: *ephemeral,
		RequireCoW: *requireCoW, Materializer: materializer, PreparedIndex: indexOverride,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, result)
	}
	if *printPath {
		fmt.Fprintln(a.Stdout, result.Workspace.Path)
		return nil
	}
	fmt.Fprintf(a.Stdout, "speculative root: %s\napplied paths: %d\n", result.Root, result.Applied)
	printWorkspace(a.Stdout, result.Workspace)
	return nil
}

func (a *App) runRoot(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cambium root <list|show|diff|drop|gc> ...")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	store, err := manager.SpecStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		set := a.flagSet("root list")
		jsonOutput := set.Bool("json", false, "emit JSON")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("usage: cambium root list [--json]")
		}
		refs, err := store.ListRefs(ctx)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(a.Stdout, refs)
		}
		writer := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "REF\tROOT\tPARENT\tSOURCE\tCREATED\tMESSAGE")
		for _, ref := range refs {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", ref.Name, ref.Root, ref.Parent, ref.SourceWorkspace, ref.CreatedAt.Format(time.RFC3339), ref.Message)
		}
		return writer.Flush()
	case "show":
		set := a.flagSet("root show")
		jsonOutput := set.Bool("json", false, "emit JSON")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 1 {
			return errors.New("usage: cambium root show [--json] ROOT_OR_REF")
		}
		id, root, ref, err := store.Resolve(ctx, set.Arg(0))
		if err != nil {
			return err
		}
		changes, err := store.ChangesFromBase(ctx, root)
		if err != nil {
			return err
		}
		value := map[string]any{"id": id, "root": root, "ref": ref, "changes": changes}
		if *jsonOutput {
			return writeJSON(a.Stdout, value)
		}
		fmt.Fprintf(a.Stdout, "root: %s\nbase: %s\ntree: %s\nchanged paths: %d\n", id, root.BaseCommit, root.Tree, len(changes))
		if ref != nil {
			fmt.Fprintf(a.Stdout, "ref: %s\nparent: %s\nsource: %s\nmessage: %s\n", ref.Name, ref.Parent, ref.SourceWorkspace, ref.Message)
		}
		for _, change := range changes {
			if change.FromPath != "" {
				fmt.Fprintf(a.Stdout, "%s\t%s\t%s\n", change.Status, change.FromPath, change.Path)
			} else {
				fmt.Fprintf(a.Stdout, "%s\t%s\n", change.Status, change.Path)
			}
		}
		return nil
	case "diff":
		set := a.flagSet("root diff")
		jsonOutput := set.Bool("json", false, "emit JSON")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 2 {
			return errors.New("usage: cambium root diff [--json] LEFT RIGHT")
		}
		leftID, _, _, err := store.Resolve(ctx, set.Arg(0))
		if err != nil {
			return err
		}
		rightID, _, _, err := store.Resolve(ctx, set.Arg(1))
		if err != nil {
			return err
		}
		result, err := store.Diff(ctx, leftID, rightID)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(a.Stdout, result)
		}
		if result.BaseChanged {
			fmt.Fprintf(a.Stderr, "warning: base commits differ (%s vs %s); changes compare the complete checkpoint trees\n", result.Left.BaseCommit, result.Right.BaseCommit)
		}
		for _, change := range result.Changes {
			if change.FromPath != "" {
				fmt.Fprintf(a.Stdout, "%s\t%s\t%s\n", change.Status, change.FromPath, change.Path)
			} else {
				fmt.Fprintf(a.Stdout, "%s\t%s\n", change.Status, change.Path)
			}
		}
		return nil
	case "drop":
		set := a.flagSet("root drop")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 1 {
			return errors.New("usage: cambium root drop REF")
		}
		if err := store.DropRef(ctx, set.Arg(0)); err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, set.Arg(0))
		return nil
	case "gc":
		set := a.flagSet("root gc")
		olderThanValue := set.String("older-than", "168h", "minimum age for unreferenced objects")
		dryRun := set.Bool("dry-run", false, "report without deleting")
		jsonOutput := set.Bool("json", false, "emit JSON")
		if err := set.Parse(args[1:]); err != nil {
			return err
		}
		if set.NArg() != 0 {
			return errors.New("usage: cambium root gc [flags]")
		}
		olderThan, err := time.ParseDuration(*olderThanValue)
		if err != nil {
			return fmt.Errorf("parse --older-than: %w", err)
		}
		result, err := manager.RootGC(ctx, olderThan, *dryRun)
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(a.Stdout, result)
		}
		fmt.Fprintf(a.Stdout, "protected: %d\nreleased root refs: %d\nretained root refs: %d\nremoved metadata: %d\nnote: %s\n", len(result.Protected), len(result.ReleasedRoots), result.RetainedRoots, len(result.RemovedMetadata), result.Note)
		return nil
	default:
		return fmt.Errorf("unknown root command %q", args[0])
	}
}

func (a *App) runList(ctx context.Context, args []string) error {
	set := a.flagSet("list")
	refresh := set.Bool("refresh", false, "verify filesystem and Git registration")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium list [flags]")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	values, err := manager.List(ctx, *refresh)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, values)
	}
	writer := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tSTATUS\tMODE\tINDEX\tBRANCH\tPATH")
	for _, value := range values {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%t\t%s\t%s\n", value.Name, value.Status, value.CloneMode, value.PreparedIndex, value.Branch, value.Path)
	}
	return writer.Flush()
}

func (a *App) runInspect(ctx context.Context, args []string) error {
	set := a.flagSet("inspect")
	refresh := set.Bool("refresh", false, "verify filesystem and Git registration")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: cambium inspect [flags] NAME")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	value, err := manager.Load(ctx, set.Arg(0), *refresh)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, value)
	}
	printWorkspace(a.Stdout, value)
	return nil
}

func (a *App) runRemove(ctx context.Context, args []string) error {
	set := a.flagSet("remove")
	force := set.Bool("force", false, "discard uncommitted changes")
	deleteBranch := set.Bool("delete-branch", false, "delete the workspace branch")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return errors.New("usage: cambium remove [flags] NAME")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	if err := manager.Remove(ctx, set.Arg(0), *force, *deleteBranch); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, set.Arg(0))
	return nil
}

func (a *App) runCommand(ctx context.Context, args []string) (int, error) {
	before, command := splitCommand(args)
	set := a.flagSet("run")
	materializerValue := set.String("materializer", "", "materializer for a newly created workspace")
	ref := set.String("ref", "HEAD", "commit-ish for a newly created workspace")
	branch := set.String("branch", "", "branch for a newly created workspace")
	path := set.String("path", "", "path for a newly created workspace")
	requireCoW := set.Bool("require-cow", false, "require CoW for a newly created workspace")
	cleanup := set.Bool("cleanup", false, "remove a clean workspace after success")
	discard := set.Bool("discard-on-exit", false, "force-remove the workspace after success")
	deleteBranch := set.Bool("delete-branch", false, "delete the branch during cleanup")
	if err := set.Parse(before); err != nil {
		return 1, err
	}
	if set.NArg() != 1 || len(command) == 0 {
		return 1, errors.New("usage: cambium run [flags] NAME -- COMMAND [ARGS...]")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return 1, err
	}
	name := set.Arg(0)
	value, err := manager.Load(ctx, name, true)
	if errors.Is(err, state.ErrWorkspaceNotFound) {
		materializer := model.Materializer("")
		if *materializerValue != "" {
			materializer, err = parseMaterializer(*materializerValue)
			if err != nil {
				return 1, err
			}
		}
		value, err = manager.Create(ctx, workspace.CreateSpec{Name: name, Ref: *ref, Branch: *branch, Path: *path, Ephemeral: true, RequireCoW: *requireCoW, Materializer: materializer})
	}
	if err != nil {
		return 1, err
	}
	if value.Status != model.WorkspaceReady {
		return 1, fmt.Errorf("workspace %q is %s", name, value.Status)
	}
	child := exec.CommandContext(ctx, command[0], command[1:]...)
	child.Dir = value.Path
	child.Stdin = a.Stdin
	child.Stdout = a.Stdout
	child.Stderr = a.Stderr
	child.Env = append(os.Environ(), "CAMBIUM_WORKSPACE="+value.Name, "CAMBIUM_WORKSPACE_PATH="+value.Path, "CAMBIUM_REPOSITORY="+value.RepositoryRoot)
	runErr := child.Run()
	_ = manager.Touch(name)
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, runErr
	}
	if *discard {
		if err := manager.Remove(ctx, name, true, *deleteBranch); err != nil {
			return 1, err
		}
	} else if *cleanup {
		if err := manager.Remove(ctx, name, false, *deleteBranch); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

func (a *App) runGC(ctx context.Context, args []string) error {
	set := a.flagSet("gc")
	ephemeral := set.Bool("ephemeral", false, "only select ephemeral workspaces")
	olderThanValue := set.String("older-than", "24h", "minimum idle age")
	force := set.Bool("force", false, "discard dirty workspaces")
	deleteBranches := set.Bool("delete-branches", false, "delete workspace branches")
	dryRun := set.Bool("dry-run", false, "show selection without removing")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium gc [flags]")
	}
	olderThan, err := time.ParseDuration(*olderThanValue)
	if err != nil {
		return fmt.Errorf("parse --older-than: %w", err)
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	result, err := manager.GC(ctx, workspace.GCOptions{EphemeralOnly: *ephemeral, OlderThan: olderThan, Force: *force, DeleteBranches: *deleteBranches, DryRun: *dryRun})
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, result)
	}
	for _, name := range result.Selected {
		fmt.Fprintf(a.Stdout, "selected\t%s\n", name)
	}
	for _, name := range result.Removed {
		fmt.Fprintf(a.Stdout, "removed\t%s\n", name)
	}
	for _, skipped := range result.Skipped {
		fmt.Fprintf(a.Stdout, "skipped\t%s\t%s\n", skipped.Name, skipped.Reason)
	}
	return nil
}

func (a *App) runRecover(ctx context.Context, args []string) error {
	set := a.flagSet("recover")
	dryRun := set.Bool("dry-run", false, "show recovery actions without applying them")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium recover [flags]")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	actions, err := manager.Recover(ctx, *dryRun)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, actions)
	}
	for _, action := range actions {
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%s\n", action.Status, action.Action, action.Name, action.Message)
	}
	if len(actions) == 0 {
		fmt.Fprintln(a.Stdout, "no interrupted operations")
	}
	return nil
}

func (a *App) runPrune(ctx context.Context, args []string) error {
	set := a.flagSet("prune")
	olderThanValue := set.String("older-than", "168h", "minimum cache idle age")
	dryRun := set.Bool("dry-run", false, "show old unreferenced caches without removing them")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium prune [flags]")
	}
	olderThan, err := time.ParseDuration(*olderThanValue)
	if err != nil {
		return fmt.Errorf("parse --older-than: %w", err)
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return err
	}
	result, err := manager.PruneCaches(ctx, olderThan, *dryRun)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSON(a.Stdout, result)
	}
	for _, entry := range result.Selected {
		verb := "selected"
		if !*dryRun {
			verb = "removed"
		}
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%s\n", verb, entry.Kind, entry.ID, entry.Age.Round(time.Second))
	}
	fmt.Fprintf(a.Stdout, "protected: %d\nretained: %d\n", result.Protected, result.Retained)
	return nil
}

func (a *App) runDoctor(ctx context.Context, args []string) (int, error) {
	set := a.flagSet("doctor")
	repair := set.Bool("repair", false, "recover interrupted operations first")
	jsonOutput := set.Bool("json", false, "emit JSON")
	if err := set.Parse(args); err != nil {
		return 1, err
	}
	if set.NArg() != 0 {
		return 1, errors.New("usage: cambium doctor [flags]")
	}
	manager, err := a.manager(ctx)
	if err != nil {
		return 1, err
	}
	var recovery any
	if *repair {
		actions, err := manager.Recover(ctx, false)
		if err != nil {
			return 1, err
		}
		recovery = actions
	}
	checks := manager.Doctor(ctx)
	if *jsonOutput {
		return doctorCode(checks), writeJSON(a.Stdout, map[string]any{"checks": checks, "recovery": recovery})
	}
	writer := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	for _, check := range checks {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", check.Status, check.Name, check.Message)
	}
	if err := writer.Flush(); err != nil {
		return 1, err
	}
	return doctorCode(checks), nil
}

func (a *App) runBenchmark(ctx context.Context, args []string) error {
	defaults := bench.DefaultOptions()
	set := a.flagSet("benchmark")
	methods := set.String("methods", strings.Join(defaults.Methods, ","), "comma-separated methods: git,git-env-copy,cambium-auto,cambium-git,cambium-cow,cambium-copy,simgit,cow")
	count := set.Int("count", defaults.WorkspaceCount, "workspace count")
	files := set.Int("files", defaults.TrackedFiles, "tracked file count")
	fileBytes := set.Int("file-bytes", defaults.TrackedFileBytes, "bytes per tracked file")
	envFiles := set.Int("env-files", defaults.EnvironmentFiles, "ignored environment file count")
	envBytes := set.Int("env-file-bytes", defaults.EnvironmentBytes, "bytes per environment file")
	scratch := set.String("scratch", "", "scratch directory; defaults to a temporary directory")
	keep := set.Bool("keep", false, "keep generated benchmark repositories")
	jsonOutput := set.Bool("json", false, "emit JSON")
	output := set.String("output", "", "write the full JSON report to this file")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: cambium benchmark [flags]")
	}
	options := defaults
	options.Methods = splitComma(*methods)
	options.WorkspaceCount = *count
	options.TrackedFiles = *files
	options.TrackedFileBytes = *fileBytes
	options.EnvironmentFiles = *envFiles
	options.EnvironmentBytes = *envBytes
	options.Scratch = *scratch
	options.Keep = *keep
	report, err := bench.Run(ctx, options)
	if err != nil {
		return err
	}
	bytes, err := bench.EncodeJSON(report)
	if err != nil {
		return err
	}
	if *output != "" {
		absolute, err := filepath.Abs(*output)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(absolute, bytes, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(a.Stderr, "report: %s\n", absolute)
	}
	if *jsonOutput {
		_, err := a.Stdout.Write(bytes)
		return err
	}
	fmt.Fprintf(a.Stdout, "platform: %s\nfilesystem: %s\n", report.Platform, report.Filesystem)
	writer := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "METHOD\tREADY\tPREPARE\tCREATE\tSTATUS1\tSTATUS2\tPHYSICAL\tENV")
	for _, result := range report.Results {
		if !result.Available {
			fmt.Fprintf(writer, "%s\tSKIP\t-\t-\t-\t-\t-\t%s\n", result.Method, result.SkipReason)
			continue
		}
		fmt.Fprintf(writer, "%s\t%d/%d\t%.3fs\t%.3fs\t%.3fs\t%.3fs\t%s\t%s\n", result.Method, result.EnvironmentReady, result.WorkspaceCount, result.PrepareSeconds, result.CreateSeconds, result.FirstStatusSeconds, result.WarmStatusSeconds, humanBytes(result.PhysicalAddedBytes), result.Semantics)
	}
	return writer.Flush()
}

func (a *App) manager(ctx context.Context) (*workspace.Manager, error) {
	projectValue, err := project.Open(ctx, a.Cwd, a.runner())
	if err != nil {
		return nil, err
	}
	return workspace.New(projectValue), nil
}

func (a *App) runner() execx.Runner {
	if a.Runner == nil {
		return execx.OSRunner{}
	}
	return a.Runner
}

func (a *App) flagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(a.Stderr)
	return set
}

func (a *App) printUsage() {
	fmt.Fprint(a.Stdout, `Cambium — native copy-on-write workspaces and hidden Git Merkle checkpoints

Usage:
  cambium init [flags]
  cambium prepare [flags]
  git cambium add [flags] PATH [COMMIT]
  cambium create [flags] NAME
  cambium checkpoint [flags] WORKSPACE
  cambium fork [flags] ROOT_OR_REF NAME
  cambium root <list|show|diff|drop|gc> ...
  cambium run [flags] NAME -- COMMAND [ARGS...]
  cambium list [--refresh] [--json]
  cambium inspect [--refresh] [--json] NAME
  cambium path NAME
  cambium adopt NAME PATH
  cambium reconcile [--dry-run] [--json]
  cambium env <explain|capture> WORKSPACE
  cambium remove [--force] [--delete-branch] NAME
  cambium gc [flags]
  cambium recover [--dry-run] [--json]
  cambium prune [--older-than DURATION] [--dry-run] [--json]
  cambium doctor [--repair] [--json]
  cambium benchmark [flags]
  cambium version

The default materializer uses APFS clone/reflink when available and otherwise
falls back to Git's normal checkout. Cambium is not a mounted filesystem and is
not in the I/O path after workspace creation. Checkpoint commands use a private
Git index and hidden refs; the user's branch and staging area remain unchanged.
`)
}

func parseMaterializer(value string) (model.Materializer, error) {
	materializer := model.Materializer(strings.ToLower(strings.TrimSpace(value)))
	if err := config.ValidateMaterializer(materializer); err != nil {
		return "", err
	}
	return materializer, nil
}

func printWorkspace(writer io.Writer, value model.Workspace) {
	fmt.Fprintf(writer, "name: %s\nstatus: %s\npath: %s\nbranch: %s\nbase: %s\nmode: %s\nprepared index: %t\nenvironment ready: %t\n", value.Name, value.Status, value.Path, value.Branch, value.BaseCommit, value.CloneMode, value.PreparedIndex, value.EnvironmentReady)
	if len(value.EnvironmentMissing) > 0 {
		fmt.Fprintf(writer, "environment missing: %s\n", strings.Join(value.EnvironmentMissing, ", "))
	}
	if value.SpeculativeRoot != "" {
		fmt.Fprintf(writer, "speculative root: %s\n", value.SpeculativeRoot)
	}
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func splitCommand(args []string) ([]string, []string) {
	for index, value := range args {
		if value == "--" {
			return args[:index], args[index+1:]
		}
	}
	return args, nil
}

func splitComma(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func doctorCode(checks []model.DoctorCheck) int {
	for _, check := range checks {
		if check.Status == "error" {
			return 1
		}
	}
	return 0
}

func humanBytes(value int64) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	unit := 0
	for amount >= 1024 && unit < len(units)-1 {
		amount /= 1024
		unit++
	}
	if unit == 0 {
		return sign + strconv.FormatInt(value, 10) + units[unit]
	}
	return fmt.Sprintf("%s%.1f%s", sign, amount, units[unit])
}
