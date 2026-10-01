package native

import (
	"testing"

	"github.com/nbardy/cambium/internal/config"
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
