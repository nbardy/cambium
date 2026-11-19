package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// Command is a deterministic description of one child process invocation.
type Command struct {
	Dir   string
	Env   map[string]string
	Name  string
	Args  []string
	Stdin io.Reader
}

// Result captures process output without losing the exit status.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner exists so workspace operations can be integration-tested without
// smuggling process execution throughout the codebase.
type Runner interface {
	Run(context.Context, Command) (Result, error)
	LookPath(string) (string, error)
}

// OSRunner runs real processes. Scrub (the scrub_env setting) gives every child
// an allow-listed environment and no Git hooks, so a repository hook run by a
// checkout never sees the caller's provider keys.
type OSRunner struct{ Scrub bool }

func (OSRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (r OSRunner) Run(ctx context.Context, spec Command) (Result, error) {
	if spec.Name == "" {
		return Result{}, errors.New("empty command name")
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdin = spec.Stdin
	cmd.Env = childEnv(os.Environ(), spec.Env, r.Scrub)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, &CommandError{Command: spec, Result: result, Cause: err}
	}
	result.ExitCode = -1
	return result, &CommandError{Command: spec, Result: result, Cause: err}
}

// repositorySelectors are the variables Git reads to pick a repository. A
// caller running inside a Git hook (or a wrapper that exports them) must not
// aim Cambium's own Git calls at some other repository.
var repositorySelectors = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true,
	"GIT_PREFIX": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE": true, "GIT_CEILING_DIRECTORIES": true,
}

var scrubAllow = regexp.MustCompile(`^(PATH|HOME|USER|LOGNAME|LANG|LC_[A-Z_]+|TZ|TMPDIR|GIT_(AUTHOR|COMMITTER)_(NAME|EMAIL|DATE))$`)

// childEnv builds a child's environment from the parent's plus explicit
// extras. Repository selectors are always dropped from the parent's; extras
// (GIT_INDEX_FILE for a private index) are the command's own and always kept.
func childEnv(parent []string, extra map[string]string, scrub bool) []string {
	values := make(map[string]string, len(parent)+len(extra)+4)
	for _, item := range parent {
		key, value, ok := strings.Cut(item, "=")
		if !ok || repositorySelectors[key] {
			continue
		}
		if scrub && !scrubAllow.MatchString(key) {
			continue
		}
		values[key] = value
	}
	if scrub {
		values["GIT_CONFIG_COUNT"] = "2"
		values["GIT_CONFIG_KEY_0"] = "core.hooksPath"
		values["GIT_CONFIG_VALUE_0"] = "/dev/null"
		values["GIT_CONFIG_KEY_1"] = "core.fsmonitor"
		values["GIT_CONFIG_VALUE_1"] = "false"
		values["GIT_TERMINAL_PROMPT"] = "0"
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

type CommandError struct {
	Command Command
	Result  Result
	Cause   error
}

func (e *CommandError) Error() string {
	stderr := strings.TrimSpace(e.Result.Stderr)
	if stderr == "" {
		stderr = strings.TrimSpace(e.Result.Stdout)
	}
	if stderr == "" {
		return fmt.Sprintf("%s failed with exit code %d: %v", formatCommand(e.Command), e.Result.ExitCode, e.Cause)
	}
	return fmt.Sprintf("%s failed with exit code %d: %s", formatCommand(e.Command), e.Result.ExitCode, stderr)
}

func (e *CommandError) Unwrap() error { return e.Cause }

func formatCommand(command Command) string {
	parts := append([]string{command.Name}, command.Args...)
	return strings.Join(parts, " ")
}
