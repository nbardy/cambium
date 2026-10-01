package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/environment"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/project"
)

const layerVersion = 5

var errBuiltinNotManaged = errors.New("built-in path is not ignored or is tracked")

type ReceiptInput struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Object string `json:"object"`
}

type LayerSnapshot struct {
	Version     int              `json:"version"`
	ID          string           `json:"id"`
	Rule        config.LayerRule `json:"rule"`
	RuleBuiltin bool             `json:"rule_builtin,omitempty"`
	RuleOrigin  string           `json:"rule_origin,omitempty"`
	Present     bool             `json:"present"`
	Expected    bool             `json:"expected"`
	Payload     string           `json:"payload,omitempty"`
	SourceRef   string           `json:"source_ref"`
	Inputs      []ReceiptInput   `json:"inputs,omitempty"`
	Validated   bool             `json:"validated"`
	CreatedAt   time.Time        `json:"created_at"`
}

type LayerApplyResult struct {
	IDs       []string `json:"ids"`
	Ready     bool     `json:"ready"`
	Missing   []string `json:"missing,omitempty"`
	Recreated []string `json:"recreated,omitempty"`
}

// PrepareLayers resolves branch-correct receipts from the target Git tree. A
// source output is snapshotted only when the primary checkout has the same
// receipt; this prevents a worktree for branch B from receiving branch A's
// node_modules merely because both directories happen to exist locally.
func PrepareLayers(ctx context.Context, project *project.Project, commit string, cloner Cloner, requireCoW bool) ([]LayerSnapshot, error) {
	targetEntries, err := project.Repository.TreeEntries(ctx, commit)
	if err != nil {
		return nil, err
	}
	sourceCommit, sourceCommitErr := project.Repository.ResolveCommitAt(ctx, project.Repository.Root, "HEAD")
	var sourceEntries []gitx.TreeEntry
	if sourceCommitErr == nil {
		sourceEntries, sourceCommitErr = project.Repository.TreeEntries(ctx, sourceCommit)
	}

	rules, err := environment.ResolveRulesAt(ctx, project.Repository, commit, project.Config.Layers)
	if err != nil {
		return nil, fmt.Errorf("resolve environment policies at %s: %w", commit, err)
	}
	layers := make([]LayerSnapshot, 0, len(rules))
	for _, rule := range rules {
		active, targetInputs := activeRule(rule, targetEntries, filepath.Join(project.Repository.Root, filepath.FromSlash(rule.Path)))
		id := fingerprintRule(commit, rule, targetInputs)
		if cached, ok := loadLayer(filepath.Join(project.LayersDir(), id)); ok {
			_ = touchCacheEntry(filepath.Join(project.LayersDir(), id))
			layers = append(layers, cached)
			continue
		}
		if !active {
			continue
		}
		sourcePath := filepath.Join(project.Repository.Root, filepath.FromSlash(rule.Path))
		_, sourcePathErr := os.Lstat(sourcePath)
		sourcePresent := sourcePathErr == nil
		if sourcePathErr != nil && !errors.Is(sourcePathErr, os.ErrNotExist) {
			return nil, sourcePathErr
		}
		base := LayerSnapshot{Version: layerVersion, ID: id, Rule: rule, RuleBuiltin: rule.Builtin, RuleOrigin: rule.Origin, Expected: rule.Required || sourcePresent, SourceRef: commit, Inputs: targetInputs, CreatedAt: time.Now().UTC()}
		switch rule.Mode {
		case config.LayerSkip, config.LayerEmpty, config.LayerShare, config.LayerRecreate:
			layer, err := publishMetadataLayer(ctx, project, base)
			if err != nil {
				return nil, err
			}
			layers = append(layers, layer)
		case config.LayerClone, config.LayerSeed:
			// Only snapshot the primary checkout if its source inputs describe the
			// same environment receipt as the requested target tree.
			if sourceCommitErr != nil {
				layer, publishErr := publishMetadataLayer(ctx, project, base)
				if publishErr != nil {
					return nil, publishErr
				}
				layers = append(layers, layer)
				continue
			}
			_, sourceInputs := activeRule(rule, sourceEntries, filepath.Join(project.Repository.Root, filepath.FromSlash(rule.Path)))
			if fingerprintRule(sourceCommit, rule, sourceInputs) != id {
				layer, publishErr := publishMetadataLayer(ctx, project, base)
				if publishErr != nil {
					return nil, publishErr
				}
				layers = append(layers, layer)
				continue
			}
			if err := validateLayerSafetyAt(ctx, project, project.Repository.Root, rule); err != nil {
				if errors.Is(err, errBuiltinNotManaged) {
					continue
				}
				return nil, err
			}
			layer, err := publishLayer(ctx, project, project.Repository.Root, commit, rule, targetInputs, cloner, requireCoW, false)
			if err != nil {
				return nil, err
			}
			layers = append(layers, layer)
		default:
			return nil, fmt.Errorf("unsupported layer mode %q", rule.Mode)
		}
	}
	return layers, nil
}

