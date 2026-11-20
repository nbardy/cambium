package workspace

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/inuse"
	"github.com/nbardy/cambium/internal/model"
	"github.com/nbardy/cambium/internal/native"
)

// DefaultMergeRef is the ref a workspace is judged "merged" against when the
// caller names none: origin's default branch, else main or master.
func (m *Manager) DefaultMergeRef(ctx context.Context) (string, error) {
	repo := m.Project.Repository
	if result, err := repo.RunCommon(ctx, nil, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(result.Stdout); ref != "" {
			return ref, nil
		}
	}
	for _, candidate := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := repo.RunCommon(ctx, nil, "rev-parse", "--verify", "-q", candidate+"^{commit}"); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("no default branch found (origin/HEAD, origin/main, origin/master, main, master); pass --merged-into REF")
}

// InUse reports what, if anything, is using the workspace right now.
func (m *Manager) InUse(value model.Workspace) ([]inuse.Holder, error) {
	return inuse.Check(value.Path, m.Project.UseDir(value.ID))
}

// MarkInUse registers the calling process as a user of the workspace until the
// returned function is called.
func (m *Manager) MarkInUse(value model.Workspace) (func(), error) {
	return inuse.Mark(m.Project.UseDir(value.ID))
}

// Status is one workspace's answer to "can I delete this, and what does it cost".
type Status struct {
	Name            string             `json:"name"`
	Repository      string             `json:"repository"`
	Path            string             `json:"path"`
	Branch          string             `json:"branch"`
	Ephemeral       bool               `json:"ephemeral"`
	Missing         bool               `json:"missing"`
	Dirty           int                `json:"dirty_paths"`
	Unpushed        int                `json:"unpushed_commits"` // reachable from HEAD, from no remote-tracking ref and no other branch or tag
	HasCommits      bool               `json:"has_commits"`      // HEAD moved past the base commit
	Merged          bool               `json:"merged"`
	MergedInto      string             `json:"merged_into"`
	InUse           []inuse.Holder     `json:"in_use,omitempty"`
	Size            *fsx.TreeSize      `json:"size,omitempty"`
	LastUsedAt      time.Time          `json:"last_used_at"`
	Errors          []string           `json:"errors,omitempty"`
	DependencySeeds []model.SeedResult `json:"dependency_seeds,omitempty"`
}

// Removable is true only when removal loses nothing: present, clean, nothing
// unpushed, not in use. Merged is reported separately; an unmerged but pushed
// branch is removable and its commits stay on the remote.
func (s Status) Removable() bool {
	return !s.Missing && s.Dirty == 0 && s.Unpushed == 0 && len(s.InUse) == 0 && len(s.Errors) == 0
}

// Statuses reports every workspace of this repository. mergedInto names the
// ref for the merged column ("" selects DefaultMergeRef); withBytes walks each
// tree, which dominates the cost.
func (m *Manager) Statuses(ctx context.Context, mergedInto string, withBytes bool) ([]Status, error) {
	values, err := m.Registry.List()
	if err != nil {
		return nil, err
	}
	if mergedInto == "" {
		mergedInto, err = m.DefaultMergeRef(ctx)
		if err != nil {
			return nil, err
		}
	}
	out := make([]Status, 0, len(values))
	for _, value := range values {
		out = append(out, m.statusOf(ctx, value, mergedInto, withBytes))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) statusOf(ctx context.Context, value model.Workspace, mergedInto string, withBytes bool) Status {
	repo := m.Project.Repository
	status := Status{
		Name: value.Name, Repository: m.Project.PrimaryRoot(), Path: value.Path, Branch: value.Branch,
		Ephemeral: value.Ephemeral, MergedInto: mergedInto, LastUsedAt: value.LastUsedAt,
		DependencySeeds: value.DependencySeeds,
	}
	if _, err := os.Stat(value.Path); err != nil {
		status.Missing = true
		return status
	}
	fail := func(what string, err error) { status.Errors = append(status.Errors, what+": "+err.Error()) }
	if porcelain, err := repo.StatusPorcelain(ctx, value.Path); err != nil {
		fail("status", err)
	} else {
		status.Dirty = len(strings.FieldsFunc(porcelain, func(r rune) bool { return r == '\n' }))
	}
	head, err := repo.ResolveCommitAt(ctx, value.Path, "HEAD")
	if err != nil {
		fail("HEAD", err)
		return status
	}
	status.HasCommits = head != value.BaseCommit
	if count, err := unpushedCommits(ctx, repo, value.Path, value.Branch); err != nil {
		fail("unpushed", err)
	} else {
		status.Unpushed = count
	}
	if err := checkMerged(ctx, repo, head, mergedInto); err == nil {
		status.Merged = true
	} else {
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			fail("merged", err)
		}
	}
	if holders, err := m.InUse(value); err != nil {
		fail("in-use", err)
	} else {
		status.InUse = holders
	}
	if withBytes {
		if size, err := fsx.MeasureTree(value.Path); err != nil {
			fail("size", err)
		} else {
			status.Size = &size
		}
	}
	return status
}

