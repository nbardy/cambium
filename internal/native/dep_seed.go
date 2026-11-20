package native

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/environment"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/fsx"
	"github.com/nbardy/cambium/internal/lockfile"
	"github.com/nbardy/cambium/internal/model"
)

const SeedRootEnv = "CAMBIUM_SEED_ROOT"

var (
	errNoSeedArtifacts      = errors.New("no compiled registry or git dependency units")
	errSeedCompilerMismatch = errors.New("target was built with a different rustc")
)

func SeedRoot() (string, error) {
	if value := os.Getenv(SeedRootEnv); value != "" {
		return filepath.Abs(value)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "cambium-seed"), nil
}

type Toolchain struct {
	Release string
	Host    string
	Info    string
	Rustc   string
}

func pinnedRustup(runner execx.Runner, dir string) (string, error) {
	pinned := os.Getenv("RUSTUP_TOOLCHAIN") != ""
	for _, name := range []string{"rust-toolchain.toml", "rust-toolchain"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			pinned = true
		}
	}
	if !pinned {
		return "", nil
	}
	return runner.LookPath("rustup")
}

func detectToolchain(ctx context.Context, runner execx.Runner, dir string) (Toolchain, error) {
	rustc := os.Getenv("RUSTC")
	if rustc == "" {
		rustc = "rustc"
		rustup, err := pinnedRustup(runner, dir)
		if err != nil {
			return Toolchain{}, err
		}
		if rustup != "" {
			result, err := runner.Run(ctx, execx.Command{Dir: dir, Name: rustup, Args: []string{"which", "rustc"}})
			if err != nil {
				return Toolchain{}, err
			}
			rustc = strings.TrimSpace(result.Stdout)
		}
	}
	result, err := runner.Run(ctx, execx.Command{Dir: dir, Name: rustc, Args: []string{"-vV"}})
	if err != nil {
		return Toolchain{}, fmt.Errorf("detect rustc in %s: %w", dir, err)
	}
	toolchain := Toolchain{Info: strings.TrimSpace(result.Stdout), Rustc: rustc}
	for _, line := range strings.Split(result.Stdout, "\n") {
		switch key, value, _ := strings.Cut(line, ": "); key {
		case "release":
			toolchain.Release = value
		case "host":
			toolchain.Host = value
		}
	}
	if toolchain.Release == "" || toolchain.Host == "" {
		return Toolchain{}, errors.New("rustc -vV did not report its release and host")
	}
	return toolchain, nil
}

type ProfileSeed struct {
	Profile           string `json:"profile"`
	Target            string `json:"target"`
	Units             int    `json:"units"`
	Files             int    `json:"files"`
	DroppedLocalUnits int    `json:"dropped_local_units"`
}

type SeedPublication struct {
	Release  string        `json:"release"`
	Seed     string        `json:"seed"`
	Snapshot string        `json:"snapshot"`
	Profiles []ProfileSeed `json:"profiles"`
}

type seedRecord struct {
	ProfileSeed
	Payload string `json:"payload"`
}

func (p ProfileSeed) key() string {
	return seedDigest([]byte(p.Target + "\x00" + p.Profile))
}

type depSeed struct {
	target    string
	lockRoot  string
	root      string
	lockHash  string
	toolchain Toolchain
	locals    map[string]struct{}
	sourced   map[string]struct{}
}

func seedDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func resolveDepSeed(ctx context.Context, cloner Cloner, target string) (depSeed, model.SeedReason, error) {
	lock := cargoLockfile(target)
	content, err := os.ReadFile(lock)
	if err != nil {
		return depSeed{}, model.SeedLockUnavailable, fmt.Errorf("read %s: %w", lock, err)
	}
	locals, sourced, err := parseCargoLock(bytes.NewReader(content))
	if err != nil {
		return depSeed{}, model.SeedLockUnavailable, fmt.Errorf("read %s: %w", lock, err)
	}
	if len(sourced) == 0 {
		return depSeed{}, model.SeedNoSourceBuild, errors.New("Cargo.lock lists no registry or Git dependencies")
	}
	for name := range locals {
		if _, both := sourced[name]; both {
			return depSeed{}, model.SeedCaptureFailed, fmt.Errorf("package %q has both local and sourced entries in Cargo.lock", name)
		}
	}
	toolchain, err := detectToolchain(ctx, cloner.runner(), filepath.Dir(target))
	if err != nil {
		return depSeed{}, model.SeedToolchainUnavailable, err
	}
	root, err := SeedRoot()
	if err != nil {
		return depSeed{}, model.SeedCaptureFailed, err
	}
	lockHash := seedDigest(content)
	lockRoot := filepath.Join(root, "v2", lockHash)
	return depSeed{target: target, lockRoot: lockRoot, root: filepath.Join(lockRoot, seedDigest([]byte(toolchain.Info))), lockHash: lockHash, toolchain: toolchain, locals: locals, sourced: sourced}, "", nil
}

// The store is shared by repositories, so the layer lock lives beside the
// shared entries. Readers hold it until their clone of the generation ends.
func (s depSeed) lock(ctx context.Context) (*lockfile.Lock, error) {
	return lockfile.AcquireContext(ctx, filepath.Join(s.lockRoot, "layer-"+s.lockHash+".lock"))
}

func (s depSeed) failure(err error) {
	reason := model.SeedCaptureFailed
	if errors.Is(err, errSeedCompilerMismatch) {
		reason = model.SeedToolchainMismatch
	}
	detail, _, _ := strings.Cut(err.Error(), "\n")
	data, _ := json.Marshal(model.SeedResult{Reason: reason, Detail: detail})
	_ = fsx.WriteFileAtomic(filepath.Join(s.root, "failure"), data, 0o644)
}

func readSeedRecord(root string) (seedRecord, bool) {
	data, err := os.ReadFile(filepath.Join(root, "metadata.json"))
	var record seedRecord
	if err != nil || json.Unmarshal(data, &record) != nil || record.Units <= 0 || record.Files <= 0 {
		return seedRecord{}, false
	}
	profile := filepath.FromSlash(record.Profile)
	if !filepath.IsLocal(profile) || filepath.Clean(profile) != profile || profile == "." || record.key() != filepath.Base(root) ||
		!strings.HasPrefix(record.Payload, ".payload-") || filepath.Base(record.Payload) != record.Payload {
		return seedRecord{}, false
	}
	payload := filepath.Join(root, record.Payload)
	if info, err := os.Lstat(payload); err != nil || !info.IsDir() {
		return seedRecord{}, false
	}
	for _, dir := range []string{".fingerprint", "deps"} {
		path := filepath.Join(payload, dir)
		if info, err := os.Lstat(path); err != nil || !info.IsDir() {
			return seedRecord{}, false
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) == 0 {
			return seedRecord{}, false
		}
		if dir == "deps" && !hasSeedArtifact(entries) {
			return seedRecord{}, false
		}
	}
	return record, true
}

func (s depSeed) capture(ctx context.Context, cloner Cloner, onlyMissing bool) (publication SeedPublication, err error) {
	if _, err := os.Lstat(s.target); errors.Is(err, os.ErrNotExist) {
		return publication, fmt.Errorf("%s: %w", s.target, errNoSeedArtifacts)
	}
	lock, err := s.lock(ctx)
	if err != nil {
		return publication, err
	}
	defer lock.Release()
	defer func() {
		if err != nil && !errors.Is(err, errNoSeedArtifacts) {
			s.failure(err)
		}
	}()
	release, err := holdCargoIdle(s.target)
	if err != nil {
		return publication, err
	}
	defer release()
	if err := s.checkCompiler(); err != nil {
		return publication, err
	}
	profiles, _, err := cargoProfileDirs(s.target)
	if err != nil {
		return publication, err
	}
	publication = SeedPublication{Release: s.toolchain.Release, Seed: s.root, Snapshot: s.root}
	for _, dir := range profiles {
		profile, err := s.captureProfile(ctx, cloner, dir, onlyMissing)
		if err != nil {
			return publication, err
		}
		if profile.Units > 0 && profile.Files > 0 {
			publication.Profiles = append(publication.Profiles, profile)
		}
	}
	if len(publication.Profiles) == 0 {
		return publication, fmt.Errorf("%s: %w", s.target, errNoSeedArtifacts)
	}
	_ = os.Remove(filepath.Join(s.root, "failure"))
	return publication, nil
}