func ApplyLayers(ctx context.Context, project *project.Project, workspacePath string, layers []LayerSnapshot, cloner Cloner, requireCoW bool) (LayerApplyResult, error) {
	return applyLayers(ctx, project, workspacePath, layers, cloner, requireCoW, false)
}

func ApplyLayersReplacing(ctx context.Context, project *project.Project, workspacePath string, layers []LayerSnapshot, cloner Cloner, requireCoW bool) (LayerApplyResult, error) {
	return applyLayers(ctx, project, workspacePath, layers, cloner, requireCoW, true)
}

func applyLayers(ctx context.Context, project *project.Project, workspacePath string, layers []LayerSnapshot, cloner Cloner, requireCoW, replace bool) (LayerApplyResult, error) {
	result := LayerApplyResult{Ready: true, IDs: make([]string, 0, len(layers))}
	for _, layer := range layers {
		rule := layer.Rule
		if err := validateLayerSafetyAt(ctx, project, workspacePath, rule); err != nil {
			if errors.Is(err, errBuiltinNotManaged) {
				continue
			}
			return LayerApplyResult{}, err
		}
		destination := filepath.Join(workspacePath, filepath.FromSlash(rule.Path))
		source := filepath.Join(project.Repository.Root, filepath.FromSlash(rule.Path))
		if replace && rule.Mode != config.LayerSkip {
			if err := removeIfPresent(destination); err != nil {
				return LayerApplyResult{}, err
			}
		}
		switch rule.Mode {
		case config.LayerSkip:
			// Deliberately unmanaged. This is used for secrets, sockets,
			// databases, and unknown runtime state.
		case config.LayerEmpty:
			if err := ensureAbsent(destination); err != nil {
				return LayerApplyResult{}, err
			}
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return LayerApplyResult{}, err
			}
		case config.LayerShare:
			if _, err := os.Stat(source); err != nil {
				if rule.Required {
					result.Ready = false
					result.Missing = append(result.Missing, rule.Path)
				}
				break
			}
			if err := ensureAbsent(destination); err != nil {
				return LayerApplyResult{}, err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return LayerApplyResult{}, err
			}
			if err := os.Symlink(source, destination); err != nil {
				return LayerApplyResult{}, err
			}
		case config.LayerClone, config.LayerSeed:
			if !layer.Present {
				if rule.Mode == config.LayerClone && (layer.Expected || rule.Required) {
					result.Ready = false
					result.Missing = append(result.Missing, rule.Path)
				}
				break
			}
			if err := ensureAbsent(destination); err != nil {
				return LayerApplyResult{}, err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return LayerApplyResult{}, err
			}
			if _, err := cloner.ClonePath(ctx, layer.Payload, destination, requireCoW); err != nil {
				return LayerApplyResult{}, fmt.Errorf("materialize layer %s: %w", rule.Path, err)
			}
			if err := runRuleCommand(ctx, project, workspacePath, rule.Validate, "validate", rule.Path); err != nil {
				return LayerApplyResult{}, err
			}
		case config.LayerRecreate:
			if err := removeIfPresent(destination); err != nil {
				return LayerApplyResult{}, err
			}
			if len(rule.Prepare) == 0 {
				if layer.Expected || rule.Required {
					result.Ready = false
					result.Missing = append(result.Missing, rule.Path)
				}
				break
			}
			if err := runRuleCommand(ctx, project, workspacePath, rule.Prepare, "prepare", rule.Path); err != nil {
				return LayerApplyResult{}, err
			}
			if err := runRuleCommand(ctx, project, workspacePath, rule.Validate, "validate", rule.Path); err != nil {
				return LayerApplyResult{}, err
			}
			if _, err := os.Lstat(destination); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					result.Ready = false
					result.Missing = append(result.Missing, rule.Path)
					break
				}
				return LayerApplyResult{}, err
			}
			layer.Present = true
			layer.Expected = true
			layer.Validated = len(rule.Validate) > 0
			if _, err := publishMetadataLayer(ctx, project, layer); err != nil {
				return LayerApplyResult{}, fmt.Errorf("record recreated environment path %s: %w", rule.Path, err)
			}
			result.Recreated = append(result.Recreated, rule.Path)
		default:
			return LayerApplyResult{}, fmt.Errorf("unsupported layer mode %q", rule.Mode)
		}
		result.IDs = append(result.IDs, layer.ID)
	}
	sort.Strings(result.Missing)
	sort.Strings(result.Recreated)
	return result, nil
}

