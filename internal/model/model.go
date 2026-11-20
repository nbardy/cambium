package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Materializer selects how tracked files are populated into a linked Git
// worktree. Auto prefers filesystem CoW and otherwise delegates checkout to
// Git. Copy exists primarily for diagnostics and benchmarks.
type Materializer string

const (
	MaterializerAuto Materializer = "auto"
	MaterializerCoW  Materializer = "cow"
	MaterializerGit  Materializer = "git"
	MaterializerCopy Materializer = "copy"
)

type CloneMode string

const (
	CloneModeAPFS        CloneMode = "apfs-clone"
	CloneModeReflink     CloneMode = "reflink"
	CloneModeCopy        CloneMode = "copy"
	CloneModeGitCheckout CloneMode = "git-checkout"
)

type WorkspaceStatus string

const (
	WorkspaceReady  WorkspaceStatus = "ready"
	WorkspaceBroken WorkspaceStatus = "broken"
)

type Workspace struct {
	Version               int             `json:"version"`
	ID                    string          `json:"id"`
	Name                  string          `json:"name"`
	Status                WorkspaceStatus `json:"status"`
	RepositoryRoot        string          `json:"repository_root"`
	CommonGitDir          string          `json:"common_git_dir"`
	WorktreeGitDir        string          `json:"worktree_git_dir,omitempty"`
	BaseCommit            string          `json:"base_commit"`
	BaseRef               string          `json:"base_ref"`
	Branch                string          `json:"branch"`
	Path                  string          `json:"path"`
	Requested             Materializer    `json:"requested_materializer"`
	CloneMode             CloneMode       `json:"clone_mode"`
	PreparedIndex         bool            `json:"prepared_index"`
	Ephemeral             bool            `json:"ephemeral"`
	CreatedAt             time.Time       `json:"created_at"`
	LastUsedAt            time.Time       `json:"last_used_at"`
	LayerIDs              []string        `json:"layer_ids,omitempty"`
	EnvironmentReady      bool            `json:"environment_ready"`
	EnvironmentMissing    []string        `json:"environment_missing,omitempty"`
	EnvironmentUnverified []string        `json:"environment_unverified,omitempty"`
	DependencySeeds       []SeedResult    `json:"dependency_seeds,omitempty"`
	SpeculativeRoot       string          `json:"speculative_root,omitempty"`
}

type SeedReason string

const (
	SeedCloned               SeedReason = "seeded"
	SeedNoSourceBuild        SeedReason = "no_source_build"
	SeedCaptureFailed        SeedReason = "capture_failed"
	SeedToolchainMismatch    SeedReason = "toolchain_mismatch"
	SeedToolchainUnavailable SeedReason = "toolchain_unavailable"
	SeedLockUnavailable      SeedReason = "lock_unavailable"
	SeedDestinationExists    SeedReason = "destination_exists"
	SeedCloneFailed          SeedReason = "clone_failed"
	SeedLegacyUnknown        SeedReason = "legacy_unknown"
)

// SeedResult records what happened when the workspace was materialized.
type SeedResult struct {
	Path   string     `json:"path"`
	Reason SeedReason `json:"reason"`
	Detail string     `json:"detail,omitempty"`
}

func (s SeedResult) String() string {
	if s.Reason == SeedCloned {
		return fmt.Sprintf("%s: seeded: %s", s.Path, s.Detail)
	}
	return fmt.Sprintf("%s: cold (%s): %s", s.Path, s.Reason, s.Detail)
}

func (s *SeedResult) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		// v0.10 stored these observations as display strings.
		var line string
		if err := json.Unmarshal(data, &line); err != nil {
			return err
		}
		path, detail, _ := strings.Cut(line, ": ")
		*s = SeedResult{Path: path, Reason: SeedLegacyUnknown, Detail: detail}
		switch {
		case strings.HasPrefix(detail, "seeded from "):
			s.Reason, s.Detail = SeedCloned, strings.TrimPrefix(detail, "seeded ")
		case strings.HasPrefix(detail, "unseeded: no seed for rustc "):
			s.Reason = SeedNoSourceBuild
		case strings.HasPrefix(detail, "unseeded: seed was built by a different rustc "):
			s.Reason = SeedToolchainMismatch
		}
		return nil
	}
	type record SeedResult
	return json.Unmarshal(data, (*record)(s))
}

type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
