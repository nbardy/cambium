package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/model"
)

const (
	Filename       = ".cambium.json"
	PolicyFilename = ".cambium.toml"
)

type LayerMode string

const (
	LayerClone    LayerMode = "clone"
	LayerSeed     LayerMode = "seed"
	LayerRecreate LayerMode = "recreate"
	LayerShare    LayerMode = "share"
	LayerEmpty    LayerMode = "empty"
	LayerSkip     LayerMode = "skip"
)

// LayerRule is a generic filesystem policy. Cambium deliberately does not
// implement package managers: inputs identify compatible source states, Path
// identifies workspace-local output, and Mode determines how the output is
// materialized. Prepare/Validate are optional argv arrays executed directly,
// never through a shell.
type LayerRule struct {
	Name             string    `json:"name,omitempty"`
	Path             string    `json:"path"`
	Mode             LayerMode `json:"mode"`
	Inputs           []string  `json:"inputs,omitempty"`
	Fingerprint      []string  `json:"fingerprint,omitempty"` // legacy alias for Inputs
	Prepare          []string  `json:"prepare,omitempty"`
	Validate         []string  `json:"validate,omitempty"`
	AllowUnignored   bool      `json:"allow_unignored,omitempty"`
	SourceSensitive  bool      `json:"source_sensitive,omitempty"`
	Required         bool      `json:"required,omitempty"`
	ActivateOnInputs bool      `json:"activate_on_inputs,omitempty"`
	Priority         int       `json:"priority,omitempty"`
	Builtin          bool      `json:"-"`
	Origin           string    `json:"-"`
}

func (r LayerRule) EffectiveInputs() []string {
	values := append([]string(nil), r.Inputs...)
	values = append(values, r.Fingerprint...)
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(value))))
		if value == "." || value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

type Config struct {
	Version              int                `json:"version"`
	BranchPrefix         string             `json:"branch_prefix"`
	Materializer         model.Materializer `json:"materializer"`
	RequireCoW           bool               `json:"require_cow"`
	PreparedIndex        bool               `json:"prepared_index"`
	RequireIgnoredLayers bool               `json:"require_ignored_layers"`
	AllowPolicyCommands  bool               `json:"allow_policy_commands"`
	Layers               []LayerRule        `json:"layers,omitempty"` // legacy/JSON overrides
}

func Default() Config {
	return Config{
		Version:              3,
		BranchPrefix:         "cambium/",
		Materializer:         model.MaterializerAuto,
		PreparedIndex:        true,
		RequireIgnoredLayers: true,
		Layers:               []LayerRule{},
	}
}