// CaptureWorkspaceLayers publishes matching clone/seed outputs from a live
// workspace against sourceRef (normally a speculative root). Recreate outputs
// are represented by their receipt and regenerated on fork rather than copied.
func CaptureWorkspaceLayers(ctx context.Context, project *project.Project, workspacePath, sourceRef string, cloner Cloner, requireCoW bool) ([]LayerSnapshot, error) {
	entries, err := project.Repository.TreeEntries(ctx, sourceRef)
	if err != nil {
		return nil, err
	}
	rules, err := environment.ResolveRulesAt(ctx, project.Repository, sourceRef, project.Config.Layers)
	if err != nil {
		return nil, fmt.Errorf("resolve environment policies at %s: %w", sourceRef, err)
	}
	var layers []LayerSnapshot
	for _, rule := range rules {
		active, inputs := activeRule(rule, entries, filepath.Join(workspacePath, filepath.FromSlash(rule.Path)))
		if !active {
			continue
		}
		id := fingerprintRule(sourceRef, rule, inputs)
		_, pathErr := os.Lstat(filepath.Join(workspacePath, filepath.FromSlash(rule.Path)))
		base := LayerSnapshot{Version: layerVersion, ID: id, Rule: rule, RuleBuiltin: rule.Builtin, RuleOrigin: rule.Origin, Present: pathErr == nil, Expected: rule.Required || pathErr == nil, SourceRef: sourceRef, Inputs: inputs, CreatedAt: time.Now().UTC()}
		if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
			return nil, pathErr
		}
		if existing, ok := loadLayer(filepath.Join(project.LayersDir(), id)); ok {
			if rule.Mode == config.LayerClone || rule.Mode == config.LayerSeed {
				if !(base.Present && !existing.Present) {
					layers = append(layers, existing)
					continue
				}
			}
			// Metadata-only policies merge live presence/validation observations
			// into the stable receipt instead of returning stale cache metadata.
		}
		if err := validateLayerSafetyAt(ctx, project, workspacePath, rule); err != nil {
			if errors.Is(err, errBuiltinNotManaged) {
				continue
			}
			return nil, err
		}
		switch rule.Mode {
		case config.LayerClone, config.LayerSeed:
			layer, err := publishLayer(ctx, project, workspacePath, sourceRef, rule, inputs, cloner, requireCoW, true)
			if err != nil {
				return nil, err
			}
			layers = append(layers, layer)
		default:
			if base.Present && len(rule.Validate) > 0 {
				if err := runRuleCommand(ctx, project, workspacePath, rule.Validate, "validate", rule.Path); err != nil {
					return nil, err
				}
				base.Validated = true
			}
			layer, err := publishMetadataLayer(ctx, project, base)
			if err != nil {
				return nil, err
			}
			layers = append(layers, layer)
		}
	}
	return layers, nil
}

