package workspace

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/native"
)

type EnvironmentPath struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Path      string `json:"path"`
	Policy    string `json:"policy"`
	Present   bool   `json:"present"`
	Validated bool   `json:"validated"`
	Expected  bool   `json:"expected"`
	Origin    string `json:"origin,omitempty"`
}

type EnvironmentExplanation struct {
	Workspace  string            `json:"workspace"`
	SourceRef  string            `json:"source_ref"`
	Ready      bool              `json:"ready"`
	Missing    []string          `json:"missing,omitempty"`
	Unverified []string          `json:"unverified,omitempty"`
	Paths      []EnvironmentPath `json:"paths"`
}

func (m *Manager) CaptureEnvironment(ctx context.Context, name, sourceRef string) (EnvironmentExplanation, error) {
	value, err := m.Load(ctx, name, true)
	if err != nil {
		return EnvironmentExplanation{}, err
	}
	if strings.TrimSpace(sourceRef) == "" {
		sourceRef = value.SpeculativeRoot
		if sourceRef == "" {
			sourceRef, err = m.Project.Repository.ResolveCommitAt(ctx, value.Path, "HEAD")
			if err != nil {
				return EnvironmentExplanation{}, fmt.Errorf("resolve workspace HEAD: %w", err)
			}
		}
	}
	layers, err := native.CaptureWorkspaceLayers(ctx, m.Project, value.Path, sourceRef, m.Native.Cloner, m.Project.Config.RequireCoW)
	if err != nil {
		return EnvironmentExplanation{}, err
	}
	ids := make([]string, 0, len(layers))
	for _, layer := range layers {
		ids = append(ids, layer.ID)
	}
	ready, missing, unverified := environmentReadiness(layers)
	value.LayerIDs = ids
	value.EnvironmentReady = ready
	value.EnvironmentMissing = missing
	value.EnvironmentUnverified = unverified
	value.LastUsedAt = time.Now().UTC()
	if err := m.Registry.Save(value); err != nil {
		return EnvironmentExplanation{}, err
	}
	return explainLayers(value.Name, sourceRef, ready, missing, unverified, layers), nil
}

func (m *Manager) ExplainEnvironment(ctx context.Context, name string) (EnvironmentExplanation, error) {
	return m.observeEnvironment(ctx, name, false)
}

// ValidateEnvironment explicitly executes configured validation hooks against
// the live workspace. It records an unready observation even when a hook fails.
func (m *Manager) ValidateEnvironment(ctx context.Context, name string) (EnvironmentExplanation, error) {
	return m.observeEnvironment(ctx, name, true)
}

func (m *Manager) observeEnvironment(ctx context.Context, name string, validate bool) (EnvironmentExplanation, error) {
	value, err := m.Load(ctx, name, true)
	if err != nil {
		return EnvironmentExplanation{}, err
	}
	layers, err := native.LoadLayers(m.Project, value.LayerIDs)
	if err != nil && len(value.LayerIDs) > 0 {
		return EnvironmentExplanation{}, fmt.Errorf("load workspace environment receipts: %w", err)
	}
	// Validation belongs to a workspace observation, not to the shared cache.
	// In particular, a failed explicit validation must not be overwritten by
	// an older successful observation stored with the canonical payload.
	for i := range layers {
		path := layers[i].Rule.Path
		layers[i].Validated = len(layers[i].Rule.Validate) > 0 &&
			!slices.Contains(value.EnvironmentMissing, path) &&
			!slices.Contains(value.EnvironmentUnverified, path)
	}
	sourceRef := value.SpeculativeRoot
	if sourceRef == "" {
		sourceRef, err = m.Project.Repository.ResolveCommitAt(ctx, value.Path, "HEAD")
		if err != nil {
			return EnvironmentExplanation{}, fmt.Errorf("resolve workspace HEAD: %w", err)
		}
	}
	layers, observationErr := native.ObserveWorkspaceLayers(ctx, m.Project, value.Path, layers, validate)
	if layers == nil && observationErr != nil {
		return EnvironmentExplanation{}, observationErr
	}
	ready, missing, unverified := environmentReadiness(layers)
	if validate {
		value.EnvironmentReady = ready
		value.EnvironmentMissing = missing
		value.EnvironmentUnverified = unverified
		value.LastUsedAt = time.Now().UTC()
		if err := m.Registry.Save(value); err != nil {
			return EnvironmentExplanation{}, err
		}
	}
	return explainLayers(value.Name, sourceRef, ready, missing, unverified, layers), observationErr
}

func explainLayers(name, sourceRef string, ready bool, missing, unverified []string, layers []native.LayerSnapshot) EnvironmentExplanation {
	value := EnvironmentExplanation{Workspace: name, SourceRef: sourceRef, Ready: ready, Missing: append([]string(nil), missing...), Unverified: append([]string(nil), unverified...)}
	for _, layer := range layers {
		value.Paths = append(value.Paths, EnvironmentPath{
			ID: layer.ID, Name: layer.Rule.Name, Path: layer.Rule.Path,
			Policy: string(layer.Rule.Mode), Present: layer.Present,
			Validated: layer.Validated, Expected: layer.Expected, Origin: layer.Rule.Origin,
		})
	}
	return value
}