func Load(repoRoot string) (Config, error) {
	path := filepath.Join(repoRoot, Filename)
	bytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(bytes, &header); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	value := Default()
	switch header.Version {
	case 1:
		var legacy struct {
			Version      int         `json:"version"`
			BranchPrefix string      `json:"branch_prefix"`
			RequireCoW   bool        `json:"require_cow"`
			Layers       []LayerRule `json:"layers"`
		}
		if err := decodeStrictJSON(bytes, &legacy); err != nil {
			return Config{}, fmt.Errorf("parse legacy %s: %w", path, err)
		}
		if legacy.BranchPrefix != "" {
			value.BranchPrefix = legacy.BranchPrefix
		}
		value.RequireCoW = legacy.RequireCoW
		value.Layers = legacy.Layers
	case 2, 3:
		if err := decodeStrictJSON(bytes, &value); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
		value.Version = 3
	default:
		return Config{}, fmt.Errorf("unsupported config version %d", header.Version)
	}
	if err := value.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return value, nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func Write(repoRoot string, value Config, overwrite bool) (string, error) {
	value.Version = 3
	if err := value.Validate(); err != nil {
		return "", err
	}
	path := filepath.Join(repoRoot, Filename)
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("%s already exists", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	bytes = append(bytes, '\n')
	if err := fsx.WriteFileAtomic(path, bytes, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (c Config) Validate() error {
	if c.Version != 3 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if err := ValidateMaterializer(c.Materializer); err != nil {
		return err
	}
	if c.BranchPrefix == "" {
		return errors.New("branch_prefix cannot be empty")
	}
	if strings.ContainsAny(c.BranchPrefix, " \t\n~^:?*[\\") {
		return fmt.Errorf("branch_prefix %q contains characters Git rejects", c.BranchPrefix)
	}
	return ValidateRules(c.Layers)
}

func ValidateRules(rules []LayerRule) error {
	seen := make(map[string]int, len(rules))
	paths := make([]string, 0, len(rules))
	for index, rule := range rules {
		if err := ValidateRule(rule); err != nil {
			return fmt.Errorf("rules[%d]: %w", index, err)
		}
		clean := cleanRelative(rule.Path)
		if previous, exists := seen[clean]; exists {
			return fmt.Errorf("duplicate path policy %q at rules[%d] and rules[%d]", clean, previous, index)
		}
		seen[clean] = index
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	for i := 1; i < len(paths); i++ {
		if strings.HasPrefix(paths[i], paths[i-1]+"/") {
			return fmt.Errorf("path policies overlap: %q contains %q", paths[i-1], paths[i])
		}
	}
	return nil
}

func ValidateRule(rule LayerRule) error {
	if err := validateRelative(rule.Path, false); err != nil {
		return fmt.Errorf("path: %w", err)
	}
	clean := cleanRelative(rule.Path)
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return errors.New("path may not include .git")
	}
	switch rule.Mode {
	case LayerClone, LayerSeed, LayerRecreate, LayerShare, LayerEmpty, LayerSkip:
	default:
		return fmt.Errorf("unsupported mode %q", rule.Mode)
	}
	for index, input := range rule.EffectiveInputs() {
		if err := validateRelative(input, true); err != nil {
			return fmt.Errorf("inputs[%d]: %w", index, err)
		}
	}
	for field, argv := range map[string][]string{"prepare": rule.Prepare, "validate": rule.Validate} {
		if len(argv) > 0 && strings.TrimSpace(argv[0]) == "" {
			return fmt.Errorf("%s command has an empty executable", field)
		}
		for _, value := range argv {
			if strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("%s command contains NUL", field)
			}
		}
	}
	return nil
}

func MergeRules(groups ...[]LayerRule) ([]LayerRule, error) {
	// Later groups override earlier groups by exact normalized path. This lets a
	// repository override a built-in without copying the rest of the defaults.
	byPath := make(map[string]LayerRule)
	order := make([]string, 0)
	for _, group := range groups {
		for _, rule := range group {
			clean := cleanRelative(rule.Path)
			rule.Path = clean
			if _, exists := byPath[clean]; !exists {
				order = append(order, clean)
			}
			byPath[clean] = rule
		}
	}
	rules := make([]LayerRule, 0, len(byPath))
	for _, path := range order {
		rules = append(rules, byPath[path])
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority > rules[j].Priority
		}
		return rules[i].Path < rules[j].Path
	})
	if err := ValidateRules(rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func ValidateMaterializer(value model.Materializer) error {
	switch value {
	case model.MaterializerAuto, model.MaterializerCoW, model.MaterializerGit, model.MaterializerCopy:
		return nil
	default:
		return fmt.Errorf("unsupported materializer %q", value)
	}
}

func cleanRelative(path string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path))))
}

func validateRelative(path string, allowGlob bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("path cannot be empty")
	}
	if strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("path %q contains NUL", path)
	}
	if filepath.IsAbs(filepath.FromSlash(path)) {
		return fmt.Errorf("path %q must be relative", path)
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes the repository", path)
	}
	if !allowGlob && strings.ContainsAny(path, "*?[") {
		return fmt.Errorf("output path %q must be concrete; globs are supported only for inputs", path)
	}
	return nil
}