func LoadLayers(project *project.Project, ids []string) ([]LayerSnapshot, error) {
	layers := make([]LayerSnapshot, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		layer, ok := loadLayer(filepath.Join(project.LayersDir(), id))
		if !ok {
			return nil, fmt.Errorf("environment layer %s is missing or incomplete", id)
		}
		layers = append(layers, layer)
	}
	return layers, nil
}

func publishMetadataLayer(ctx context.Context, project *project.Project, layer LayerSnapshot) (LayerSnapshot, error) {
	finalRoot := filepath.Join(project.LayersDir(), layer.ID)
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(project.LocksDir(), "layer-"+layer.ID+".lock"))
	if err != nil {
		return LayerSnapshot{}, err
	}
	defer lock.Release()

	if existing, ok := loadLayer(finalRoot); ok {
		merged := mergeLayerObservation(existing, layer)
		metadata, err := json.MarshalIndent(merged, "", "  ")
		if err != nil {
			return LayerSnapshot{}, err
		}
		if err := fsx.WriteFileAtomic(filepath.Join(finalRoot, "metadata.json"), append(metadata, '\n'), 0o644); err != nil {
			return LayerSnapshot{}, err
		}
		_ = touchCacheEntry(finalRoot)
		return merged, nil
	}
	temporary, err := os.MkdirTemp(project.LayersDir(), ".layer-"+layer.ID+"-*")
	if err != nil {
		return LayerSnapshot{}, err
	}
	defer os.RemoveAll(temporary)
	metadata, err := json.MarshalIndent(layer, "", "  ")
	if err != nil {
		return LayerSnapshot{}, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "metadata.json"), append(metadata, '\n'), 0o644); err != nil {
		return LayerSnapshot{}, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "ready"), []byte(layer.ID+"\n"), 0o444); err != nil {
		return LayerSnapshot{}, err
	}
	if err := fsx.PublishDir(temporary, finalRoot); err != nil {
		if existing, ok := loadLayer(finalRoot); ok {
			return existing, nil
		}
		return LayerSnapshot{}, err
	}
	return layer, nil
}

func mergeLayerObservation(existing, observed LayerSnapshot) LayerSnapshot {
	merged := observed
	if !existing.CreatedAt.IsZero() && (merged.CreatedAt.IsZero() || existing.CreatedAt.Before(merged.CreatedAt)) {
		merged.CreatedAt = existing.CreatedAt
	}
	merged.Present = existing.Present || observed.Present
	merged.Expected = existing.Expected || observed.Expected
	merged.Validated = existing.Validated || observed.Validated
	if existing.Payload != "" && merged.Payload == "" {
		merged.Payload = existing.Payload
	}
	return merged
}

