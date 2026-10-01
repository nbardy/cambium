package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/model"
)

var ErrWorkspaceNotFound = errors.New("workspace not found")

type Registry struct {
	Root string
}

func New(root string) Registry { return Registry{Root: root} }

func NewID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func (r Registry) Save(workspace model.Workspace) error {
	if workspace.Name == "" {
		return errors.New("workspace name cannot be empty")
	}
	if workspace.Version == 0 {
		workspace.Version = 2
	}
	bytes, err := json.MarshalIndent(workspace, "", "  ")
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	return fsx.WriteFileAtomic(r.path(workspace.Name), bytes, 0o600)
}

func (r Registry) Load(name string) (model.Workspace, error) {
	bytes, err := os.ReadFile(r.path(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.Workspace{}, fmt.Errorf("%w: %q", ErrWorkspaceNotFound, name)
		}
		return model.Workspace{}, err
	}
	var workspace model.Workspace
	if err := json.Unmarshal(bytes, &workspace); err != nil {
		return model.Workspace{}, fmt.Errorf("parse workspace %q: %w", name, err)
	}
	if workspace.Version != 2 {
		return model.Workspace{}, fmt.Errorf("workspace %q has unsupported metadata version %d", name, workspace.Version)
	}
	return workspace, nil
}

func (r Registry) Exists(name string) (bool, error) {
	_, err := os.Stat(r.path(name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (r Registry) List() ([]model.Workspace, error) {
	entries, err := os.ReadDir(r.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	workspaces := make([]model.Workspace, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		bytes, readErr := os.ReadFile(filepath.Join(r.Root, entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		var workspace model.Workspace
		if unmarshalErr := json.Unmarshal(bytes, &workspace); unmarshalErr != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), unmarshalErr)
		}
		workspaces = append(workspaces, workspace)
	}
	sort.Slice(workspaces, func(i, j int) bool {
		if workspaces[i].CreatedAt.Equal(workspaces[j].CreatedAt) {
			return workspaces[i].Name < workspaces[j].Name
		}
		return workspaces[i].CreatedAt.Before(workspaces[j].CreatedAt)
	})
	return workspaces, nil
}

func (r Registry) Touch(name string) error {
	workspace, err := r.Load(name)
	if err != nil {
		return err
	}
	workspace.LastUsedAt = time.Now().UTC()
	return r.Save(workspace)
}

func (r Registry) Delete(name string) error {
	return fsx.RemoveFile(r.path(name))
}

func (r Registry) path(name string) string {
	digest := sha256.Sum256([]byte(name))
	return filepath.Join(r.Root, safeName(name)+"-"+hex.EncodeToString(digest[:6])+".json")
}

func safeName(name string) string {
	var builder strings.Builder
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	value := strings.Trim(builder.String(), "-.")
	if value == "" {
		return "workspace"
	}
	return value
}
