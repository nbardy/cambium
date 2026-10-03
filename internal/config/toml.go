package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nbardy/cambium/internal/model"
)

// Scope says which of the two configuration files is being parsed. It decides
// which keys are legal, so a trust violation is a parse error, never a value
// that is silently ignored.
type Scope int

const (
	// ScopeCommitted is .cambium.toml: [settings] and [[path]] rules, but never
	// allow_policy_commands, because any branch can edit it.
	ScopeCommitted Scope = iota
	// ScopeLocal is <git-common-dir>/cambium/config.toml: [settings] only.
	ScopeLocal
)

// Setting is one validated `key = value` line from a [settings] table. Raw is
// the TOML literal; applySettings decodes it into Config.
type Setting struct {
	Key  string
	Raw  string
	Line int
}

// File is one parsed configuration file. A missing file parses to an empty
// File: absence means "no overrides", which is the documented meaning.
type File struct {
	Name     string
	Settings []Setting
	Rules    []LayerRule
}

// StringSetting and BoolSetting build settings for WriteLocal.
func StringSetting(key, value string) Setting {
	return Setting{Key: key, Raw: strconv.Quote(value)}
}

func BoolSetting(key string, value bool) Setting {
	return Setting{Key: key, Raw: strconv.FormatBool(value)}
}

type section int

const (
	sectionTop section = iota
	sectionSettings
	sectionPath
)

// LoadPolicyFile reads [[path]] rules from the primary checkout's committed
// .cambium.toml.
func LoadPolicyFile(repoRoot string) ([]LayerRule, error) {
	file, err := readFile(filepath.Join(repoRoot, PolicyFilename), ScopeCommitted)
	if err != nil {
		return nil, err
	}
	return file.Rules, nil
}

// ParsePolicy reads [[path]] rules from committed .cambium.toml content, for
// example read straight from a target Git tree without a worktree.
func ParsePolicy(name string, reader io.Reader) ([]LayerRule, error) {
	file, err := Parse(name, reader, ScopeCommitted)
	if err != nil {
		return nil, err
	}
	return file.Rules, nil
}

func readFile(path string, scope Scope) (File, error) {
	handle, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{Name: path}, nil
	}
	if err != nil {
		return File{}, err
	}
	defer handle.Close()
	return Parse(path, handle, scope)
}