func publishLayer(ctx context.Context, project *project.Project, sourceRoot, sourceRef string, rule config.LayerRule, inputs []ReceiptInput, cloner Cloner, requireCoW, upgradeMissing bool) (LayerSnapshot, error) {
	id := fingerprintRule(sourceRef, rule, inputs)
	finalRoot := filepath.Join(project.LayersDir(), id)
	source := filepath.Join(sourceRoot, filepath.FromSlash(rule.Path))
	sourcePresent := false
	if _, statErr := os.Lstat(source); statErr == nil {
		sourcePresent = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return LayerSnapshot{}, statErr
	}
	if layer, ok := loadLayer(finalRoot); ok && !(upgradeMissing && sourcePresent && !layer.Present) {
		_ = touchCacheEntry(finalRoot)
		return layer, nil
	}
	lock, err := lockfile.AcquireContext(ctx, filepath.Join(project.LocksDir(), "layer-"+id+".lock"))
	if err != nil {
		return LayerSnapshot{}, err
	}
	defer lock.Release()
	if layer, ok := loadLayer(finalRoot); ok {
		if !(upgradeMissing && sourcePresent && !layer.Present) {
			return layer, nil
		}
		if err := os.RemoveAll(finalRoot); err != nil {
			return LayerSnapshot{}, err
		}
	}
	if _, err := os.Stat(finalRoot); err == nil {
		return LayerSnapshot{}, fmt.Errorf("incomplete layer exists at %s; run `cambium doctor --repair`", finalRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return LayerSnapshot{}, err
	}

	temporary, err := os.MkdirTemp(project.LayersDir(), ".layer-"+id+"-*")
	if err != nil {
		return LayerSnapshot{}, err
	}
	defer os.RemoveAll(temporary)
	payload := filepath.Join(temporary, "payload")
	present := true
	validated := false
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		present = false
	} else if err != nil {
		return LayerSnapshot{}, err
	} else {
		if err := runRuleCommand(ctx, project, sourceRoot, rule.Validate, "validate", rule.Path); err != nil {
			return LayerSnapshot{}, err
		}
		validated = len(rule.Validate) > 0
		if _, err := cloner.ClonePath(ctx, source, payload, requireCoW); err != nil {
			return LayerSnapshot{}, fmt.Errorf("snapshot layer %s: %w", rule.Path, err)
		}
	}
	layer := LayerSnapshot{Version: layerVersion, ID: id, Rule: rule, RuleBuiltin: rule.Builtin, RuleOrigin: rule.Origin, Present: present, Expected: rule.Required || sourcePresent, SourceRef: sourceRef, Inputs: inputs, Validated: validated, CreatedAt: time.Now().UTC()}
	if present {
		layer.Payload = filepath.Join(finalRoot, "payload")
	}
	metadata, err := json.MarshalIndent(layer, "", "  ")
	if err != nil {
		return LayerSnapshot{}, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "metadata.json"), append(metadata, '\n'), 0o644); err != nil {
		return LayerSnapshot{}, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "ready"), []byte(id+"\n"), 0o444); err != nil {
		return LayerSnapshot{}, err
	}
	if err := fsx.PublishDir(temporary, finalRoot); err != nil {
		if existing, ok := loadLayer(finalRoot); ok {
			return existing, nil
		}
		return LayerSnapshot{}, err
	}
	return layer, nil
}

