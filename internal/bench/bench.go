package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/workspace"
)

type Options struct {
	Scratch          string
	Methods          []string
	WorkspaceCount   int
	TrackedFiles     int
	TrackedFileBytes int
	EnvironmentFiles int
	EnvironmentBytes int
	Keep             bool
}

type Report struct {
	Version    int            `json:"version"`
	CreatedAt  time.Time      `json:"created_at"`
	Platform   string         `json:"platform"`
	Filesystem string         `json:"filesystem,omitempty"`
	Options    Options        `json:"options"`
	Results    []MethodResult `json:"results"`
}

type MethodResult struct {
	Method                   string          `json:"method"`
	Semantics                string          `json:"semantics"`
	Available                bool            `json:"available"`
	SkipReason               string          `json:"skip_reason,omitempty"`
	CloneMode                model.CloneMode `json:"clone_mode,omitempty"`
	PrepareSeconds           float64         `json:"prepare_seconds"`
	CreateSeconds            float64         `json:"create_seconds"`
	CreateP50Seconds         float64         `json:"create_p50_seconds"`
	CreateP95Seconds         float64         `json:"create_p95_seconds"`
	FirstStatusSeconds       float64         `json:"first_status_seconds"`
	WarmStatusSeconds        float64         `json:"warm_status_seconds"`
	DirtyStatusSeconds       float64         `json:"dirty_status_seconds"`
	RemoveSeconds            float64         `json:"remove_seconds"`
	PhysicalAddedBytes       int64           `json:"physical_added_bytes"`
	PhysicalAfterRemoveBytes int64           `json:"physical_after_remove_bytes"`
	EnvironmentReady         int             `json:"environment_ready"`
	WorkspaceCount           int             `json:"workspace_count"`
	Notes                    []string        `json:"notes,omitempty"`
}

type methodHarness interface {
	Prepare(context.Context) (model.CloneMode, error)
	Create(context.Context, int) (string, error)
	Remove(context.Context, int, string) error
	Semantics() string
}

func DefaultOptions() Options {
	return Options{
		Methods:          []string{"git", "git-env-copy", "cambium-auto", "cambium-git", "cambium-cow", "cambium-copy", "simgit", "cow"},
		WorkspaceCount:   8,
		TrackedFiles:     10_000,
		TrackedFileBytes: 1024,
		EnvironmentFiles: 2_000,
		EnvironmentBytes: 2048,
	}
}

