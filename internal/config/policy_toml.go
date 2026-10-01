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
)

// LoadPolicyFile reads the deliberately small, dependency-free TOML subset
// used by .cambium.toml. Supported values are strings, booleans, integers, and
// one- or multi-line arrays of strings inside [[path]] tables. Unknown and
// duplicate keys are rejected so configuration mistakes cannot silently change
// filesystem behavior.
func LoadPolicyFile(repoRoot string) ([]LayerRule, error) {
	path := filepath.Join(repoRoot, PolicyFilename)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ParsePolicy(path, file)
}

// ParsePolicy parses the strict .cambium.toml subset from reader. name is used
// only for diagnostics, which allows policy files to be read directly from a
// target Git tree without first materializing a worktree.
func ParsePolicy(name string, reader io.Reader) ([]LayerRule, error) {
	var rules []LayerRule
	var current *LayerRule
	var currentKeys map[string]int
	seenVersion := false
	pending := ""
	pendingLine := 0

	consume := func(statement string, lineNumber int) error {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			return nil
		}
		if statement == "[[path]]" || statement == "[[paths]]" {
			if !seenVersion {
				return fmt.Errorf("%s:%d: version = 1 must appear before [[path]]", name, lineNumber)
			}
			rules = append(rules, LayerRule{Origin: name})
			current = &rules[len(rules)-1]
			currentKeys = map[string]int{}
			return nil
		}
		key, raw, ok := strings.Cut(statement, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected key = value", name, lineNumber)
		}
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if current == nil {
			if key != "version" {
				return fmt.Errorf("%s:%d: only version is allowed before [[path]]", name, lineNumber)
			}
			if seenVersion {
				return fmt.Errorf("%s:%d: duplicate version", name, lineNumber)
			}
			version, err := strconv.Atoi(raw)
			if err != nil || version != 1 {
				return fmt.Errorf("%s:%d: supported policy version is 1", name, lineNumber)
			}
			seenVersion = true
			return nil
		}
		canonical := canonicalPolicyKey(key)
		if previous, exists := currentKeys[canonical]; exists {
			return fmt.Errorf("%s:%d: duplicate path-policy key %q (first set on line %d)", name, lineNumber, key, previous)
		}
		currentKeys[canonical] = lineNumber
		if err := assignPolicyValue(current, key, raw); err != nil {
			return fmt.Errorf("%s:%d: %w", name, lineNumber, err)
		}
		return nil
	}

	scanner := bufio.NewScanner(reader)
	// Policy files are small, but commands/input lists may exceed Scanner's
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
				return nil, err
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
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if pending != "" {
		return nil, fmt.Errorf("%s:%d: unterminated value", name, pendingLine)
	}
	if !seenVersion {
		return nil, fmt.Errorf("%s: missing required version = 1", name)
	}
	for i := range rules {
		if rules[i].Path == "" {
			return nil, fmt.Errorf("%s: path rule %d has no path", name, i+1)
		}
		if rules[i].Mode == "" {
			return nil, fmt.Errorf("%s: path rule %d has no policy", name, i+1)
		}
		if err := ValidateRule(rules[i]); err != nil {
			return nil, fmt.Errorf("%s: path rule %d: %w", name, i+1, err)
		}
	}
	return rules, nil
}

func canonicalPolicyKey(key string) string {
	switch key {
	case "pattern":
		return "path"
	case "mode":
		return "policy"
	case "fingerprint":
		return "inputs"
	default:
		return key
	}
}

func assignPolicyValue(rule *LayerRule, key, raw string) error {
	switch key {
	case "name":
		value, err := parseTOMLString(raw)
		if err != nil {
			return err
		}
		rule.Name = value
	case "path", "pattern":
		value, err := parseTOMLString(raw)
		if err != nil {
			return err
		}
		rule.Path = value
	case "policy", "mode":
		value, err := parseTOMLString(raw)
		if err != nil {
			return err
		}
		rule.Mode = LayerMode(value)
	case "inputs", "fingerprint":
		value, err := parseTOMLStringArray(raw)
		if err != nil {
			return err
		}
		rule.Inputs = append(rule.Inputs, value...)
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
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("allow_unignored must be true or false")
		}
		rule.AllowUnignored = value
	case "source_sensitive":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("source_sensitive must be true or false")
		}
		rule.SourceSensitive = value
	case "required":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("required must be true or false")
		}
		rule.Required = value
	case "activate_on_inputs":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("activate_on_inputs must be true or false")
		}
		rule.ActivateOnInputs = value
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