func atoi(text string) int {
	n := 0
	for _, r := range strings.TrimSpace(text) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

type PruneMergedOptions struct {
	Ref          string        // required: remove workspaces whose HEAD is an ancestor of Ref
	DryRun       bool          //
	DeleteBranch bool          // also delete the (merged) branch
	MinIdle      time.Duration // when > 0, a workspace with no commits of its own is selected only if idle this long
}

type PruneMergedResult struct {
	Ref      string   `json:"ref"`
	Selected []string `json:"selected"`
	Removed  []string `json:"removed"`
	Skipped  []GCSkip `json:"skipped"`
}

// PruneMerged removes workspaces whose work is already in Ref. Removal is the
// guarded SafeRemove with RequireMerged, so a dirty, in-progress, in-use or
// unmerged workspace is skipped and reported, never removed.
func (m *Manager) PruneMerged(ctx context.Context, options PruneMergedOptions) (PruneMergedResult, error) {
	if options.Ref == "" {
		return PruneMergedResult{}, errors.New("prune --merged requires a ref")
	}
	values, err := m.Registry.List()
	if err != nil {
		return PruneMergedResult{}, err
	}
	result := PruneMergedResult{Ref: options.Ref}
	for _, value := range values {
		skip := func(reason string) { result.Skipped = append(result.Skipped, GCSkip{Name: value.Name, Reason: reason}) }
		if native.SamePath(value.Path, m.Project.PrimaryRoot()) {
			skip("primary checkout")
			continue
		}
		// Only disposable workspaces are swept; a workspace made without
		// --ephemeral is somebody's long-lived checkout.
		if !value.Ephemeral {
			skip("not ephemeral")
			continue
		}
		if _, err := os.Stat(value.Path); err != nil {
			skip("worktree missing; use forget --missing")
			continue
		}
		head, err := m.Project.Repository.ResolveCommitAt(ctx, value.Path, "HEAD")
		if err != nil {
			skip(err.Error())
			continue
		}
		if err := checkMerged(ctx, m.Project.Repository, head, options.Ref); err != nil {
			var refusal *Refusal
			if !errors.As(err, &refusal) || refusal.Reason != RefuseUnmerged {
				skip(err.Error()) // unmerged is the expected case, not news
			}
			continue
		}
		// A workspace with no commits of its own is "merged" trivially. An explicit
		// `prune --merged` removes it; automatic sweeps pass MinIdle so a workspace
		// another agent just made is not reaped before it has done anything.
		if head == value.BaseCommit && options.MinIdle > 0 {
			lastUsed := value.LastUsedAt
			if lastUsed.IsZero() {
				lastUsed = value.CreatedAt
			}
			if time.Since(lastUsed) < options.MinIdle {
				skip("no commits of its own and used within " + options.MinIdle.String())
				continue
			}
		}
		// The same guards as the real removal, so --dry-run lists only what a real run removes.
		removal := SafeRemoveOptions{DeleteBranch: options.DeleteBranch, RequireMerged: options.Ref}
		removal.DryRun = true
		if _, err := m.SafeRemove(ctx, value.Name, removal); err != nil {
			skip(err.Error())
			continue
		}
		result.Selected = append(result.Selected, value.Name)
		if options.DryRun {
			continue
		}
		removal.DryRun = false
		if _, err := m.SafeRemove(ctx, value.Name, removal); err != nil {
			skip(err.Error())
			continue
		}
		result.Removed = append(result.Removed, value.Name)
	}
	return result, nil
}

// FetchMergeTarget refreshes a "remote/branch" ref before a merged check. It
// is best effort: offline, or a ref that is not remote-tracking, changes
// nothing and the check runs against the ref as it is.
func (m *Manager) FetchMergeTarget(ctx context.Context, ref string) {
	remote, branch, ok := strings.Cut(ref, "/")
	if !ok || remote == "" || branch == "" {
		return
	}
	if _, err := m.Project.Repository.RunCommon(ctx, nil, "remote", "get-url", remote); err != nil {
		return
	}
	_, _ = m.Project.Repository.RunCommon(ctx, nil, "fetch", "--quiet", remote, branch)
}