func activeRule(rule config.LayerRule, entries []gitx.TreeEntry, sourcePath string) (bool, []ReceiptInput) {
	patterns := rule.EffectiveInputs()
	var inputs []ReceiptInput
	for _, entry := range entries {
		for _, pattern := range patterns {
			if gitx.MatchPath(pattern, entry.Path) {
				inputs = append(inputs, ReceiptInput{Path: entry.Path, Mode: entry.Mode, Object: entry.Object})
				break
			}
		}
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	if !rule.Builtin || rule.ActivateOnInputs && len(inputs) > 0 {
		return true, inputs
	}
	if _, err := os.Lstat(sourcePath); err == nil {
		return true, inputs
	}
	return false, inputs
}

func fingerprintRule(sourceRef string, rule config.LayerRule, inputs []ReceiptInput) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("cambium-environment-receipt-v5\x00"))
	_, _ = hash.Write([]byte(runtime.GOOS + "\x00" + runtime.GOARCH + "\x00"))
	_, _ = hash.Write([]byte(rule.Path + "\x00" + string(rule.Mode) + "\x00"))
	for _, pattern := range rule.EffectiveInputs() {
		_, _ = hash.Write([]byte("input-pattern\x00" + pattern + "\x00"))
	}
	_, _ = hash.Write([]byte(fmt.Sprintf("required=%t\x00activate=%t\x00allow-unignored=%t\x00builtin=%t\x00", rule.Required, rule.ActivateOnInputs, rule.AllowUnignored, rule.Builtin)))
	for _, value := range rule.Prepare {
		_, _ = hash.Write([]byte("prepare\x00" + value + "\x00"))
	}
	for _, value := range rule.Validate {
		_, _ = hash.Write([]byte("validate\x00" + value + "\x00"))
	}
	for _, input := range inputs {
		_, _ = hash.Write([]byte(input.Path + "\x00" + input.Mode + "\x00" + input.Object + "\x00"))
	}
	if rule.SourceSensitive || len(inputs) == 0 {
		_, _ = hash.Write([]byte("source\x00" + sourceRef + "\x00"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validateLayerSafetyAt(ctx context.Context, project *project.Project, worktree string, rule config.LayerRule) error {
	if rule.Mode == config.LayerSkip {
		return nil
	}
	tracked, err := project.Repository.IsTrackedAt(ctx, worktree, rule.Path)
	if err != nil {
		return fmt.Errorf("check tracked path %s: %w", rule.Path, err)
	}
	if tracked {
		if rule.Builtin {
			return errBuiltinNotManaged
		}
		return fmt.Errorf("path %q is tracked by Git; Cambium never overlays tracked paths", rule.Path)
	}
	if project.Config.RequireIgnoredLayers && !rule.AllowUnignored {
		ignored, err := project.Repository.IsIgnoredAt(ctx, worktree, rule.Path)
		if err != nil {
			return fmt.Errorf("check ignored path %s: %w", rule.Path, err)
		}
		if !ignored {
			if rule.Builtin {
				return errBuiltinNotManaged
			}
			return fmt.Errorf("path %q is not ignored by Git; add it to .gitignore or set allow_unignored explicitly", rule.Path)
		}
	}
	path := filepath.Join(worktree, filepath.FromSlash(rule.Path))
	if _, statErr := os.Lstat(path); statErr == nil && !rule.AllowUnignored {
		unignored, err := project.Repository.UnignoredUntrackedAt(ctx, worktree, rule.Path)
		if err != nil {
			return fmt.Errorf("inspect unignored files beneath %s: %w", rule.Path, err)
		}
		if len(unignored) > 0 {
			if rule.Builtin {
				return errBuiltinNotManaged
			}
			preview := unignored
			if len(preview) > 3 {
				preview = preview[:3]
			}
			return fmt.Errorf("path %q contains untracked files that Git does not ignore (%s); refine .gitignore or set allow_unignored explicitly", rule.Path, strings.Join(preview, ", "))
		}
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 && rule.Mode != config.LayerShare {
		return fmt.Errorf("managed path %q is a symlink; use share explicitly or materialize it first", rule.Path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func runRuleCommand(ctx context.Context, project *project.Project, dir string, argv []string, kind, path string) error {
	if len(argv) == 0 {
		return nil
	}
	if !project.Config.AllowPolicyCommands {
		return fmt.Errorf("%s command for environment path %s is disabled; set allow_policy_commands in %s only after reviewing the repository policy", kind, path, config.Filename)
	}
	_, err := project.Runner.Run(ctx, execx.Command{Dir: dir, Name: argv[0], Args: argv[1:]})
	if err != nil {
		return fmt.Errorf("%s environment path %s with %v: %w", kind, path, argv, err)
	}
	return nil
}

func loadLayer(root string) (LayerSnapshot, bool) {
	if _, err := os.Stat(filepath.Join(root, "ready")); err != nil {
		return LayerSnapshot{}, false
	}
	bytes, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	if err != nil {
		return LayerSnapshot{}, false
	}
	var layer LayerSnapshot
	if json.Unmarshal(bytes, &layer) != nil || layer.Version != layerVersion {
		return LayerSnapshot{}, false
	}
	layer.Rule.Builtin = layer.RuleBuiltin
	layer.Rule.Origin = layer.RuleOrigin
	if layer.Present && (layer.Rule.Mode == config.LayerClone || layer.Rule.Mode == config.LayerSeed) {
		if _, err := os.Lstat(filepath.Join(root, "payload")); err != nil {
			return LayerSnapshot{}, false
		}
		layer.Payload = filepath.Join(root, "payload")
	}
	return layer, true
}

func ensureAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("managed path destination already exists: %s", path)
}

func removeIfPresent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func layerLogicalBytes(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
