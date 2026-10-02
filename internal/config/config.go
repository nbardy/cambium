package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/model"
)

// Cambium has exactly one configuration format, TOML, in two places with
// different trust:
//
//   - PolicyFilename (.cambium.toml) is committed. Its [[path]] rules are read
//     from the exact target commit; its [settings] are read from the primary
//     checkout. Any branch can edit it, so it may not enable command execution.
//   - LocalFilename (<git-common-dir>/cambium/config.toml) is never committed.
//     It accepts [settings] only and is the sole place allow_policy_commands
//     may be turned on.
//
// Precedence: built-in defaults < committed [settings] < local [settings].
const (
	PolicyFilename = ".cambium.toml"
	LocalFilename  = "config.toml"
)

// LocalPath returns the clone-local settings file inside Git's common dir, so
// it is shared by every linked worktree and can never be committed.
func LocalPath(commonGitDir string) string {
	return filepath.Join(commonGitDir, "cambium", LocalFilename)
}

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
	values := r.Inputs
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
	BranchPrefix         string             `json:"branch_prefix"`
	Materializer         model.Materializer `json:"materializer"`
	RequireCoW           bool               `json:"require_cow"`
	PreparedIndex        bool               `json:"prepared_index"`
	RequireIgnoredLayers bool               `json:"require_ignored_layers"`
	AllowPolicyCommands  bool               `json:"allow_policy_commands"`
}

func Default() Config {
	return Config{
		BranchPrefix:         "cambium/",
		Materializer:         model.MaterializerAuto,
		PreparedIndex:        true,
		RequireIgnoredLayers: true,
	}
}

// Load resolves operational settings: defaults, then the primary checkout's
// committed .cambium.toml [settings], then the clone-local config.toml.
func Load(repoRoot, commonGitDir string) (Config, error) {
	value := Default()
	committed, err := readFile(filepath.Join(repoRoot, PolicyFilename), ScopeCommitted)
	if err != nil {
		return Config{}, err
	}
	local, err := readFile(LocalPath(commonGitDir), ScopeLocal)
	if err != nil {
		return Config{}, err
	}
	for _, file := range []File{committed, local} {
		if err := applySettings(&value, file); err != nil {
			return Config{}, err
		}
	}
	if err := value.Validate(); err != nil {
		return Config{}, err
	}
	return value, nil
}

// WriteLocal writes the clone-local settings file. Only explicitly chosen
// settings are written, so unset keys keep following the committed file.
func WriteLocal(commonGitDir string, settings []Setting, overwrite bool) (string, error) {
	path := LocalPath(commonGitDir)
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("%s already exists; pass --force to replace it", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	var builder strings.Builder
	builder.WriteString("# Cambium settings for this clone only. Never committed.\n")
	builder.WriteString("# Overrides [settings] in the committed " + PolicyFilename + ".\n")
	builder.WriteString("version = 1\n\n[settings]\n")
	for _, setting := range settings {
		builder.WriteString(setting.Key + " = " + setting.Raw + "\n")
	}
	content := builder.String()
	// Round-trip through the real parser so init can never write a file that a
	// later Load rejects.
	parsed, err := Parse(path, strings.NewReader(content), ScopeLocal)
	if err != nil {
		return "", err
	}
	check := Default()
	if err := applySettings(&check, parsed); err != nil {
		return "", err
	}
	if err := check.Validate(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := fsx.WriteFileAtomic(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (c Config) Validate() error {
	if err := ValidateMaterializer(c.Materializer); err != nil {
		return err
	}
	if c.BranchPrefix == "" {
		return errors.New("branch_prefix cannot be empty")
	}
	if strings.ContainsAny(c.BranchPrefix, " \t\n~^:?*[\\") {
		return fmt.Errorf("branch_prefix %q contains characters Git rejects", c.BranchPrefix)
	}
	return nil
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
