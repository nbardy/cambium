package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/model"
)

func TestCloneTreeCommandCopiesContentsRatherThanNestingSourceDirectory(t *testing.T) {
	source := filepath.Join(string(os.PathSeparator), "tmp", "baseline", "tree")
	destination := filepath.Join(string(os.PathSeparator), "tmp", "workspace")
	_, args := cloneTreeCommand(source, destination, false)
	if len(args) < 2 {
		t.Fatalf("unexpected copy arguments: %#v", args)
	}
	from := args[len(args)-2]
	if !strings.HasSuffix(from, string(os.PathSeparator)+".") {
		t.Fatalf("source %q would nest the baseline directory instead of copying its contents", from)
	}
}

func TestAutoFallsBackToGitRatherThanRecursiveCopy(t *testing.T) {
	plan, err := planMaterialization(model.MaterializerAuto, CloneCapability{Supported: false, Detail: "no reflink"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Resolved != model.MaterializerGit || plan.UseBaseline || plan.CloneMode != model.CloneModeGitCheckout {
		t.Fatalf("unexpected fallback plan: %#v", plan)
	}
}

func TestAutoUsesCowWhenAvailable(t *testing.T) {
	capability := CloneCapability{Supported: true, Mode: model.CloneModeAPFS, Detail: "ok"}
	plan, err := planMaterialization(model.MaterializerAuto, capability, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Resolved != model.MaterializerCoW || !plan.UseBaseline || plan.CloneMode != model.CloneModeAPFS {
		t.Fatalf("unexpected CoW plan: %#v", plan)
	}
}

func TestRequireCowRefusesUnsupportedFilesystem(t *testing.T) {
	if _, err := planMaterialization(model.MaterializerAuto, CloneCapability{Supported: false, Detail: "no reflink"}, true); err == nil {
		t.Fatal("expected strict CoW plan to fail")
	}
}

func TestProbeBetweenRejectsMissingSourceDirectory(t *testing.T) {
	destination := t.TempDir()
	capability := (Cloner{}).ProbeBetween(context.Background(), filepath.Join(destination, "missing"), destination)
	if capability.Supported {
		t.Fatal("missing source unexpectedly supported CoW")
	}
	if !strings.Contains(capability.Detail, "inspect clone source") {
		t.Fatalf("unexpected detail: %s", capability.Detail)
	}
}