// Parse reads the deliberately small, dependency-free TOML subset Cambium
// uses: `version = 1`, an optional [settings] table, and [[path]] tables.
// Supported values are strings, booleans, integers, and one- or multi-line
// arrays of strings. Unknown, duplicate, and out-of-scope keys are rejected so
// a configuration mistake cannot silently change filesystem behavior.
func Parse(name string, reader io.Reader, scope Scope) (File, error) {
	file := File{Name: name}
	var current *LayerRule
	var currentKeys map[string]int
	settingKeys := map[string]int{}
	at := sectionTop
	seenVersion := false
	seenSettings := false
	pending := ""
	pendingLine := 0

	consume := func(statement string, lineNumber int) error {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			return nil
		}
		switch statement {
		case "[settings]":
			if !seenVersion {
				return fmt.Errorf("%s:%d: version = 1 must appear before [settings]", name, lineNumber)
			}
			if seenSettings {
				return fmt.Errorf("%s:%d: duplicate [settings] table", name, lineNumber)
			}
			seenSettings = true
			at = sectionSettings
			return nil
		case "[[path]]":
			if !seenVersion {
				return fmt.Errorf("%s:%d: version = 1 must appear before [[path]]", name, lineNumber)
			}
			if scope == ScopeLocal {
				return fmt.Errorf("%s:%d: [[path]] rules belong in the committed %s, not the local config", name, lineNumber, PolicyFilename)
			}
			file.Rules = append(file.Rules, LayerRule{Origin: name})
			current = &file.Rules[len(file.Rules)-1]
			currentKeys = map[string]int{}
			at = sectionPath
			return nil
		}
		if strings.HasPrefix(statement, "[") {
			return fmt.Errorf("%s:%d: unknown table %s", name, lineNumber, statement)
		}
		key, raw, ok := strings.Cut(statement, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected key = value", name, lineNumber)
		}
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		switch at {
		case sectionTop:
			if key != "version" {
				return fmt.Errorf("%s:%d: only version is allowed before [settings] or [[path]]", name, lineNumber)
			}
			if seenVersion {
				return fmt.Errorf("%s:%d: duplicate version", name, lineNumber)
			}
			version, err := strconv.Atoi(raw)
			if err != nil || version != 1 {
				return fmt.Errorf("%s:%d: supported config version is 1", name, lineNumber)
			}
			seenVersion = true
			return nil
		case sectionSettings:
			if previous, exists := settingKeys[key]; exists {
				return fmt.Errorf("%s:%d: duplicate setting %q (first set on line %d)", name, lineNumber, key, previous)
			}
			settingKeys[key] = lineNumber
			setting := Setting{Key: key, Raw: raw, Line: lineNumber}
			if err := checkSetting(setting, scope); err != nil {
				return fmt.Errorf("%s:%d: %w", name, lineNumber, err)
			}
			file.Settings = append(file.Settings, setting)
			return nil
		case sectionPath:
			if previous, exists := currentKeys[key]; exists {
				return fmt.Errorf("%s:%d: duplicate path-policy key %q (first set on line %d)", name, lineNumber, key, previous)
			}
			currentKeys[key] = lineNumber
			if err := assignPolicyValue(current, key, raw); err != nil {
				return fmt.Errorf("%s:%d: %w", name, lineNumber, err)
			}
			return nil
		default:
			panic(fmt.Sprintf("unhandled config section %d", at))
		}
	}

	scanner := bufio.NewScanner(reader)
	// Config files are small, but commands/input lists may exceed Scanner's
	// conservative default token size.
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if pending != "" {
			if line != "" {
				pending += " " + line
			}
			if !tomlStatementComplete(pending) {
				continue
			}
			if err := consume(pending, pendingLine); err != nil {
				return File{}, err
			}
			pending = ""
			pendingLine = 0
			continue
		}
		if line == "" {
			continue
		}
		if strings.Contains(line, "=") && !tomlStatementComplete(line) {
			pending = line
			pendingLine = lineNumber
			continue
		}
		if err := consume(line, lineNumber); err != nil {
			return File{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return File{}, err
	}
	if pending != "" {
		return File{}, fmt.Errorf("%s:%d: unterminated value", name, pendingLine)
	}
	if !seenVersion {
		return File{}, fmt.Errorf("%s: missing required version = 1", name)
	}
	for i := range file.Rules {
		if file.Rules[i].Path == "" {
			return File{}, fmt.Errorf("%s: path rule %d has no path", name, i+1)
		}
		if file.Rules[i].Mode == "" {
			return File{}, fmt.Errorf("%s: path rule %d has no policy", name, i+1)
		}
		if err := ValidateRule(file.Rules[i]); err != nil {
			return File{}, fmt.Errorf("%s: path rule %d: %w", name, i+1, err)
		}
	}
	return file, nil
}

// checkSetting enforces the trust boundary and value types at parse time.
func checkSetting(setting Setting, scope Scope) error {
	if setting.Key == "allow_policy_commands" && scope == ScopeCommitted {
		return fmt.Errorf("allow_policy_commands may only be set in the local config (%s under the Git common dir), never in the committed %s: a branch must not be able to enable command execution", filepath.Join("cambium", LocalFilename), PolicyFilename)
	}
	var probe Config
	return assignSetting(&probe, setting)
}

func applySettings(value *Config, file File) error {
	for _, setting := range file.Settings {
		if err := assignSetting(value, setting); err != nil {
			return fmt.Errorf("%s:%d: %w", file.Name, setting.Line, err)
		}
	}
	return nil
}

