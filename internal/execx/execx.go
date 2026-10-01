package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

type OSRunner struct{}

func (OSRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (OSRunner) Run(ctx context.Context, spec Command) (Result, error) {
	if spec.Name == "" {
		return Result{}, errors.New("empty command name")
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Stdin = spec.Stdin
	cmd.Env = mergedEnv(spec.Env)

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

func mergedEnv(extra map[string]string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	values := make(map[string]string, len(os.Environ())+len(extra))
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
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
