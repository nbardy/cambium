package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nbardy/cambium/internal/model"
)

func TestValidateRejectsEscapingAndOverlappingLayers(t *testing.T) {
	t.Run("escaping", func(t *testing.T) {
		value := Default()
		value.Layers = []LayerRule{{Path: filepath.Join("..", "secret"), Mode: LayerClone}}
		if err := value.Validate(); err == nil {
			t.Fatal("expected escaping path to be rejected")
		}
	})
	t.Run("overlap", func(t *testing.T) {
		value := Default()
		value.Layers = []LayerRule{{Path: "node_modules", Mode: LayerClone}, {Path: "node_modules/pkg", Mode: LayerEmpty}}
		if err := value.Validate(); err == nil {
			t.Fatal("expected overlapping layers to be rejected")
		}
	})
	t.Run("git", func(t *testing.T) {
		value := Default()
		value.Layers = []LayerRule{{Path: ".git/cache", Mode: LayerClone}}
		if err := value.Validate(); err == nil {
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

func TestWriteAndLoad(t *testing.T) {
	root := t.TempDir()
	value := Default()
	value.AllowPolicyCommands = true
	value.Layers = []LayerRule{{Path: "node_modules", Mode: LayerClone, Fingerprint: []string{"package-lock.json"}}}
	if _, err := Write(root, value, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 3 || !loaded.AllowPolicyCommands || len(loaded.Layers) != 1 || loaded.Layers[0].Path != "node_modules" {
		t.Fatalf("unexpected config: %#v", loaded)
	}
	if _, err := Write(root, value, false); err == nil {
		t.Fatal("write without overwrite replaced an existing config")
	}
}

func TestLoadMigratesUsefulV1FieldsInMemory(t *testing.T) {
	root := t.TempDir()
	legacy := map[string]any{
		"version":       1,
		"branch_prefix": "agents/",
		"require_cow":   true,
		"layers": []map[string]any{{
			"path": "node_modules", "mode": "clone", "fingerprint": []string{"package-lock.json"},
		}},
	}
	bytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Filename), bytes, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 3 || loaded.BranchPrefix != "agents/" || !loaded.RequireCoW || len(loaded.Layers) != 1 {
		t.Fatalf("legacy migration lost useful settings: %#v", loaded)
	}
}

func TestLoadPolicyFileAndOverride(t *testing.T) {
	root := t.TempDir()
	content := `version = 1

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

func TestLoadRejectsUnknownOperationalConfigFields(t *testing.T) {
	root := t.TempDir()
	content := `{"version":3,"branch_prefix":"cambium/","materializer":"auto","prepared_index":true,"require_ignored_layers":true,"materalizer":"typo"}`
	if err := os.WriteFile(filepath.Join(root, Filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("unknown operational config field was silently ignored")
	}
}
