package workspace

import (
	"context"
	"fmt"
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
	Workspace string            `json:"workspace"`
	SourceRef string            `json:"source_ref"`
	Ready     bool              `json:"ready"`
	Missing   []string          `json:"missing,omitempty"`
	Paths     []EnvironmentPath `json:"paths"`
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
	ready, missing := environmentReadiness(layers)
	value.LayerIDs = ids
	value.EnvironmentReady = ready
	value.EnvironmentMissing = missing
	value.LastUsedAt = time.Now().UTC()
	if err := m.Registry.Save(value); err != nil {
		return EnvironmentExplanation{}, err
	}
	return explainLayers(value.Name, sourceRef, ready, missing, layers), nil
}

func (m *Manager) ExplainEnvironment(ctx context.Context, name string) (EnvironmentExplanation, error) {
	value, err := m.Load(ctx, name, true)
	if err != nil {
		return EnvironmentExplanation{}, err
	}
	layers, err := native.LoadLayers(m.Project, value.LayerIDs)
	if err != nil && len(value.LayerIDs) > 0 {
		return EnvironmentExplanation{}, fmt.Errorf("load workspace environment receipts: %w", err)
	}
	sourceRef := value.SpeculativeRoot
	if sourceRef == "" {
		sourceRef, err = m.Project.Repository.ResolveCommitAt(ctx, value.Path, "HEAD")
		if err != nil {
			return EnvironmentExplanation{}, fmt.Errorf("resolve workspace HEAD: %w", err)
		}
	}
	return explainLayers(value.Name, sourceRef, value.EnvironmentReady, value.EnvironmentMissing, layers), nil
}

func explainLayers(name, sourceRef string, ready bool, missing []string, layers []native.LayerSnapshot) EnvironmentExplanation {
	value := EnvironmentExplanation{Workspace: name, SourceRef: sourceRef, Ready: ready, Missing: append([]string(nil), missing...)}
	for _, layer := range layers {
		value.Paths = append(value.Paths, EnvironmentPath{
			ID: layer.ID, Name: layer.Rule.Name, Path: layer.Rule.Path,
			Policy: string(layer.Rule.Mode), Present: layer.Present,
			Validated: layer.Validated, Expected: layer.Expected, Origin: layer.Rule.Origin,
		})
	}
	return value
}