func assignSetting(value *Config, setting Setting) error {
	switch setting.Key {
	case "branch_prefix":
		parsed, err := parseTOMLString(setting.Raw)
		if err != nil {
			return fmt.Errorf("branch_prefix: %w", err)
		}
		value.BranchPrefix = parsed
	case "materializer":
		parsed, err := parseTOMLString(setting.Raw)
		if err != nil {
			return fmt.Errorf("materializer: %w", err)
		}
		if err := ValidateMaterializer(model.Materializer(parsed)); err != nil {
			return err
		}
		value.Materializer = model.Materializer(parsed)
	case "require_cow":
		return parseBoolInto(&value.RequireCoW, setting)
	case "prepared_index":
		return parseBoolInto(&value.PreparedIndex, setting)
	case "require_ignored_layers":
		return parseBoolInto(&value.RequireIgnoredLayers, setting)
	case "allow_policy_commands":
		return parseBoolInto(&value.AllowPolicyCommands, setting)
	default:
		return fmt.Errorf("unknown setting %q", setting.Key)
	}
	return nil
}

func parseBoolInto(destination *bool, setting Setting) error {
	return parseTOMLBool(destination, setting.Key, setting.Raw)
}

func assignPolicyValue(rule *LayerRule, key, raw string) error {
	switch key {
	case "name":
		value, err := parseTOMLString(raw)
		if err != nil {
			return err
		}
		rule.Name = value
	case "path":
		value, err := parseTOMLString(raw)
		if err != nil {
			return err
		}
		rule.Path = value
	case "policy":
		value, err := parseTOMLString(raw)
		if err != nil {
			return err
		}
		rule.Mode = LayerMode(value)
	case "inputs":
		value, err := parseTOMLStringArray(raw)
		if err != nil {
			return err
		}
		rule.Inputs = value
	case "prepare":
		value, err := parseTOMLStringArray(raw)
		if err != nil {
			return err
		}
		rule.Prepare = value
	case "validate":
		value, err := parseTOMLStringArray(raw)
		if err != nil {
			return err
		}
		rule.Validate = value
	case "allow_unignored":
		return parseTOMLBool(&rule.AllowUnignored, key, raw)
	case "source_sensitive":
		return parseTOMLBool(&rule.SourceSensitive, key, raw)
	case "required":
		return parseTOMLBool(&rule.Required, key, raw)
	case "activate_on_inputs":
		return parseTOMLBool(&rule.ActivateOnInputs, key, raw)
	case "priority":
		value, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("priority must be an integer")
		}
		rule.Priority = value
	default:
		return fmt.Errorf("unknown path-policy key %q", key)
	}
	return nil
}

func parseTOMLBool(destination *bool, key, raw string) error {
	if raw != "true" && raw != "false" {
		return fmt.Errorf("%s must be true or false", key)
	}
	*destination = raw == "true"
	return nil
}

func stripTOMLComment(line string) string {
	quoted := false
	escaped := false
	for index, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return line[:index]
		}
	}
	return line
}

func tomlStatementComplete(value string) bool {
	_, raw, ok := strings.Cut(value, "=")
	if !ok {
		return true
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '[' {
		return true
	}
	depth := 0
	quoted := false
	escaped := false
	for _, r := range raw {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return !quoted && depth == 0
}

func parseTOMLString(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", fmt.Errorf("expected a quoted string")
	}
	value, err := strconv.Unquote(raw)
	if err != nil {
		return "", fmt.Errorf("invalid string: %w", err)
	}
	return value, nil
}

func parseTOMLStringArray(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, fmt.Errorf("expected an array of quoted strings")
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return []string{}, nil
	}
	var values []string
	for len(inner) > 0 {
		inner = strings.TrimSpace(inner)
		if inner == "" {
			break
		}
		if inner[0] != '"' {
			return nil, fmt.Errorf("array entries must be quoted strings")
		}
		end := 1
		escaped := false
		for ; end < len(inner); end++ {
			if escaped {
				escaped = false
				continue
			}
			if inner[end] == '\\' {
				escaped = true
				continue
			}
			if inner[end] == '"' {
				break
			}
		}
		if end >= len(inner) {
			return nil, fmt.Errorf("unterminated string in array")
		}
		value, err := strconv.Unquote(inner[:end+1])
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		inner = strings.TrimSpace(inner[end+1:])
		if inner == "" {
			break
		}
		if inner[0] != ',' {
			return nil, fmt.Errorf("expected comma between array entries")
		}
		inner = strings.TrimSpace(inner[1:])
		// TOML permits a trailing comma in multi-line arrays.
		if inner == "" {
			break
		}
	}
	return values, nil
}