// Cargo caches the compiler probe alongside the target. Check it when
// available so a toolchain change after a build cannot label the old target
// as the new compiler. Cargo also verifies fingerprints of individual units.
func (s depSeed) checkCompiler() error {
	data, err := os.ReadFile(filepath.Join(s.target, ".rustc_info.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var probe struct {
		Outputs map[string]struct{ Stdout string }
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	found := false
	for _, output := range probe.Outputs {
		info := strings.TrimSpace(output.Stdout)
		if strings.HasPrefix(info, "rustc ") && strings.Contains(info, "\nrelease: ") {
			if info == s.toolchain.Info {
				return nil
			}
			found = true
		}
	}
	if found {
		return fmt.Errorf("%w than %s", errSeedCompilerMismatch, s.toolchain.Release)
	}
	return nil
}

func (s depSeed) captureProfile(ctx context.Context, cloner Cloner, dir string, onlyMissing bool) (ProfileSeed, error) {
	relative, err := filepath.Rel(s.target, dir)
	if err != nil {
		return ProfileSeed{}, err
	}
	triple := s.toolchain.Host
	if parent := filepath.Dir(relative); parent != "." {
		triple = parent
	}
	layout := ProfileSeed{Target: triple, Profile: filepath.ToSlash(relative)}
	root := filepath.Join(s.root, layout.key())
	previous, exists := readSeedRecord(root)
	if onlyMissing && exists {
		return previous.ProfileSeed, nil
	}
	if !exists {
		// An interrupted or externally damaged entry owns neither a valid
		// payload nor a reusable receipt. Keep it aside for inspection.
		if _, err := os.Lstat(root); err == nil {
			aside, err := os.MkdirTemp(s.root, ".incomplete-")
			if err != nil {
				return ProfileSeed{}, err
			}
			if err := os.Rename(root, aside); err != nil {
				_ = os.Remove(aside)
				return ProfileSeed{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return ProfileSeed{}, err
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return ProfileSeed{}, err
	}
	staging, err := os.MkdirTemp(root, ".payload-")
	if err != nil {
		return ProfileSeed{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
			if !exists {
				_ = os.Remove(root)
			}
		}
	}()
	profile, err := seedProfile(ctx, cloner.runner(), dir, staging, layout, s.sourced, s.locals)
	if err != nil || profile.Units == 0 || profile.Files == 0 {
		return profile, err
	}
	current, err := os.ReadFile(cargoLockfile(s.target))
	if err != nil {
		return ProfileSeed{}, err
	}
	if seedDigest(current) != s.lockHash {
		return ProfileSeed{}, errors.New("Cargo.lock changed during seed capture")
	}
	record, err := json.Marshal(seedRecord{ProfileSeed: profile, Payload: filepath.Base(staging)})
	if err != nil {
		return ProfileSeed{}, err
	}
	if err := fsx.WriteFileAtomic(filepath.Join(root, "metadata.json"), record, 0o644); err != nil {
		// A directory sync can fail after metadata points at the new generation.
		current, ok := readSeedRecord(root)
		published = ok && current.Payload == filepath.Base(staging)
		return ProfileSeed{}, err
	}
	published = true
	if exists {
		_ = os.RemoveAll(filepath.Join(root, previous.Payload))
	}
	return profile, nil
}

var seedUnitFile = regexp.MustCompile(`-([0-9a-f]{16})(?:\.|$)`)
var cargoUnitFull = regexp.MustCompile(`^(.+)-([0-9a-f]{16})$`)

// seedProfile stages one profile directory: the fingerprint directories of
// registry/git packages, their build-script directories, and the deps files
// carrying one of their unit hashes.
func seedProfile(ctx context.Context, runner execx.Runner, source, destination string, profile ProfileSeed, sourced, locals map[string]struct{}) (ProfileSeed, error) {
	entries, err := os.ReadDir(filepath.Join(source, ".fingerprint"))
	if err != nil {
		return ProfileSeed{}, err
	}
	hashes := map[string]struct{}{}
	var fingerprints, builds []string
	droppedLocal := 0
	for _, entry := range entries {
		match := cargoUnitFull.FindStringSubmatch(entry.Name())
		if match == nil || !entry.IsDir() {
			continue
		}
		if _, ok := sourced[match[1]]; ok {
			hashes[match[2]] = struct{}{}
			fingerprints = append(fingerprints, filepath.Join(source, ".fingerprint", entry.Name()))
			build := filepath.Join(source, "build", entry.Name())
			if info, err := os.Lstat(build); err == nil && info.IsDir() {
				builds = append(builds, build)
			}
		} else if _, ok := locals[match[1]]; ok {
			droppedLocal++
		}
	}
	var deps []string
	var artifacts []os.DirEntry
	depEntries, err := os.ReadDir(filepath.Join(source, "deps"))
	if err != nil {
		return ProfileSeed{}, err
	}
	for _, entry := range depEntries {
		if !entry.Type().IsRegular() {
			continue
		}
		if match := seedUnitFile.FindStringSubmatch(entry.Name()); match != nil {
			if _, ok := hashes[match[1]]; ok {
				deps = append(deps, filepath.Join(source, "deps", entry.Name()))
				artifacts = append(artifacts, entry)
			}
		}
	}
	if !hasSeedArtifact(artifacts) {
		return profile, nil
	}
	for _, part := range []struct {
		sources []string
		name    string
	}{{fingerprints, ".fingerprint"}, {builds, "build"}, {deps, "deps"}} {
		if err := cloneInto(ctx, runner, part.sources, filepath.Join(destination, part.name)); err != nil {
			return ProfileSeed{}, err
		}
	}
	profile.Units, profile.Files, profile.DroppedLocalUnits = len(hashes), len(deps), droppedLocal
	return profile, nil
}

func hasSeedArtifact(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) != ".d" {
			return true
		}
	}
	return false
}

// cloneInto copies sources into directory with one clone primitive per
// platform (APFS clonefile on macOS, reflink on Linux), in batches.
func cloneInto(ctx context.Context, runner execx.Runner, sources []string, directory string) error {
	if len(sources) == 0 {
		return nil
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	var flags []string
	switch runtime.GOOS {
	case "darwin":
		flags = []string{"-c", "-R", "-p"}
	case "linux":
		flags = []string{"--reflink=auto", "-R", "-p"}
	default:
		return fmt.Errorf("no clone primitive for platform %s", runtime.GOOS)
	}
	for start := 0; start < len(sources); start += 400 {
		end := min(start+400, len(sources))
		args := append(append([]string{}, flags...), sources[start:end]...)
		args = append(args, directory)
		if _, err := runner.Run(ctx, execx.Command{Name: "cp", Args: args}); err != nil {
			return err
		}
	}
	return nil
}

func applyDepSeed(ctx context.Context, cloner Cloner, workspace string, rule config.LayerRule, requireCoW bool) (model.SeedResult, error) {
	cold := func(reason model.SeedReason, detail string) (model.SeedResult, error) {
		return model.SeedResult{Path: rule.Path, Reason: reason, Detail: detail}, ctx.Err()
	}
	destination := filepath.Join(workspace, filepath.FromSlash(rule.Path))
	if _, err := os.Lstat(destination); err == nil {
		return cold(model.SeedDestinationExists, "a seed only starts an absent path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return cold(model.SeedCloneFailed, err.Error())
	}
	seed, reason, err := resolveDepSeed(ctx, cloner, destination)
	if err != nil {
		return cold(reason, err.Error())
	}
	lock, err := seed.lock(ctx)
	if err != nil {
		return cold(model.SeedCaptureFailed, err.Error())
	}
	defer lock.Release()
	profiles, err := os.ReadDir(seed.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cold(model.SeedCaptureFailed, err.Error())
	}
	var records []seedRecord
	var sources []string
	incomplete := false
	for _, profile := range profiles {
		if !profile.IsDir() || strings.HasPrefix(profile.Name(), ".") {
			continue
		}
		root := filepath.Join(seed.root, profile.Name())
		record, ok := readSeedRecord(root)
		if !ok {
			incomplete = true
			continue
		}
		records = append(records, record)
		sources = append(sources, filepath.Join(root, record.Payload))
	}
	if len(records) == 0 {
		if failure, err := os.ReadFile(filepath.Join(seed.root, "failure")); err == nil {
			var result model.SeedResult
			if json.Unmarshal(failure, &result) == nil && result.Reason != "" {
				return cold(result.Reason, result.Detail)
			}
			return cold(model.SeedCaptureFailed, "seed failure record is unreadable")
		}
		if incomplete {
			return cold(model.SeedCaptureFailed, "seed has an incomplete payload; run cambium seed warm "+rule.Path)
		}
		toolchains, err := os.ReadDir(seed.lockRoot)
		if err != nil {
			return cold(model.SeedCaptureFailed, err.Error())
		}
		for _, compiler := range toolchains {
			if !compiler.IsDir() || compiler.Name() == filepath.Base(seed.root) {
				continue
			}
			entries, _ := os.ReadDir(filepath.Join(seed.lockRoot, compiler.Name()))
			for _, entry := range entries {
				if _, ok := readSeedRecord(filepath.Join(seed.lockRoot, compiler.Name(), entry.Name())); ok {
					return cold(model.SeedToolchainMismatch, "this Cargo.lock has a seed for a different compiler; run cambium seed warm "+rule.Path)
				}
			}
		}
		return cold(model.SeedNoSourceBuild, "no dependency build for this Cargo.lock and rustc "+seed.toolchain.Release+"; run cambium seed warm "+rule.Path)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return cold(model.SeedCloneFailed, err.Error())
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".cambium-seed-")
	if err != nil {
		return cold(model.SeedCloneFailed, err.Error())
	}
	defer os.RemoveAll(staging)
	units := 0
	for i, record := range records {
		if _, err := cloner.ClonePath(ctx, sources[i], filepath.Join(staging, filepath.FromSlash(record.Profile)), requireCoW); err != nil {
			return cold(model.SeedCloneFailed, err.Error())
		}
		units += record.Units
	}
	if err := os.Rename(staging, destination); err != nil {
		return cold(model.SeedCloneFailed, err.Error())
	}
	return model.SeedResult{Path: rule.Path, Reason: model.SeedCloned, Detail: fmt.Sprintf("%d dependency units for rustc %s", units, seed.toolchain.Release)}, nil
}

func (b *Backend) seedRules(ctx context.Context, worktree, commit string) ([]config.LayerRule, error) {
	return environment.ResolveRulesAt(ctx, b.Project.Repository, commit, func() bool {
		return b.Cloner.Probe(ctx, worktree).Supported
	})
}

func (b *Backend) harvestDepSeeds(ctx context.Context, worktree string) {
	commit, err := b.Project.Repository.ResolveCommitAt(ctx, worktree, "HEAD")
	if err != nil {
		return
	}
	rules, err := b.seedRules(ctx, worktree, commit)
	if err != nil {
		return
	}
	for _, rule := range rules {
		if !rule.IsDepSeed() {
			continue
		}
		target := filepath.Join(worktree, filepath.FromSlash(rule.Path))
		info, err := os.Lstat(target)
		if err != nil || !info.IsDir() || validateLayerSafetyAt(ctx, b.Project, worktree, rule) != nil {
			continue
		}
		seed, _, err := resolveDepSeed(ctx, b.Cloner, target)
		if err == nil {
			_, _ = seed.capture(ctx, b.Cloner, true)
		}
	}
}