func Run(ctx context.Context, options Options) (Report, error) {
	if options.WorkspaceCount <= 0 || options.TrackedFiles < 0 || options.TrackedFileBytes < 0 || options.EnvironmentFiles < 0 || options.EnvironmentBytes < 0 {
		return Report{}, errors.New("benchmark counts and sizes must be non-negative; workspace count must be positive")
	}
	if len(options.Methods) == 0 {
		options.Methods = DefaultOptions().Methods
	}
	if options.Scratch == "" {
		var err error
		options.Scratch, err = os.MkdirTemp("", "cambium-benchmark-*")
		if err != nil {
			return Report{}, err
		}
		if !options.Keep {
			defer os.RemoveAll(options.Scratch)
		}
	} else {
		absolute, err := filepath.Abs(options.Scratch)
		if err != nil {
			return Report{}, err
		}
		options.Scratch = absolute
		if err := os.MkdirAll(options.Scratch, 0o755); err != nil {
			return Report{}, err
		}
	}

	report := Report{Version: 1, CreatedAt: time.Now().UTC(), Platform: platformName(), Options: options}
	if fs, err := filesystemName(options.Scratch); err == nil {
		report.Filesystem = fs
	}
	for index, method := range options.Methods {
		method = strings.TrimSpace(method)
		if method == "" {
			continue
		}
		root := filepath.Join(options.Scratch, fmt.Sprintf("%02d-%s", index, sanitize(method)))
		result, err := runMethod(ctx, root, method, options)
		if err != nil {
			return report, fmt.Errorf("benchmark %s: %w", method, err)
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func runMethod(ctx context.Context, root, method string, options Options) (MethodResult, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return MethodResult{}, err
	}
	repo := filepath.Join(root, "repo")
	workspaceRoot := filepath.Join(root, "workspaces")
	if err := createSyntheticRepository(ctx, repo, options); err != nil {
		return MethodResult{}, err
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return MethodResult{}, err
	}
	harness, available, reason, err := newHarness(ctx, method, repo, workspaceRoot, options)
	if err != nil {
		return MethodResult{}, err
	}
	result := MethodResult{Method: method, Available: available, WorkspaceCount: options.WorkspaceCount}
	if !available {
		result.SkipReason = reason
		return result, nil
	}
	result.Semantics = harness.Semantics()
	before, beforeErr := freeBytes(root)
	if beforeErr != nil {
		result.Notes = append(result.Notes, "physical allocation unavailable: "+beforeErr.Error())
	}
	prepareStart := time.Now()
	cloneMode, err := harness.Prepare(ctx)
	result.PrepareSeconds = time.Since(prepareStart).Seconds()
	if err != nil {
		if method == "cambium-cow" {
			result.Available = false
			result.SkipReason = err.Error()
			return result, nil
		}
		return result, err
	}
	result.CloneMode = cloneMode

	paths := make([]string, 0, options.WorkspaceCount)
	createDurations := make([]float64, 0, options.WorkspaceCount)
	createStart := time.Now()
	for i := 0; i < options.WorkspaceCount; i++ {
		start := time.Now()
		path, err := harness.Create(ctx, i)
		if err != nil {
			for k, existing := range paths {
				_ = harness.Remove(context.Background(), k, existing)
			}
			return result, err
		}
		createDurations = append(createDurations, time.Since(start).Seconds())
		paths = append(paths, path)
	}
	result.CreateSeconds = time.Since(createStart).Seconds()
	result.CreateP50Seconds = percentile(createDurations, 0.50)
	result.CreateP95Seconds = percentile(createDurations, 0.95)
	syncFilesystem()
	if beforeErr == nil {
		after, measureErr := freeBytes(root)
		if measureErr == nil {
			result.PhysicalAddedBytes = before - after
		}
	}

	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(path, "deps", "0000", "00000000.bin")); err == nil {
			result.EnvironmentReady++
		}
	}
	statusStart := time.Now()
	for _, path := range paths {
		if err := gitStatus(ctx, path); err != nil {
			return result, err
		}
	}
	result.FirstStatusSeconds = time.Since(statusStart).Seconds()
	statusStart = time.Now()
	for _, path := range paths {
		if err := gitStatus(ctx, path); err != nil {
			return result, err
		}
	}
	result.WarmStatusSeconds = time.Since(statusStart).Seconds()

	for _, path := range paths {
		target := filepath.Join(path, "src", "0000", "00000000.bin")
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return result, err
		}
		if _, err := file.Write([]byte("changed")); err != nil {
			_ = file.Close()
			return result, err
		}
		if err := file.Close(); err != nil {
			return result, err
		}
	}
	statusStart = time.Now()
	for _, path := range paths {
		if err := gitStatus(ctx, path); err != nil {
			return result, err
		}
	}
	result.DirtyStatusSeconds = time.Since(statusStart).Seconds()

	removeStart := time.Now()
	for i, path := range paths {
		if err := harness.Remove(ctx, i, path); err != nil {
			return result, err
		}
	}
	result.RemoveSeconds = time.Since(removeStart).Seconds()
	syncFilesystem()
	if after, err := freeBytes(root); err == nil {
		result.PhysicalAfterRemoveBytes = before - after
	}
	return result, nil
}

