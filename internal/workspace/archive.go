package workspace

import (
	"context"
	"strings"

	"github.com/nbardy/cambium/internal/gitx"
)

type ArchiveResult struct {
	Archived bool   `json:"archived"`
	Already  bool   `json:"already,omitempty"`
	SHA      string `json:"sha"`
	Tag      string `json:"tag"`
}

// Archive replaces a branch with a tag that is verified on the remote before
// the branch is deleted. It fails closed: every step that cannot prove the
// commit survives elsewhere refuses and leaves the branch in place. Pushes run
// with the caller's environment (SSH_AUTH_SOCK, credential helpers).
func Archive(ctx context.Context, repo gitx.Repository, branch, tag, remote string) (ArchiveResult, error) {
	branchRef := "refs/heads/" + branch
	tagRef := "refs/tags/" + tag
	worktrees, err := repo.ListWorktrees(ctx)
	if err != nil {
		return ArchiveResult{}, err
	}
	for _, worktree := range worktrees {
		if worktree.Branch == branch {
			return ArchiveResult{}, refuse(RefuseCheckedOut, "branch %q is checked out at %s", branch, worktree.Path)
		}
	}
	tip, err := repo.RunCommon(ctx, nil, "rev-parse", "--verify", "-q", branchRef+"^{commit}")
	if err != nil {
		// Idempotent rerun: the branch is gone and its tag already reached the remote.
		remoteSHA, lsErr := remoteTagCommit(ctx, repo, remote, tagRef)
		if lsErr == nil && remoteSHA != "" {
			return ArchiveResult{Archived: true, Already: true, SHA: remoteSHA, Tag: tag}, nil
		}
		return ArchiveResult{}, refuse(RefuseMissing, "branch %q does not exist and %s has no tag %q", branch, remote, tag)
	}
	sha := strings.TrimSpace(tip.Stdout)
	existing, err := repo.RunCommon(ctx, nil, "rev-parse", "--verify", "-q", tagRef+"^{commit}")
	if err == nil {
		if tagged := strings.TrimSpace(existing.Stdout); tagged != sha {
			return ArchiveResult{}, refuse(RefuseTagConflict, "tag %q points at %s, branch %q is at %s", tag, tagged, branch, sha)
		}
	} else if _, err := repo.RunCommon(ctx, nil, "update-ref", tagRef, sha, ""); err != nil {
		return ArchiveResult{}, refuse(RefuseTagConflict, "create tag %q: %v", tag, err)
	}
	if _, err := repo.RunCommon(ctx, nil, "push", remote, tagRef+":"+tagRef); err != nil {
		return ArchiveResult{}, refuse(RefusePushUnverified, "push %s to %s: %v", tagRef, remote, err)
	}
	remoteSHA, err := remoteTagCommit(ctx, repo, remote, tagRef)
	if err != nil {
		return ArchiveResult{}, refuse(RefusePushUnverified, "ls-remote %s %s: %v", remote, tagRef, err)
	}
	if remoteSHA != sha {
		return ArchiveResult{}, refuse(RefusePushUnverified, "%s %s resolves to %q, expected %s", remote, tagRef, remoteSHA, sha)
	}
	// Compare-and-delete: a commit made after we read the tip is not covered
	// by the pushed tag, so the branch must survive.
	if _, err := repo.RunCommon(ctx, nil, "update-ref", "-d", branchRef, sha); err != nil {
		return ArchiveResult{}, refuse(RefuseMoved, "branch %q moved from %s; kept (tag %q is pushed): %v", branch, sha, tag, err)
	}
	return ArchiveResult{Archived: true, SHA: sha, Tag: tag}, nil
}

// remoteTagCommit returns the commit a remote tag resolves to (peeled for an
// annotated tag), or "" when the remote has no such tag.
func remoteTagCommit(ctx context.Context, repo gitx.Repository, remote, tagRef string) (string, error) {
	result, err := repo.RunCommon(ctx, nil, "ls-remote", remote, tagRef, tagRef+"^{}")
	if err != nil {
		return "", err
	}
	direct, peeled := "", ""
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		object, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch name {
		case tagRef:
			direct = object
		case tagRef + "^{}":
			peeled = object
		}
	}
	if peeled != "" {
		return peeled, nil
	}
	return direct, nil
}
