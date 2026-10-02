package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbardy/cambium/internal/model"
)

func TestValidateRejectsEscapingAndOverlappingLayers(t *testing.T) {
	t.Run("escaping", func(t *testing.T) {
		if err := ValidateRules([]LayerRule{{Path: filepath.Join("..", "secret"), Mode: LayerClone}}); err == nil {
			t.Fatal("expected escaping path to be rejected")
		}
	})
	t.Run("overlap", func(t *testing.T) {
		if err := ValidateRules([]LayerRule{{Path: "node_modules", Mode: LayerClone}, {Path: "node_modules/pkg", Mode: LayerEmpty}}); err == nil {
			t.Fatal("expected overlapping layers to be rejected")
		}
	})
	t.Run("git", func(t *testing.T) {
		if err := ValidateRules([]LayerRule{{Path: ".git/cache", Mode: LayerClone}}); err == nil {
			t.Fatal("expected .git layer to be rejected")
		}
	})
}

func TestValidateRejectsUnknownMaterializer(t *testing.T) {
	value := Default()
	value.Materializer = model.Materializer("magic")
	if err := value.Validate(); err == nil {
		t.Fatal("expected invalid materializer to be rejected")
	}
}

// Local settings must override only the keys they set. If init wrote every
// default into the local file, a committed [settings] value (e.g. a team
// branch_prefix) would be silently shadowed in every clone.
func TestLocalSettingsOverrideOnlyKeysTheySet(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	committed := "version = 1\n\n[settings]\nbranch_prefix = \"team/\"\nrequire_cow = true\n"
	if err := os.WriteFile(filepath.Join(root, PolicyFilename), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteLocal(gitDir, []Setting{BoolSetting("require_cow", false), BoolSetting("allow_policy_commands", true)}, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BranchPrefix != "team/" || loaded.RequireCoW || !loaded.AllowPolicyCommands {
		t.Fatalf("layering wrong: %#v", loaded)
	}
	if _, err := WriteLocal(gitDir, nil, false); err == nil {
		t.Fatal("WriteLocal replaced an existing local config without overwrite")
	}
}

// The committed file is controlled by whatever branch is checked out. It must
// never be able to turn on command execution, and the local file must not be
// able to smuggle in path rules that differ from the commit.
func TestTrustBoundaryBetweenCommittedAndLocal(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	enable := "version = 1\n\n[settings]\nallow_policy_commands = true\n"
	if err := os.WriteFile(filepath.Join(root, PolicyFilename), []byte(enable), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, gitDir); err == nil || !strings.Contains(err.Error(), "allow_policy_commands") {
		t.Fatalf("committed allow_policy_commands was not rejected: %v", err)
	}
	if _, err := ParsePolicy("target", strings.NewReader(enable)); err == nil {
		t.Fatal("target-tree policy enabling commands was not rejected")
	}

	localRules := "version = 1\n\n[[path]]\npath = \"node_modules\"\npolicy = \"share\"\n"
	if _, err := Parse("local", strings.NewReader(localRules), ScopeLocal); err == nil {
		t.Fatal("local config accepted [[path]] rules")
	}
}

func TestLoadPolicyFileAndOverride(t *testing.T) {
	root := t.TempDir()
	content := `version = 1

[settings]
branch_prefix = "agents/"

[[path]]
path = "node_modules"
policy = "skip"
inputs = ["package.json", "yarn.lock"]
priority = 20

[[path]]
path = ".company-env"
policy = "recreate"
inputs = ["company.lock"]
prepare = ["company-pm", "sync", "--locked"]
validate = ["company-pm", "check"]
required = true
`
	if err := os.WriteFile(filepath.Join(root, PolicyFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadPolicyFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Mode != LayerSkip || rules[1].Prepare[0] != "company-pm" || !rules[1].Required {
		t.Fatalf("unexpected parsed rules: %#v", rules)
	}
	merged, err := MergeRules([]LayerRule{{Path: "node_modules", Mode: LayerClone, Builtin: true}}, rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range merged {
		if rule.Path == "node_modules" && rule.Mode != LayerSkip {
			t.Fatalf("override did not win: %#v", merged)
		}
	}
}

func TestPolicyFileRejectsUnknownKeysAndEscapes(t *testing.T) {
	for name, content := range map[string]string{
		"unknown": "version = 1\n[[path]]\npath = \"x\"\npolicy = \"clone\"\nmagic = true\n",
		"escape":  "version = 1\n[[path]]\npath = \"../x\"\npolicy = \"clone\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, PolicyFilename), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPolicyFile(root); err == nil {
				t.Fatal("expected invalid policy file to fail")
			}
		})
	}
}

func TestPolicyFileSupportsMultilineArraysAndRejectsDuplicateKeys(t *testing.T) {
	root := t.TempDir()
	content := `version = 1

[[path]]
path = ".company-env"
policy = "recreate"
inputs = [
  "company.lock",
  "company.toml", # comments are allowed
]
prepare = [
  "company-pm",
  "sync",
  "--locked",
]
`
	if err := os.WriteFile(filepath.Join(root, PolicyFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadPolicyFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || len(rules[0].Inputs) != 2 || len(rules[0].Prepare) != 3 {
		t.Fatalf("multiline policy parsed incorrectly: %#v", rules)
	}

	for name, invalid := range map[string]string{
		"missing-version": "[[path]]\npath=\"x\"\npolicy=\"skip\"\n",
		"duplicate":       "version=1\n[[path]]\npath=\"x\"\npattern=\"y\"\npolicy=\"skip\"\n",
		"unterminated":    "version=1\n[[path]]\npath=\"x\"\npolicy=\"clone\"\ninputs=[\"a\",\n",
	} {
		t.Run(name, func(t *testing.T) {
			caseRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(caseRoot, PolicyFilename), []byte(invalid), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadPolicyFile(caseRoot); err == nil {
				t.Fatal("expected invalid policy to fail")
			}
		})
	}
}

func TestLoadRejectsUnknownSettings(t *testing.T) {
	root := t.TempDir()
	content := "version = 1\n\n[settings]\nmateralizer = \"git\"\n"
	if err := os.WriteFile(filepath.Join(root, PolicyFilename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, filepath.Join(root, ".git")); err == nil {
		t.Fatal("misspelled setting was silently ignored")
	}
}
