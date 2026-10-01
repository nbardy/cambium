package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
	"github.com/nbardy/cambium/internal/project"
	"github.com/nbardy/cambium/internal/state"
)

type CreateSpec struct {
	Name          string
	Ref           string
	Branch        string
	Path          string
	Ephemeral     bool
	RequireCoW    bool
	Materializer  model.Materializer
	PreparedIndex *bool
}

type Manager struct {
	Project  *project.Project
	Registry state.Registry
	Native   *native.Backend
}

func New(project *project.Project) *Manager {
	return &Manager{
		Project:  project,
		Registry: state.New(project.MetadataDir()),
		Native:   native.New(project),
	}
}

func (m *Manager) Create(ctx context.Context, spec CreateSpec) (model.Workspace, error) {
	return m.Native.Create(ctx, native.CreateSpec{
		Name: spec.Name, Ref: spec.Ref, Branch: spec.Branch, Path: spec.Path,
		Ephemeral: spec.Ephemeral, RequireCoW: spec.RequireCoW,
		Materializer: spec.Materializer, PreparedIndex: spec.PreparedIndex,
	})
}

func (m *Manager) Prepare(ctx context.Context, ref string, materializer model.Materializer, requireCoW bool) (native.PrepareResult, error) {
	return m.Native.Prepare(ctx, ref, materializer, requireCoW)
}

func (m *Manager) Load(ctx context.Context, name string, refresh bool) (model.Workspace, error) {
	value, err := m.Registry.Load(name)
	if err != nil {
		return model.Workspace{}, err
	}
	if !refresh {
		return value, nil
	}
	registered, regErr := m.Project.Repository.IsWorktreeRegistered(ctx, value.Path)
	if regErr != nil {
		return model.Workspace{}, regErr
	}
	_, statErr := os.Stat(value.Path)
	ready := registered && statErr == nil
	status := model.WorkspaceReady
	if !ready {
		status = model.WorkspaceBroken
	}
	if value.Status != status {
		value.Status = status
		if err := m.Registry.Save(value); err != nil {
			return model.Workspace{}, err
		}
	}
	return value, nil
}

func (m *Manager) List(ctx context.Context, refresh bool) ([]model.Workspace, error) {
	values, err := m.Registry.List()
	if err != nil {
		return nil, err
	}
	if !refresh {
		return values, nil
	}
	for index := range values {
		updated, loadErr := m.Load(ctx, values[index].Name, true)
		if loadErr == nil {
			values[index] = updated
		}
	}
	return values, nil
}

func (m *Manager) Remove(ctx context.Context, name string, force, deleteBranch bool) error {
	return m.Native.Remove(ctx, name, force, deleteBranch)
}

func (m *Manager) Touch(name string) error { return m.Registry.Touch(name) }

func (m *Manager) Recover(ctx context.Context, dryRun bool) ([]native.RecoveryAction, error) {
	return m.Native.Recover(ctx, dryRun)
}

func (m *Manager) PruneCaches(ctx context.Context, olderThan time.Duration, dryRun bool) (native.CachePruneResult, error) {
	return m.Native.PruneCaches(ctx, olderThan, dryRun)
}

type GCOptions struct {
	EphemeralOnly  bool
	OlderThan      time.Duration
	Force          bool
	DeleteBranches bool
	DryRun         bool
}

type GCSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type GCResult struct {
	Selected []string `json:"selected"`
	Removed  []string `json:"removed"`
	Skipped  []GCSkip `json:"skipped"`
}

