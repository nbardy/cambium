// Package speculative stores immutable dirty-worktree checkpoints in Git's
// existing Merkle object graph. Live agent workspaces remain ordinary native
// linked worktrees; checkpoint commits live only under hidden Cambium refs.
package speculative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/gitx"
	"github.com/nbardy/cambium/internal/lockfile"
)

const (
	rootMessage       = "cambium speculative root v1\n"
	rootIdentityName  = "Cambium Speculative Root"
	rootIdentityEmail = "roots@cambium.invalid"
	rootUnixTime      = "946684800"
	rootDate          = "2000-01-01T00:00:00Z"
	refVersion        = 1
)

var objectIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type ID string

func (id ID) String() string { return string(id) }
func (id ID) IsZero() bool   { return id == "" }

func ParseID(value string) (ID, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !objectIDPattern.MatchString(value) {
		return "", fmt.Errorf("invalid Git object id %q", value)
	}
	return ID(value), nil
}

func (id ID) MarshalText() ([]byte, error) { return []byte(id), nil }
func (id *ID) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*id = ""
		return nil
	}
	parsed, err := ParseID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

type Root struct {
	ID         ID     `json:"id"`
	BaseCommit string `json:"base_commit"`
	Tree       string `json:"tree"`
}

type Ref struct {
	Version         int       `json:"version"`
	Name            string    `json:"name"`
	Root            ID        `json:"root"`
	Parent          ID        `json:"parent,omitempty"`
	SourceWorkspace string    `json:"source_workspace,omitempty"`
	Message         string    `json:"message,omitempty"`
	EnvironmentIDs  []string  `json:"environment_ids,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type Checkpoint struct {
	Root         Root     `json:"root"`
	Ref          Ref      `json:"ref"`
	ChangedPaths []string `json:"changed_paths"`
	Attempts     int      `json:"attempts"`
}

type Change struct {
	Status   string `json:"status"`
	Path     string `json:"path"`
	FromPath string `json:"from_path,omitempty"`
}

type DiffResult struct {
	Left        Root     `json:"left"`
	Right       Root     `json:"right"`
	BaseChanged bool     `json:"base_changed"`
	Changes     []Change `json:"changes"`
	Engine      string   `json:"engine"`
}

type GCResult struct {
	Protected       []ID          `json:"protected"`
	ReleasedRoots   []ID          `json:"released_roots"`
	RemovedMetadata []string      `json:"removed_metadata"`
	RetainedRoots   int           `json:"retained_roots"`
	OlderThan       time.Duration `json:"older_than"`
	DryRun          bool          `json:"dry_run"`
	Note            string        `json:"note"`
}

type rootMetadata struct {
	Version        int       `json:"version"`
	Root           ID        `json:"root"`
	Base           string    `json:"base_commit"`
	Tree           string    `json:"tree"`
	Parents        []ID      `json:"parents,omitempty"`
	EnvironmentIDs []string  `json:"environment_ids,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type Store struct {
	Repository gitx.Repository
	StateDir   string
	NamesDir   string
	RootsDir   string
	LocksDir   string
}

