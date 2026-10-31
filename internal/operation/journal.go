package operation

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nbardy/cambium/internal/fsx"
)

type Kind string

type Stage string

const (
	KindCreate Kind = "create"
	KindRemove Kind = "remove"

	StagePlanned            Stage = "planned"
	StageWorktreeRegistered Stage = "worktree_registered"
	StageMaterialized       Stage = "materialized"
	StageLayersApplied      Stage = "layers_applied"
	StageMetadataSaved      Stage = "metadata_saved"
	StageWorktreeRemoved    Stage = "worktree_removed"
)

type Record struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	Kind         Kind      `json:"kind"`
	Stage        Stage     `json:"stage"`
	Name         string    `json:"name"`
	Branch       string    `json:"branch"`
	Path         string    `json:"path"`
	BaseCommit   string    `json:"base_commit,omitempty"`
	WorkspaceID  string    `json:"workspace_id,omitempty"`
	WorktreeDir  string    `json:"worktree_git_dir,omitempty"`
	LayerIDs     []string  `json:"layer_ids,omitempty"`
	Force        bool      `json:"force,omitempty"`
	DeleteBranch bool      `json:"delete_branch,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Journal struct {
	Root string
}

func New(root string) Journal { return Journal{Root: root} }

func (j Journal) Begin(kind Kind, name, branch, path, baseCommit string) (Record, error) {
	id, err := newID()
	if err != nil {
		return Record{}, err
	}
	now := time.Now().UTC()
	record := Record{
		Version:    1,
		ID:         id,
		Kind:       kind,
		Stage:      StagePlanned,
		Name:       name,
		Branch:     branch,
		Path:       path,
		BaseCommit: baseCommit,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := j.Save(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (j Journal) Advance(record *Record, stage Stage) error {
	if record == nil {
		return errors.New("operation record is nil")
	}
	record.Stage = stage
	record.UpdatedAt = time.Now().UTC()
	return j.Save(*record)
}

func (j Journal) Save(record Record) error {
	if record.Version == 0 {
		record.Version = 1
	}
	if record.ID == "" {
		return errors.New("operation id cannot be empty")
	}
	if err := os.MkdirAll(j.Root, 0o755); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	return fsx.WriteFileAtomic(j.path(record.ID), bytes, 0o600)
}

func (j Journal) Delete(id string) error {
	return fsx.RemoveFile(j.path(id))
}

// Load lets recovery re-read an operation after taking its workspace lock.
// A snapshot returned by List may already have completed or advanced by then.
func (j Journal) Load(id string) (Record, error) {
	bytes, err := os.ReadFile(j.path(id))
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(bytes, &record); err != nil {
		return Record{}, fmt.Errorf("parse operation %s: %w", id, err)
	}
	if record.Version != 1 {
		return Record{}, fmt.Errorf("operation %s has unsupported version %d", id, record.Version)
	}
	if record.ID != id {
		return Record{}, fmt.Errorf("operation %s has mismatched id %q", id, record.ID)
	}
	return record, nil
}

func (j Journal) List() ([]Record, error) {
	entries, err := os.ReadDir(j.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-len(".json")]
		record, err := j.Load(id)
		if errors.Is(err, os.ErrNotExist) {
			// Another process can finish between ReadDir and the read.
			continue
		}
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, k int) bool {
		if records[i].CreatedAt.Equal(records[k].CreatedAt) {
			return records[i].ID < records[k].ID
		}
		return records[i].CreatedAt.Before(records[k].CreatedAt)
	})
	return records, nil
}

func (j Journal) path(id string) string { return filepath.Join(j.Root, id+".json") }

func newID() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
