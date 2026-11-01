package cli

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/native"
)

func TestParseCacheByteLimit(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"0", 0}, {"0GiB", 0}, {"42", 42}, {"42B", 42},
		{"2KB", 2000}, {"2MB", 2000000}, {"2GB", 2000000000}, {"2TB", 2000000000000},
		{"2KiB", 2 << 10}, {"2MiB", 2 << 20}, {"2GiB", 2 << 30}, {"2TiB", 2 << 40},
		{" 3 mib ", 3 << 20}, {"9223372036854775807", math.MaxInt64},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, err := parseCacheByteLimit(test.value)
			if err != nil || got != test.want {
				t.Fatalf("parse %q = %d, %v; want %d", test.value, got, err, test.want)
			}
		})
	}
	for _, value := range []string{"", " ", "-1", "+1", "1.5GiB", "NaN", "1e3", "1_000", "1,000", "GB", "1Ki", "1PB", "1MBjunk", "9223372036854775808", "8388608TiB", "18446744073709551616"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			if got, err := parseCacheByteLimit(value); err == nil {
				t.Fatalf("invalid size %q accepted as %d", value, got)
			}
		})
	}
}

func TestCLIPruneRejectsInvalidBudgetBeforeOpeningRepository(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"empty", []string{"--max-unused-bytes="}, "parse --max-unused-bytes"},
		{"negative", []string{"--max-unused-bytes=-1"}, "parse --max-unused-bytes"},
		{"fractional", []string{"--max-unused-bytes=1.5GiB"}, "parse --max-unused-bytes"},
		{"overflow", []string{"--max-unused-bytes=8388608TiB"}, "parse --max-unused-bytes"},
		{"age_and_budget", []string{"--older-than=168h", "--max-unused-bytes=0"}, "mutually exclusive"},
		{"budget_and_age", []string{"--max-unused-bytes=1MiB", "--older-than=168h"}, "mutually exclusive"},
		{"zero_age", []string{"--older-than=0s"}, "--older-than must be positive"},
		{"unexpected_argument", []string{"--max-unused-bytes=0", "extra"}, "usage: cambium prune"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, stderr := testApp(t.TempDir())
			if code := app.Run(context.Background(), append([]string{"prune"}, test.args...)); code == 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("invalid prune: code=%d stdout=%q stderr=%q; want %q", code, stdout.String(), stderr.String(), test.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("invalid prune produced stdout: %q", stdout.String())
			}
		})
	}
}

func TestCLIPruneZeroBudgetPreservesLiveBaselineAndHonorsDryRun(t *testing.T) {
	requireCLIGit(t)
	root := initializeCLIRepository(t)
	app, stdout, stderr := testApp(root)
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		resetBuffers(stdout, stderr)
		if code := app.Run(ctx, args); code != 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	run("init", "--detect-layers=false")
	run("create", "--materializer=copy", "old")
	if err := os.WriteFile(filepath.Join(root, "src", "a.txt"), []byte("second revision\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, root, "add", "src/a.txt")
	gitCLI(t, root, "commit", "-q", "-m", "second")
	run("create", "--materializer=copy", "live")
	run("remove", "--force", "--delete-branch", "old")

	// The default age policy keeps the recent unused entry. A budget must not
	// silently inherit that default when --older-than was not supplied.
	run("prune", "--json")
	var age native.CachePruneResult
	if err := json.Unmarshal(stdout.Bytes(), &age); err != nil {
		t.Fatal(err)
	}
	if len(age.Removed) != 0 || age.Protected != 1 || age.Retained != 1 {
		t.Fatalf("unexpected default age result: %#v", age)
	}

	run("prune", "--max-unused-bytes=0GiB", "--dry-run", "--json")
	var plan native.CachePruneResult
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.MaxUnusedBytes == nil || *plan.MaxUnusedBytes != 0 || plan.ByteAccounting != "logical_regular_file_bytes" {
		t.Fatalf("budget/accounting not recorded: %#v", plan)
	}
	if plan.Protected != 1 || plan.ProtectedBytes <= 0 || len(plan.Selected) != 1 || plan.EligibleBytes <= 0 || plan.SelectedBytes != plan.EligibleBytes || plan.RetainedBytes != 0 || plan.RemovedBytes != 0 || len(plan.Removed) != 0 {
		t.Fatalf("unexpected zero-budget plan: %#v", plan)
	}
	if _, err := os.Stat(plan.Selected[0].Path); err != nil {
		t.Fatalf("dry run removed selected cache: %v", err)
	}

	run("prune", "--max-unused-bytes=0")
	if !strings.Contains(stdout.String(), "unused cache budget: 0 logical bytes") || !strings.Contains(stdout.String(), "logical bytes: protected=") {
		t.Fatalf("text output does not explain byte accounting: %q", stdout.String())
	}
	if _, err := os.Stat(plan.Selected[0].Path); !os.IsNotExist(err) {
		t.Fatalf("unused baseline survived zero-budget pruning: %v", err)
	}
	run("prune", "--max-unused-bytes=0", "--json")
	var remaining native.CachePruneResult
	if err := json.Unmarshal(stdout.Bytes(), &remaining); err != nil {
		t.Fatal(err)
	}
	if remaining.Protected != 1 || remaining.EligibleBytes != 0 || remaining.RemovedBytes != 0 || len(remaining.Removed) != 0 {
		t.Fatalf("live baseline was not preserved: %#v", remaining)
	}
	run("inspect", "--refresh", "live")
	run("remove", "--force", "--delete-branch", "live")
}