func Open(repository gitx.Repository, stateDir, locksDir string) (*Store, error) {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	store := &Store{
		Repository: repository,
		StateDir:   filepath.Clean(absolute),
		NamesDir:   filepath.Join(absolute, "names"),
		RootsDir:   filepath.Join(absolute, "roots"),
		LocksDir:   locksDir,
	}
	for _, path := range []string{store.NamesDir, store.RootsDir, locksDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Acquire(ctx context.Context) (*lockfile.Lock, error) {
	return lockfile.AcquireContext(ctx, filepath.Join(s.LocksDir, "speculative-git.lock"))
}

// Capture snapshots the visible source state of worktree. It uses a private
// copy of the worktree index, so the user's staging area is never changed.
// Untracked non-ignored files are included; ignored environment layers are not.
func (s *Store) Capture(ctx context.Context, worktree string, ref Ref) (Checkpoint, error) {
	if err := validateRefName(ref.Name); err != nil {
		return Checkpoint{}, err
	}
	if !ref.Parent.IsZero() {
		if _, err := s.GetRoot(ctx, ref.Parent); err != nil {
			return Checkpoint{}, fmt.Errorf("resolve parent root: %w", err)
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		root, paths, retry, err := s.captureAttempt(ctx, worktree)
		if err != nil {
			return Checkpoint{}, err
		}
		if retry {
			continue
		}
		ref.Root = root.ID
		ref.Version = refVersion
		if ref.CreatedAt.IsZero() {
			ref.CreatedAt = time.Now().UTC()
		}
		if err := s.SaveRef(ctx, root, ref); err != nil {
			return Checkpoint{}, err
		}
		return Checkpoint{Root: root, Ref: ref, ChangedPaths: paths, Attempts: attempt}, nil
	}
	return Checkpoint{}, errors.New("workspace kept changing during checkpoint; retry when writes are briefly quiescent")
}

func (s *Store) captureAttempt(ctx context.Context, worktree string) (Root, []string, bool, error) {
	beforeHead, err := s.Repository.ResolveCommitAt(ctx, worktree, "HEAD")
	if err != nil {
		return Root{}, nil, false, err
	}
	beforeStatus, err := s.statusSignature(ctx, worktree)
	if err != nil {
		return Root{}, nil, false, err
	}
	indexPath, err := s.Repository.GitPath(ctx, worktree, "index")
	if err != nil {
		return Root{}, nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return Root{}, nil, false, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(indexPath), "cambium-spec-index-*")
	if err != nil {
		return Root{}, nil, false, err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return Root{}, nil, false, err
	}
	_ = os.Remove(temporaryPath)
	defer os.Remove(temporaryPath)
	defer os.Remove(temporaryPath + ".lock")
	if _, err := os.Stat(indexPath); err == nil {
		if err := fsx.CopyFileAtomic(indexPath, temporaryPath, 0o600); err != nil {
			return Root{}, nil, false, err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		env := map[string]string{"GIT_INDEX_FILE": temporaryPath}
		if _, err := s.Repository.RunAt(ctx, worktree, env, "read-tree", "HEAD"); err != nil {
			return Root{}, nil, false, err
		}
	} else {
		return Root{}, nil, false, err
	}

	env := map[string]string{"GIT_INDEX_FILE": temporaryPath}
	if _, err := s.Repository.RunAt(ctx, worktree, env, "add", "-A", "--", "."); err != nil {
		return Root{}, nil, false, fmt.Errorf("snapshot worktree into temporary index: %w", err)
	}
	treeResult, err := s.Repository.RunAt(ctx, worktree, env, "write-tree")
	if err != nil {
		return Root{}, nil, false, fmt.Errorf("write speculative Git tree: %w", err)
	}
	tree := strings.TrimSpace(treeResult.Stdout)
	if _, err := ParseID(tree); err != nil {
		return Root{}, nil, false, fmt.Errorf("Git returned invalid tree id: %w", err)
	}

	// The temporary index must still describe the working tree. This catches
	// modifications/deletions after git add. A second untracked scan catches
	// files created after the snapshot.
	diffResult, diffErr := s.Repository.RunAt(ctx, worktree, env, "diff-files", "--quiet", "--ignore-submodules=none", "--")
	if diffErr != nil && diffResult.ExitCode != 1 {
		return Root{}, nil, false, diffErr
	}
	others, err := s.Repository.RunAt(ctx, worktree, env, "ls-files", "-z", "--others", "--exclude-standard", "--")
	if err != nil {
		return Root{}, nil, false, err
	}
	afterHead, err := s.Repository.ResolveCommitAt(ctx, worktree, "HEAD")
	if err != nil {
		return Root{}, nil, false, err
	}
	afterStatus, err := s.statusSignature(ctx, worktree)
	if err != nil {
		return Root{}, nil, false, err
	}
	if diffResult.ExitCode == 1 || others.Stdout != "" || beforeHead != afterHead || beforeStatus != afterStatus {
		return Root{}, nil, true, nil
	}

	root, err := s.createRootCommit(ctx, beforeHead, tree)
	if err != nil {
		return Root{}, nil, false, err
	}
	changes, err := s.diffCommits(ctx, beforeHead, root.ID.String())
	if err != nil {
		return Root{}, nil, false, err
	}
	paths := changedPaths(changes)
	return root, paths, false, nil
}

func (s *Store) createRootCommit(ctx context.Context, base, tree string) (Root, error) {
	env := map[string]string{
		"GIT_AUTHOR_NAME":     rootIdentityName,
		"GIT_AUTHOR_EMAIL":    rootIdentityEmail,
		"GIT_AUTHOR_DATE":     rootDate,
		"GIT_COMMITTER_NAME":  rootIdentityName,
		"GIT_COMMITTER_EMAIL": rootIdentityEmail,
		"GIT_COMMITTER_DATE":  rootDate,
	}
	result, err := s.Repository.Runner.Run(ctx, execx.Command{
		Dir:   s.Repository.Root,
		Env:   env,
		Name:  "git",
		Args:  []string{"--git-dir=" + s.Repository.CommonGitDir, "commit-tree", tree, "-p", base},
		Stdin: strings.NewReader(rootMessage),
	})
	if err != nil {
		return Root{}, fmt.Errorf("create deterministic speculative commit: %w", err)
	}
	id, err := ParseID(strings.TrimSpace(result.Stdout))
	if err != nil {
		return Root{}, err
	}
	root := Root{ID: id, BaseCommit: base, Tree: tree}
	verified, err := s.GetRoot(ctx, id)
	if err != nil {
		return Root{}, err
	}
	if verified.BaseCommit != base || verified.Tree != tree {
		return Root{}, errors.New("speculative root verification mismatch")
	}
	return root, nil
}

func (s *Store) GetRoot(ctx context.Context, id ID) (Root, error) {
	parsed, err := ParseID(id.String())
	if err != nil {
		return Root{}, err
	}
	resolved, err := s.Repository.ResolveCommit(ctx, parsed.String())
	if err != nil {
		return Root{}, err
	}
	if resolved != parsed.String() {
		return Root{}, fmt.Errorf("object %s resolved unexpectedly to %s", parsed, resolved)
	}
	parentsResult, err := s.Repository.RunCommon(ctx, nil, "show", "-s", "--format=%P", parsed.String())
	if err != nil {
		return Root{}, err
	}
	parents := strings.Fields(strings.TrimSpace(parentsResult.Stdout))
	if len(parents) != 1 {
		return Root{}, fmt.Errorf("object %s is not a single-parent speculative root", parsed)
	}
	identityResult, err := s.Repository.RunCommon(ctx, nil, "show", "-s", "--format=%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%B", parsed.String())
	if err != nil {
		return Root{}, err
	}
	identity := strings.SplitN(identityResult.Stdout, "\x00", 7)
	if len(identity) != 7 || identity[0] != rootIdentityName || identity[1] != rootIdentityEmail ||
		identity[2] != rootUnixTime || identity[3] != rootIdentityName || identity[4] != rootIdentityEmail ||
		identity[5] != rootUnixTime || strings.TrimRight(identity[6], "\n") != strings.TrimRight(rootMessage, "\n") {
		return Root{}, fmt.Errorf("commit %s is not a Cambium speculative root", parsed)
	}
	treeResult, err := s.Repository.RunCommon(ctx, nil, "rev-parse", parsed.String()+"^{tree}")
	if err != nil {
		return Root{}, err
	}
	return Root{ID: parsed, BaseCommit: parents[0], Tree: strings.TrimSpace(treeResult.Stdout)}, nil
}

func (s *Store) SaveRef(ctx context.Context, root Root, ref Ref) error {
	if err := validateRefName(ref.Name); err != nil {
		return err
	}
	verified, err := s.GetRoot(ctx, root.ID)
	if err != nil {
		return err
	}
	if verified.BaseCommit != root.BaseCommit || verified.Tree != root.Tree {
		return errors.New("root metadata does not match Git commit")
	}
	if !ref.Parent.IsZero() {
		if _, err := s.GetRoot(ctx, ref.Parent); err != nil {
			return fmt.Errorf("validate parent root: %w", err)
		}
	}
	ref.Version = refVersion
	ref.Root = root.ID
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}
	lock, err := s.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lock.Release()
	if ref.Parent == root.ID {
		// Re-checkpointing identical state yields the same deterministic root.
		// A self edge adds no lineage information, so normalize it away.
		ref.Parent = ""
	}
	if err := s.pinRoot(ctx, root, ref.EnvironmentIDs, ref.Parent); err != nil {
		return err
	}
	if !ref.Parent.IsZero() {
		parent, err := s.GetRoot(ctx, ref.Parent)
		if err != nil {
			return err
		}
		if err := s.pinRoot(ctx, parent, nil); err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := fsx.WriteFileAtomic(s.nameMetadataPath(ref.Name), encoded, 0o644); err != nil {
		return err
	}
	if _, err := s.Repository.RunCommon(ctx, nil, "update-ref", "-m", "cambium speculative checkpoint", s.nameRef(ref.Name), root.ID.String()); err != nil {
		return err
	}
	return nil
}

func (s *Store) pinRoot(ctx context.Context, root Root, environmentIDs []string, parents ...ID) error {
	if _, err := s.Repository.RunCommon(ctx, nil, "update-ref", "-m", "cambium pin speculative root", s.rootRef(root.ID), root.ID.String()); err != nil {
		return err
	}
	path := s.rootMetadataPath(root.ID)
	metadata := rootMetadata{Version: refVersion, Root: root.ID, Base: root.BaseCommit, Tree: root.Tree, CreatedAt: time.Now().UTC()}
	if encoded, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			return err
		}
		if metadata.Version != refVersion || metadata.Root != root.ID || metadata.Base != root.BaseCommit || metadata.Tree != root.Tree {
			return fmt.Errorf("root metadata mismatch for %s", root.ID)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	set := make(map[ID]struct{}, len(metadata.Parents)+len(parents))
	for _, parent := range metadata.Parents {
		if !parent.IsZero() && parent != root.ID {
			set[parent] = struct{}{}
		}
	}
	for _, parent := range parents {
		if !parent.IsZero() && parent != root.ID {
			set[parent] = struct{}{}
		}
	}
	metadata.Parents = metadata.Parents[:0]
	for parent := range set {
		metadata.Parents = append(metadata.Parents, parent)
	}
	sort.Slice(metadata.Parents, func(i, j int) bool { return metadata.Parents[i] < metadata.Parents[j] })
	envSet := make(map[string]struct{}, len(metadata.EnvironmentIDs)+len(environmentIDs))
	for _, id := range metadata.EnvironmentIDs {
		if id != "" {
			envSet[id] = struct{}{}
		}
	}
	for _, id := range environmentIDs {
		if id != "" {
			envSet[id] = struct{}{}
		}
	}
	metadata.EnvironmentIDs = metadata.EnvironmentIDs[:0]
	for id := range envSet {
		metadata.EnvironmentIDs = append(metadata.EnvironmentIDs, id)
	}
	sort.Strings(metadata.EnvironmentIDs)
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(encoded, '\n'), 0o644)
}

func (s *Store) Resolve(ctx context.Context, value string) (ID, Root, *Ref, error) {
	if id, err := ParseID(value); err == nil {
		root, err := s.GetRoot(ctx, id)
		return id, root, nil, err
	}
	ref, err := s.LoadRef(ctx, value)
	if err != nil {
		return "", Root{}, nil, fmt.Errorf("resolve root or ref %q: %w", value, err)
	}
	root, err := s.GetRoot(ctx, ref.Root)
	if err != nil {
		return "", Root{}, nil, err
	}
	return ref.Root, root, &ref, nil
}

func (s *Store) LoadRef(ctx context.Context, name string) (Ref, error) {
	if err := validateRefName(name); err != nil {
		return Ref{}, err
	}
	lock, err := s.Acquire(ctx)
	if err != nil {
		return Ref{}, err
	}
	defer lock.Release()
	return s.loadRefLocked(ctx, name)
}

func (s *Store) loadRefLocked(ctx context.Context, name string) (Ref, error) {
	encoded, err := os.ReadFile(s.nameMetadataPath(name))
	if err != nil {
		return Ref{}, err
	}
	var ref Ref
	if err := json.Unmarshal(encoded, &ref); err != nil {
		return Ref{}, err
	}
	if ref.Version != refVersion || ref.Name != name {
		return Ref{}, fmt.Errorf("invalid speculative ref %q", name)
	}
	result, err := s.Repository.RunCommon(ctx, nil, "rev-parse", "--verify", s.nameRef(name)+"^{commit}")
	if err != nil {
		return Ref{}, err
	}
	resolved, err := ParseID(strings.TrimSpace(result.Stdout))
	if err != nil {
		return Ref{}, err
	}
	if resolved != ref.Root {
		return Ref{}, fmt.Errorf("speculative ref %q metadata points to %s but Git ref points to %s", name, ref.Root, resolved)
	}
	if _, err := s.GetRoot(ctx, ref.Root); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func (s *Store) ListRefs(ctx context.Context) ([]Ref, error) {
	lock, err := s.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	entries, err := os.ReadDir(s.NamesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join(s.NamesDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var ref Ref
		if err := json.Unmarshal(encoded, &ref); err != nil {
			return nil, fmt.Errorf("parse speculative metadata %s: %w", entry.Name(), err)
		}
		if ref.Version != refVersion || validateRefName(ref.Name) != nil || filepath.Base(s.nameMetadataPath(ref.Name)) != entry.Name() {
			return nil, fmt.Errorf("invalid speculative metadata %s", entry.Name())
		}
		result, resolveErr := s.Repository.RunCommon(ctx, nil, "rev-parse", "--verify", s.nameRef(ref.Name)+"^{commit}")
		if resolveErr != nil {
			// An interrupted drop may leave metadata behind. GC removes it.
			continue
		}
		resolved, parseErr := ParseID(strings.TrimSpace(result.Stdout))
		if parseErr != nil || resolved != ref.Root {
			return nil, fmt.Errorf("speculative ref %q is inconsistent", ref.Name)
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].CreatedAt.Equal(refs[j].CreatedAt) {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].CreatedAt.After(refs[j].CreatedAt)
	})
	return refs, nil
}

func (s *Store) DropRef(ctx context.Context, name string) error {
	if err := validateRefName(name); err != nil {
		return err
	}
	lock, err := s.Acquire(ctx)
	if err != nil {
		return err
	}
	defer lock.Release()
	if _, err := s.Repository.RunCommon(ctx, nil, "update-ref", "-d", s.nameRef(name)); err != nil {
		return err
	}
	return fsx.RemoveFile(s.nameMetadataPath(name))
}

func (s *Store) Diff(ctx context.Context, leftID, rightID ID) (DiffResult, error) {
	left, err := s.GetRoot(ctx, leftID)
	if err != nil {
		return DiffResult{}, err
	}
	right, err := s.GetRoot(ctx, rightID)
	if err != nil {
		return DiffResult{}, err
	}
	changes, err := s.diffCommits(ctx, left.ID.String(), right.ID.String())
	if err != nil {
		return DiffResult{}, err
	}
	return DiffResult{Left: left, Right: right, BaseChanged: left.BaseCommit != right.BaseCommit, Changes: changes, Engine: "git-merkle-tree"}, nil
}

func (s *Store) ChangesFromBase(ctx context.Context, root Root) ([]Change, error) {
	return s.diffCommits(ctx, root.BaseCommit, root.ID.String())
}

func (s *Store) Restore(ctx context.Context, root Root, worktree string) (int, error) {
	changes, err := s.ChangesFromBase(ctx, root)
	if err != nil {
		return 0, err
	}
	paths := changedPaths(changes)
	if len(paths) == 0 {
		return 0, nil
	}
	gitDir, err := s.Repository.GitPath(ctx, worktree, "cambium-restore")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		return 0, err
	}
	file, err := os.CreateTemp(filepath.Dir(gitDir), "cambium-spec-paths-*")
	if err != nil {
		return 0, err
	}
	pathspec := file.Name()
	defer os.Remove(pathspec)
	for _, path := range paths {
		if strings.ContainsRune(path, '\x00') {
			_ = file.Close()
			return 0, errors.New("Git returned a path containing NUL")
		}
		if _, err := file.WriteString(path + "\x00"); err != nil {
			_ = file.Close()
			return 0, err
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return 0, err
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	if _, err := s.Repository.RunAt(ctx, worktree, nil,
		"restore", "--source="+root.ID.String(), "--worktree",
		"--pathspec-from-file="+pathspec, "--pathspec-file-nul"); err != nil {
		return 0, fmt.Errorf("restore speculative root %s: %w", root.ID, err)
	}
	return len(paths), nil
}

func (s *Store) GC(ctx context.Context, protected []ID, olderThan time.Duration, dryRun bool) (GCResult, error) {
	lock, err := s.Acquire(ctx)
	if err != nil {
		return GCResult{}, err
	}
	defer lock.Release()
	refs, err := s.listRefsLocked(ctx)
	if err != nil {
		return GCResult{}, err
	}
	marked := make(map[ID]struct{}, len(refs)+len(protected))
	for _, id := range protected {
		if !id.IsZero() {
			if err := s.markRootGraph(ctx, id, marked); err != nil {
				return GCResult{}, err
			}
		}
	}
	for _, ref := range refs {
		if err := s.markRootGraph(ctx, ref.Root, marked); err != nil {
			return GCResult{}, err
		}
		if !ref.Parent.IsZero() {
			if err := s.markRootGraph(ctx, ref.Parent, marked); err != nil {
				return GCResult{}, err
			}
		}
	}
	result := GCResult{OlderThan: olderThan, DryRun: dryRun, Note: "released hidden root refs; Git reclaims unreachable objects according to its own gc/prune policy"}
	for id := range marked {
		result.Protected = append(result.Protected, id)
	}
	sort.Slice(result.Protected, func(i, j int) bool { return result.Protected[i] < result.Protected[j] })

	rootRefs, err := s.listRootRefs(ctx)
	if err != nil {
		return GCResult{}, err
	}
	now := time.Now()
	for _, id := range rootRefs {
		if _, ok := marked[id]; ok {
			result.RetainedRoots++
			continue
		}
		metadata, metadataErr := s.loadRootMetadata(id)
		if metadataErr != nil && !errors.Is(metadataErr, os.ErrNotExist) {
			return GCResult{}, metadataErr
		}
		created := time.Time{}
		if metadataErr == nil {
			created = metadata.CreatedAt
		}
		if olderThan > 0 && !created.IsZero() && now.Sub(created) < olderThan {
			result.RetainedRoots++
			continue
		}
		result.ReleasedRoots = append(result.ReleasedRoots, id)
		if !dryRun {
			if _, err := s.Repository.RunCommon(ctx, nil, "update-ref", "-d", s.rootRef(id)); err != nil {
				return GCResult{}, err
			}
			if err := fsx.RemoveFile(s.rootMetadataPath(id)); err != nil {
				return GCResult{}, err
			}
		}
	}

	entries, err := os.ReadDir(s.NamesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return GCResult{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.NamesDir, entry.Name())
		encoded, readErr := os.ReadFile(path)
		if readErr != nil {
			return GCResult{}, readErr
		}
		var ref Ref
		if json.Unmarshal(encoded, &ref) == nil && ref.Name != "" {
			if _, err := s.Repository.RunCommon(ctx, nil, "rev-parse", "--verify", s.nameRef(ref.Name)+"^{commit}"); err == nil {
				continue
			}
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return GCResult{}, statErr
		}
		if olderThan > 0 && now.Sub(info.ModTime()) < olderThan {
			continue
		}
		result.RemovedMetadata = append(result.RemovedMetadata, entry.Name())
		if !dryRun {
			if err := fsx.RemoveFile(path); err != nil {
				return GCResult{}, err
			}
		}
	}
	return result, nil
}

func (s *Store) markRootGraph(ctx context.Context, id ID, marked map[ID]struct{}) error {
	if id.IsZero() {
		return nil
	}
	if _, ok := marked[id]; ok {
		return nil
	}
	if _, err := s.GetRoot(ctx, id); err != nil {
		return err
	}
	marked[id] = struct{}{}
	metadata, err := s.loadRootMetadata(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, parent := range metadata.Parents {
		if err := s.markRootGraph(ctx, parent, marked); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) statusSignature(ctx context.Context, worktree string) (string, error) {
	result, err := s.Repository.RunAt(ctx, worktree, nil, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	return result.Stdout, nil
}

func (s *Store) diffCommits(ctx context.Context, left, right string) ([]Change, error) {
	result, err := s.Repository.RunCommon(ctx, nil,
		"diff-tree", "--no-commit-id", "--name-status", "-z", "-r", "--find-renames=50%", left, right)
	if err != nil {
		return nil, err
	}
	return parseNameStatus(result.Stdout)
}

func parseNameStatus(value string) ([]Change, error) {
	parts := strings.Split(value, "\x00")
	changes := make([]Change, 0, len(parts)/2)
	for index := 0; index < len(parts); {
		if parts[index] == "" {
			index++
			continue
		}
		status := parts[index]
		index++
		if index >= len(parts) || parts[index] == "" {
			return nil, errors.New("malformed Git name-status output")
		}
		first := parts[index]
		index++
		change := Change{Status: status, Path: first}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if index >= len(parts) || parts[index] == "" {
				return nil, errors.New("malformed Git rename/copy output")
			}
			change.FromPath = first
			change.Path = parts[index]
			index++
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return changes[i].FromPath < changes[j].FromPath
		}
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

func changedPaths(changes []Change) []string {
	set := make(map[string]struct{}, len(changes)*2)
	for _, change := range changes {
		if change.Path != "" {
			set[change.Path] = struct{}{}
		}
		if change.FromPath != "" {
			set[change.FromPath] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func (s *Store) listRefsLocked(ctx context.Context) ([]Ref, error) {
	entries, err := os.ReadDir(s.NamesDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join(s.NamesDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var ref Ref
		if err := json.Unmarshal(encoded, &ref); err != nil || ref.Version != refVersion || validateRefName(ref.Name) != nil {
			continue
		}
		result, resolveErr := s.Repository.RunCommon(ctx, nil, "rev-parse", "--verify", s.nameRef(ref.Name)+"^{commit}")
		if resolveErr != nil {
			continue
		}
		resolved, parseErr := ParseID(strings.TrimSpace(result.Stdout))
		if parseErr == nil && resolved == ref.Root {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func (s *Store) listRootRefs(ctx context.Context) ([]ID, error) {
	result, err := s.Repository.RunCommon(ctx, nil, "for-each-ref", "--format=%(objectname)", "refs/cambium/speculative/roots/")
	if err != nil {
		return nil, err
	}
	var ids []ID
	for _, line := range strings.Fields(result.Stdout) {
		id, err := ParseID(line)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (s *Store) loadRootMetadata(id ID) (rootMetadata, error) {
	encoded, err := os.ReadFile(s.rootMetadataPath(id))
	if err != nil {
		return rootMetadata{}, err
	}
	var metadata rootMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return rootMetadata{}, err
	}
	if metadata.Version != refVersion || metadata.Root != id {
		return rootMetadata{}, fmt.Errorf("invalid root metadata for %s", id)
	}
	return metadata, nil
}

// ReferencedEnvironmentIDs returns all prepared environment layers pinned by
// speculative roots, including roots that are retained only as lineage parents.
func (s *Store) ReferencedEnvironmentIDs() ([]string, error) {
	entries, err := os.ReadDir(s.RootsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join(s.RootsDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var metadata rootMetadata
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			return nil, err
		}
		for _, id := range metadata.EnvironmentIDs {
			if id != "" {
				set[id] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func validateRefName(name string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsRune(name, '\x00') || len(name) > 240 {
		return fmt.Errorf("invalid speculative ref name %q", name)
	}
	return nil
}

func nameKey(name string) string {
	hash := sha256.Sum256([]byte(name))
	return hex.EncodeToString(hash[:])
}

func (s *Store) nameRef(name string) string {
	return "refs/cambium/speculative/names/" + nameKey(name)
}

func (s *Store) rootRef(id ID) string {
	return "refs/cambium/speculative/roots/" + id.String()
}

func (s *Store) nameMetadataPath(name string) string {
	return filepath.Join(s.NamesDir, nameKey(name)+".json")
}

func (s *Store) rootMetadataPath(id ID) string {
	return filepath.Join(s.RootsDir, id.String()+".json")
}