func newHarness(ctx context.Context, method, repo, workspaceRoot string, options Options) (methodHarness, bool, string, error) {
	switch method {
	case "git":
		return &gitHarness{repo: repo, root: workspaceRoot, prefix: "git"}, true, "", nil
	case "git-env-copy":
		return &gitHarness{repo: repo, root: workspaceRoot, prefix: "git-env-copy", copyEnvironment: true}, true, "", nil
	case "cambium-auto", "cambium-cow", "cambium-copy", "cambium-git":
		projectValue, err := project.Open(ctx, repo, execx.OSRunner{})
		if err != nil {
			return nil, false, "", err
		}
		materializer := model.Materializer(strings.TrimPrefix(method, "cambium-"))
		return &cambiumHarness{manager: workspace.New(projectValue), root: workspaceRoot, materializer: materializer}, true, "", nil
	case "simgit":
		path, err := exec.LookPath("sg")
		if err != nil {
			return nil, false, "simgit sg binary not installed", nil
		}
		if ok, detail := identifyTool(ctx, path, []string{"worktree", "--help"}, []string{"simgit", "copy-on-write", "linked worktree"}); !ok {
			return nil, false, "sg is installed but is not simgit: " + detail, nil
		}
		return &simgitHarness{binary: path, repo: repo, root: workspaceRoot}, true, "", nil
	case "cow":
		path, err := exec.LookPath("cow")
		if err != nil {
			return nil, false, "cow binary not installed", nil
		}
		if ok, detail := identifyTool(ctx, path, []string{"--help"}, []string{"pasture", "copy-on-write"}); !ok {
			return nil, false, "cow is installed but is not the pasture manager: " + detail, nil
		}
		return &cowHarness{binary: path, repo: repo, root: workspaceRoot, prefix: "cambium-bench-" + strconv.FormatInt(time.Now().UnixNano(), 36)}, true, "", nil
	default:
		return nil, false, "unknown benchmark method", nil
	}
}

type gitHarness struct {
	repo            string
	root            string
	prefix          string
	copyEnvironment bool
}

func (h *gitHarness) Semantics() string {
	if h.copyEnvironment {
		return "git-linked-worktree+copied-environment"
	}
	return "git-linked-worktree"
}
func (h *gitHarness) Prepare(context.Context) (model.CloneMode, error) {
	return model.CloneModeGitCheckout, nil
}
func (h *gitHarness) Create(ctx context.Context, index int) (string, error) {
	path := filepath.Join(h.root, fmt.Sprintf("%s-%02d", h.prefix, index))
	branch := fmt.Sprintf("bench/%s-%02d", h.prefix, index)
	if err := run(ctx, h.repo, "git", "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return "", err
	}
	if h.copyEnvironment {
		if err := run(ctx, h.repo, "cp", "-R", filepath.Join(h.repo, "deps"), filepath.Join(path, "deps")); err != nil {
			_ = run(context.Background(), h.repo, "git", "worktree", "remove", "--force", path)
			_ = run(context.Background(), h.repo, "git", "branch", "-D", branch)
			return "", err
		}
	}
	return path, nil
}
func (h *gitHarness) Remove(ctx context.Context, index int, path string) error {
	if err := run(ctx, h.repo, "git", "worktree", "remove", "--force", path); err != nil {
		return err
	}
	return run(ctx, h.repo, "git", "branch", "-D", fmt.Sprintf("bench/%s-%02d", h.prefix, index))
}

type cambiumHarness struct {
	manager      *workspace.Manager
	root         string
	materializer model.Materializer
}

func (h *cambiumHarness) Semantics() string { return "git-linked-worktree+prepared-environment" }
func (h *cambiumHarness) Prepare(ctx context.Context) (model.CloneMode, error) {
	result, err := h.manager.Prepare(ctx, "HEAD", h.materializer, h.materializer == model.MaterializerCoW)
	if err != nil {
		return "", err
	}
	return result.Plan.CloneMode, nil
}
func (h *cambiumHarness) Create(ctx context.Context, index int) (string, error) {
	name := fmt.Sprintf("cambium-%s-%02d", h.materializer, index)
	value, err := h.manager.Create(ctx, workspace.CreateSpec{Name: name, Path: filepath.Join(h.root, name), Materializer: h.materializer, RequireCoW: h.materializer == model.MaterializerCoW})
	if err != nil {
		return "", err
	}
	return value.Path, nil
}
func (h *cambiumHarness) Remove(ctx context.Context, index int, path string) error {
	name := fmt.Sprintf("cambium-%s-%02d", h.materializer, index)
	return h.manager.Remove(ctx, name, true, true)
}

type simgitHarness struct{ binary, repo, root string }

