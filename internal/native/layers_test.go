package native

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/project"
)

func TestReceiptReusesEnvironmentAcrossCommitsWhenInputsMatch(t *testing.T) {
	rule := config.LayerRule{Path: "node_modules", Mode: config.LayerClone, Inputs: []string{"package-lock.json"}}
	inputs := []ReceiptInput{{Path: "package-lock.json", Mode: "100644", Object: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	first := fingerprintRule("commit-a", rule, inputs)
	second := fingerprintRule("commit-b", rule, inputs)
	if first != second {
		t.Fatalf("unrelated Git commit invalidated the same input-defined environment: %s != %s", first, second)
	}
	changed := []ReceiptInput{{Path: "package-lock.json", Mode: "100644", Object: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
	third := fingerprintRule("commit-b", rule, changed)
	if first == third {
		t.Fatal("lockfile object change did not invalidate environment receipt")
	}
}

func TestSourceSensitiveReceiptChangesAcrossCommits(t *testing.T) {
	rule := config.LayerRule{Path: "target", Mode: config.LayerSeed, Inputs: []string{"Cargo.lock"}, SourceSensitive: true}
	inputs := []ReceiptInput{{Path: "Cargo.lock", Mode: "100644", Object: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	first := fingerprintRule("commit-a", rule, inputs)
	second := fingerprintRule("commit-b", rule, inputs)
	if first == second {
		t.Fatal("source-sensitive build seed did not change across source commits")
	}
}

func TestNoInputsFallsBackToSourceIdentity(t *testing.T) {
	rule := config.LayerRule{Path: "custom", Mode: config.LayerClone}
	if fingerprintRule("commit-a", rule, nil) == fingerprintRule("commit-b", rule, nil) {
		t.Fatal("input-less rule must be keyed by source identity")
	}
}

func TestReceiptIncludesPolicyIdentityNotOnlyCurrentlyMatchedInputs(t *testing.T) {
	inputs := []ReceiptInput{{Path: "package-lock.json", Mode: "100644", Object: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	base := config.LayerRule{Path: "node_modules", Mode: config.LayerClone, Inputs: []string{"package-lock.json"}}
	changedPattern := base
	changedPattern.Inputs = []string{"package-lock.json", ".npmrc"}
	if fingerprintRule("commit", base, inputs) == fingerprintRule("commit", changedPattern, inputs) {
		t.Fatal("adding an unmatched policy input did not change receipt identity")
	}
	changedRequirement := base
	changedRequirement.Required = true
	if fingerprintRule("commit", base, inputs) == fingerprintRule("commit", changedRequirement, inputs) {
		t.Fatal("changing required policy did not change receipt identity")
	}
}

// A dep-seed layer captured from a workspace was once written as a
// metadata-only receipt with present=true and no payload, which loadLayer
// rejects; every later publish of the id then failed with "file exists".
func TestMetadataLayerNeverClaimsUnbackedPayloadAndBrokenLayerIsReplaced(t *testing.T) {
	state := t.TempDir()
	p := &project.Project{StateDir: state}
	for _, d := range []string{p.LayersDir(), p.LocksDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rule := config.LayerRule{Path: "target", Mode: config.LayerSeed, Seed: "cargo-deps"}
	layer := LayerSnapshot{Version: layerVersion, ID: "abc", Rule: rule, Present: true, CreatedAt: time.Now().UTC()}
	ctx := context.Background()

	// Prevention: present=true is dropped when there is no payload.
	got, err := publishMetadataLayer(ctx, p, layer)
	if err != nil || got.Present {
		t.Fatalf("metadata-only publish: present=%v err=%v", got.Present, err)
	}
	if loaded, ok := loadLayer(filepath.Join(p.LayersDir(), "abc")); !ok || loaded.Present {
		t.Fatalf("published layer not loadable as absent: ok=%v", ok)
	}

	// Recovery: a pre-existing broken directory is moved aside.
	broken := filepath.Join(p.LayersDir(), "def")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"metadata.json": `{"version":` + itoa(layerVersion) + `,"id":"def","rule":{"path":"target","mode":"seed","seed":"cargo-deps"},"present":true}`,
		"ready":         "def\n",
	} {
		if err := os.WriteFile(filepath.Join(broken, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := loadLayer(broken); ok {
		t.Fatal("fixture should be rejected by loadLayer")
	}
	layer.ID = "def"
	if _, err := publishMetadataLayer(ctx, p, layer); err != nil {
		t.Fatalf("publish over broken layer: %v", err)
	}
	if _, ok := loadLayer(broken); !ok {
		t.Fatal("layer not loadable after recovery")
	}
	aside, _ := filepath.Glob(filepath.Join(p.LayersDir(), ".layer-def-incomplete-*"))
	if len(aside) != 1 {
		t.Fatalf("broken directory not preserved aside: %v", aside)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
