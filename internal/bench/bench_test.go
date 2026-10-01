package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRunComparesGitAndPreparedEnvironment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	report, err := Run(context.Background(), Options{
		Scratch:          t.TempDir(),
		Methods:          []string{"git", "git-env-copy", "cambium-auto"},
		WorkspaceCount:   2,
		TrackedFiles:     24,
		TrackedFileBytes: 64,
		EnvironmentFiles: 8,
		EnvironmentBytes: 64,
		Keep:             true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 || len(report.Results) != 3 {
		t.Fatalf("unexpected report: %#v", report)
	}
	gitResult := report.Results[0]
	gitReadyResult := report.Results[1]
	cambiumResult := report.Results[2]
	if !gitResult.Available || !gitReadyResult.Available || !cambiumResult.Available {
		t.Fatalf("required methods unavailable: %#v", report.Results)
	}
	if gitResult.EnvironmentReady != 0 {
		t.Fatalf("plain Git unexpectedly materialized ignored environment: %#v", gitResult)
	}
	if gitReadyResult.EnvironmentReady != 2 {
		t.Fatalf("Git copied-environment control was not ready: %#v", gitReadyResult)
	}
	if gitReadyResult.Semantics != "git-linked-worktree+copied-environment" {
		t.Fatalf("wrong Git control semantics label: %q", gitReadyResult.Semantics)
	}
	if cambiumResult.EnvironmentReady != 2 {
		t.Fatalf("Cambium did not prepare each environment: %#v", cambiumResult)
	}
	if cambiumResult.Semantics != "git-linked-worktree+prepared-environment" {
		t.Fatalf("wrong semantics label: %q", cambiumResult.Semantics)
	}
	for _, result := range report.Results {
		if result.CreateSeconds <= 0 || result.FirstStatusSeconds <= 0 || result.RemoveSeconds <= 0 {
			t.Fatalf("benchmark did not measure full lifecycle: %#v", result)
		}
	}
}

func TestMissingOptionalToolsAreReportedRatherThanFatal(t *testing.T) {
	// Unknown methods are intentionally represented as skipped rows so a fixed
	// benchmark matrix remains usable across heterogeneous CI workers.
	report, err := Run(context.Background(), Options{
		Scratch:          t.TempDir(),
		Methods:          []string{"not-installed"},
		WorkspaceCount:   1,
		TrackedFiles:     1,
		TrackedFileBytes: 1,
		Keep:             true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Available || report.Results[0].SkipReason == "" {
		t.Fatalf("optional method was not reported as skipped: %#v", report.Results)
	}
}

func TestIdentifyToolRejectsUnrelatedSGBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	path := filepath.Join(t.TempDir(), "sg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'Log in to a new group'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := identifyTool(context.Background(), path, []string{"worktree", "--help"}, []string{"simgit", "copy-on-write"}); ok {
		t.Fatal("unrelated sg command was accepted as simgit")
	}
}