func (m *Manager) GC(ctx context.Context, options GCOptions) (GCResult, error) {
	if options.OlderThan < 0 {
		return GCResult{}, errors.New("gc age cannot be negative")
	}
	values, err := m.Registry.List()
	if err != nil {
		return GCResult{}, err
	}
	cutoff := time.Now().UTC().Add(-options.OlderThan)
	result := GCResult{}
	for _, value := range values {
		if options.EphemeralOnly && !value.Ephemeral {
			continue
		}
		lastUsed := value.LastUsedAt
		if lastUsed.IsZero() {
			lastUsed = value.CreatedAt
		}
		if options.OlderThan > 0 && lastUsed.After(cutoff) {
			continue
		}
		result.Selected = append(result.Selected, value.Name)
		if options.DryRun {
			continue
		}
		if err := m.Remove(ctx, value.Name, options.Force, options.DeleteBranches); err != nil {
			result.Skipped = append(result.Skipped, GCSkip{Name: value.Name, Reason: err.Error()})
			continue
		}
		result.Removed = append(result.Removed, value.Name)
	}
	return result, nil
}

func (m *Manager) Doctor(ctx context.Context) []model.DoctorCheck {
	checks := make([]model.DoctorCheck, 0, 8)
	if path, err := m.Project.Runner.LookPath("git"); err != nil {
		checks = append(checks, model.DoctorCheck{Name: "git", Status: "error", Message: err.Error()})
	} else {
		checks = append(checks, model.DoctorCheck{Name: "git", Status: "ok", Message: path})
	}
	checks = append(checks, model.DoctorCheck{Name: "repository", Status: "ok", Message: m.Project.Repository.Root})
	if err := m.Project.Config.Validate(); err != nil {
		checks = append(checks, model.DoctorCheck{Name: "config", Status: "error", Message: err.Error()})
	} else {
		checks = append(checks, model.DoctorCheck{Name: "config", Status: "ok", Message: filepath.Join(m.Project.Repository.Root, ".cambium.json")})
	}
	probePath := filepath.Join(m.Project.StateDir, ".doctor-write")
	if err := os.WriteFile(probePath, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		checks = append(checks, model.DoctorCheck{Name: "state", Status: "error", Message: err.Error()})
	} else {
		_ = os.Remove(probePath)
		checks = append(checks, model.DoctorCheck{Name: "state", Status: "ok", Message: m.Project.StateDir})
	}
	capability := m.Native.Cloner.ProbeBetween(ctx, m.Project.BaselinesDir(), m.Project.WorktreesDir())
	if capability.Supported {
		checks = append(checks, model.DoctorCheck{Name: "native-cow", Status: "ok", Message: string(capability.Mode) + ": " + capability.Detail})
	} else {
		status := "warn"
		if m.Project.Config.RequireCoW {
			status = "error"
		}
		checks = append(checks, model.DoctorCheck{Name: "native-cow", Status: status, Message: capability.Detail + "; auto will use Git checkout"})
	}
	operations, err := m.Native.Journal.List()
	if err != nil {
		checks = append(checks, model.DoctorCheck{Name: "operations", Status: "error", Message: err.Error()})
	} else if len(operations) == 0 {
		checks = append(checks, model.DoctorCheck{Name: "operations", Status: "ok", Message: "no interrupted operations"})
	} else {
		checks = append(checks, model.DoctorCheck{Name: "operations", Status: "warn", Message: fmt.Sprintf("%d interrupted operation(s); run cambium recover", len(operations))})
	}
	values, err := m.Registry.List()
	if err != nil {
		checks = append(checks, model.DoctorCheck{Name: "workspaces", Status: "error", Message: err.Error()})
	} else {
		broken := 0
		for _, value := range values {
			registered, regErr := m.Project.Repository.IsWorktreeRegistered(ctx, value.Path)
			if regErr != nil || !registered {
				broken++
				continue
			}
			if _, statErr := os.Stat(value.Path); statErr != nil {
				broken++
			}
		}
		status := "ok"
		message := fmt.Sprintf("%d registered workspace(s)", len(values))
		if broken > 0 {
			status = "warn"
			message = fmt.Sprintf("%d workspace(s), %d broken", len(values), broken)
		}
		checks = append(checks, model.DoctorCheck{Name: "workspaces", Status: status, Message: message})
	}
	return checks
}