func (h *simgitHarness) Semantics() string                                { return "git-linked-worktree" }
func (h *simgitHarness) Prepare(context.Context) (model.CloneMode, error) { return "", nil }
func (h *simgitHarness) Create(ctx context.Context, index int) (string, error) {
	path := filepath.Join(h.root, fmt.Sprintf("simgit-%02d", index))
	branch := fmt.Sprintf("bench/simgit-%02d", index)
	if err := run(ctx, h.repo, h.binary, "worktree", "add", branch, path, "--base", "HEAD", "--json"); err != nil {
		return "", err
	}
	return path, nil
}
func (h *simgitHarness) Remove(ctx context.Context, index int, path string) error {
	return run(ctx, h.repo, h.binary, "worktree", "remove", path, "--force", "--delete-branch", "--json")
}

type cowHarness struct{ binary, repo, root, prefix string }

func (h *cowHarness) Semantics() string                                { return "independent-repository-full-tree" }
func (h *cowHarness) Prepare(context.Context) (model.CloneMode, error) { return "", nil }
func (h *cowHarness) Create(ctx context.Context, index int) (string, error) {
	name := fmt.Sprintf("%s-%02d", h.prefix, index)
	command := exec.CommandContext(ctx, h.binary, "create", "--source", h.repo, "--dir", h.root, "--no-symlink", "--print-path", name)
	command.Dir = h.repo
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cow create: %w\n%s", err, output)
	}
	path := strings.TrimSpace(string(output))
	if path == "" {
		path = filepath.Join(h.root, name)
	}
	return path, nil
}
func (h *cowHarness) Remove(ctx context.Context, index int, path string) error {
	name := fmt.Sprintf("%s-%02d", h.prefix, index)
	return run(ctx, h.repo, h.binary, "remove", "--force", "--yes", name)
}

func createSyntheticRepository(ctx context.Context, root string, options Options) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := run(ctx, root, "git", "init", "-q", "-b", "main"); err != nil {
		return err
	}
	if err := run(ctx, root, "git", "config", "user.name", "Cambium Benchmark"); err != nil {
		return err
	}
	if err := run(ctx, root, "git", "config", "user.email", "benchmark@example.invalid"); err != nil {
		return err
	}
	if err := writeFiles(filepath.Join(root, "src"), options.TrackedFiles, options.TrackedFileBytes); err != nil {
		return err
	}
	if err := writeFiles(filepath.Join(root, "deps"), options.EnvironmentFiles, options.EnvironmentBytes); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("deps/\n"), 0o644); err != nil {
		return err
	}
	policy := "version = 1\n\n[[path]]\npath = \"deps\"\npolicy = \"clone\"\ninputs = [\"deps.lock\"]\n"
	if err := os.WriteFile(filepath.Join(root, config.PolicyFilename), []byte(policy), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "deps.lock"), []byte("synthetic-v1\n"), 0o644); err != nil {
		return err
	}
	if err := run(ctx, root, "git", "add", "."); err != nil {
		return err
	}
	return run(ctx, root, "git", "commit", "-q", "-m", "synthetic benchmark")
}

func writeFiles(root string, count, size int) error {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte('a' + (i % 23))
	}
	for i := 0; i < count; i++ {
		path := filepath.Join(root, fmt.Sprintf("%04d", i/1000), fmt.Sprintf("%08d.bin", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func gitStatus(ctx context.Context, path string) error {
	return run(ctx, path, "git", "status", "--porcelain=v1")
}

func run(ctx context.Context, dir, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return nil
}

func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]float64(nil), values...)
	sort.Float64s(copyValues)
	index := int(math.Ceil(q*float64(len(copyValues)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(copyValues) {
		index = len(copyValues) - 1
	}
	return copyValues[index]
}

func sanitize(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}

func identifyTool(ctx context.Context, path string, args, needles []string) (bool, string) {
	command := exec.CommandContext(ctx, path, args...)
	output, err := command.CombinedOutput()
	text := strings.ToLower(string(output))
	for _, needle := range needles {
		if strings.Contains(text, strings.ToLower(needle)) {
			return true, ""
		}
	}
	if err != nil {
		return false, strings.TrimSpace(fmt.Sprintf("%v: %s", err, output))
	}
	firstLine := strings.TrimSpace(string(output))
	if index := strings.IndexByte(firstLine, '\n'); index >= 0 {
		firstLine = firstLine[:index]
	}
	if firstLine == "" {
		firstLine = "help output did not identify the expected tool"
	}
	return false, firstLine
}

func EncodeJSON(report Report) ([]byte, error) {
	bytes, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(bytes, '\n'), nil
}
