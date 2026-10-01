package model

import "time"

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
	Version            int             `json:"version"`
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Status             WorkspaceStatus `json:"status"`
	RepositoryRoot     string          `json:"repository_root"`
	CommonGitDir       string          `json:"common_git_dir"`
	BaseCommit         string          `json:"base_commit"`
	BaseRef            string          `json:"base_ref"`
	Branch             string          `json:"branch"`
	Path               string          `json:"path"`
	Requested          Materializer    `json:"requested_materializer"`
	CloneMode          CloneMode       `json:"clone_mode"`
	PreparedIndex      bool            `json:"prepared_index"`
	Ephemeral          bool            `json:"ephemeral"`
	CreatedAt          time.Time       `json:"created_at"`
	LastUsedAt         time.Time       `json:"last_used_at"`
	LayerIDs           []string        `json:"layer_ids,omitempty"`
	EnvironmentReady   bool            `json:"environment_ready"`
	EnvironmentMissing []string        `json:"environment_missing,omitempty"`
	SpeculativeRoot    string          `json:"speculative_root,omitempty"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
