package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nbardy/cambium/internal/config"
	"github.com/nbardy/cambium/internal/environment"
	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/project"
)

// A dependency seed (`policy = "seed"`, `seed = "cargo-deps"`) starts a new
// workspace's Cargo target directory from the compiled units of registry and
// git dependencies, one seed per compiler release, shared by every repository
// and workspace on the machine. It generalizes Keel's scripts/seed.sh.
//
// Only dependency units travel (measured 2026-10-30 in Keel, 2026-11-12 here):
// Cargo's fingerprints for workspace members do not depend on the checkout
// path, so a member unit cloned into another checkout reads Fresh, and the
// binary prints the ORIGIN's CARGO_MANIFEST_DIR. Registry and git sources live
// at the same CARGO_HOME paths in every checkout, so their units are safe.
// Membership is a whitelist: a unit is published only when its
// `.fingerprint/<package>-<hash>` names a package that Cargo.lock lists with a
// source. Anything unidentified stays behind, so the worst outcome is a
// rebuild, never a wrong binary.

// SeedRootEnv overrides the seed store directory.
const SeedRootEnv = "CAMBIUM_SEED_ROOT"

const seedRecordName = "cambium-seed.json"

// How many published snapshots a toolchain keeps: the current one and the one
// before it, so an apply that resolved the old link just before a refresh can
// still finish cloning it.
const seedSnapshotsKept = 2

// SeedRoot is where per-toolchain seeds live: CAMBIUM_SEED_ROOT, else
// ~/Library/Caches/cambium-seed on macOS, else $XDG_CACHE_HOME/cambium-seed.
func SeedRoot() (string, error) {
	if value := os.Getenv(SeedRootEnv); value != "" {
		return value, nil
	}
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Caches", "cambium-seed"), nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "cambium-seed"), nil
}

// Toolchain is the compiler a workspace builds with, as `rustc -vV` reports it
// from inside that workspace (so rust-toolchain.toml applies).
type Toolchain struct {
	Release string
	Info    string
}

var rustcRelease = regexp.MustCompile(`(?m)^release: (\S+)$`)

func detectToolchain(ctx context.Context, runner execx.Runner, dir string) (Toolchain, error) {
	result, err := runner.Run(ctx, execx.Command{Dir: dir, Name: "rustc", Args: []string{"-vV"}})
	if err != nil {
		return Toolchain{}, fmt.Errorf("rustc -vV in %s: %w", dir, err)
	}
	match := rustcRelease.FindStringSubmatch(result.Stdout)
	if match == nil {
		return Toolchain{}, fmt.Errorf("rustc -vV in %s printed no release line", dir)
	}
	return Toolchain{Release: match[1], Info: result.Stdout}, nil
}

// seedRecord is written beside every snapshot.
type seedRecord struct {
	OriginTarget  string        `json:"origin_target"`
	Created       time.Time     `json:"created"`
	Rustc         string        `json:"rustc"`
	Registry      int           `json:"registry_packages"`
	LocalExcluded []string      `json:"local_packages_excluded"`
	Profiles      []ProfileSeed `json:"profiles"`
}

// ProfileSeed counts what one profile directory contributed.
type ProfileSeed struct {
	Profile           string `json:"profile"`
	Units             int    `json:"units"`
	Files             int    `json:"files"`
	DroppedLocalUnits int    `json:"dropped_local_units"`
}

// SeedPublication is the result of `cambium seed refresh`.
type SeedPublication struct {
	Release  string        `json:"release"`
	Seed     string        `json:"seed"`
	Snapshot string        `json:"snapshot"`
	Profiles []ProfileSeed `json:"profiles"`
}

// seedUnitFile matches the unit hash inside a deps file name:
// lib<crate>-<hash>.rlib, <crate>-<hash>.d, <crate>-<hash>.<cgu>.rcgu.o, <bin>-<hash>.
var seedUnitFile = regexp.MustCompile(`-([0-9a-f]{16})(?:\.|$)`)

var cargoUnitFull = regexp.MustCompile(`^(.+)-([0-9a-f]{16})$`)

// RefreshDepSeed publishes the dependency units of the workspace's Cargo
// targets as the seed for its compiler release, replacing the previous one.
// The workspace's rules name which paths are Cargo targets.
func RefreshDepSeed(ctx context.Context, project *project.Project, cloner Cloner, workspacePath string) (SeedPublication, error) {
	commit, err := project.Repository.ResolveCommitAt(ctx, workspacePath, "HEAD")
	if err != nil {
		return SeedPublication{}, err
	}
	rules, err := environment.ResolveRulesAt(ctx, project.Repository, commit)
	if err != nil {
		return SeedPublication{}, err
	}
	var target string
	for _, rule := range rules {
		if rule.IsDepSeed() {
			target = filepath.Join(workspacePath, filepath.FromSlash(rule.Path))
			break
		}
	}
	if target == "" {
		return SeedPublication{}, errors.New(`no path policy with policy = "seed" and seed = "cargo-deps" in this repository`)
	}
	locals, sourced, err := scanCargoLock(cargoLockfile(target))
	if err != nil {
		return SeedPublication{}, fmt.Errorf("read %s: %w", cargoLockfile(target), err)
	}
	for name := range locals {
		if _, both := sourced[name]; both {
			// A shared name would make the fingerprint whitelist ambiguous.
			return SeedPublication{}, fmt.Errorf("package %q is both local and registry in Cargo.lock; refusing to guess", name)
		}
	}
	toolchain, err := detectToolchain(ctx, cloner.runner(), workspacePath)
	if err != nil {
		return SeedPublication{}, err
	}
	root, err := SeedRoot()
	if err != nil {
		return SeedPublication{}, err
	}
	version := filepath.Join(root, toolchain.Release)
	stamp := fmt.Sprintf("%s-%d", time.Now().Format("20060102T150405"), os.Getpid())
	staging := filepath.Join(version, "target."+stamp)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return SeedPublication{}, err
	}
	// Cargo holds its build lock exclusively while it writes; holding it shared
	// while cloning gives a consistent snapshot, and refuses while a build runs.
	release, err := holdCargoIdle(target)
	if err != nil {
		_ = os.RemoveAll(staging)
		return SeedPublication{}, fmt.Errorf("%w; refresh again when it finishes", err)
	}
	defer release()
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	var rootFiles []string
	for _, name := range []string{".rustc_info.json", "CACHEDIR.TAG"} {
		if _, err := os.Lstat(filepath.Join(target, name)); err == nil {
			rootFiles = append(rootFiles, filepath.Join(target, name))
		}
	}
	if err := cloneInto(ctx, cloner.runner(), rootFiles, staging); err != nil {
		return SeedPublication{}, err
	}
	profiles, _, err := cargoProfileDirs(target)
	if err != nil {
		return SeedPublication{}, err
	}
	var seeded []ProfileSeed
	for _, dir := range profiles {
		relative, err := filepath.Rel(target, dir)
		if err != nil {
			return SeedPublication{}, err
		}
		profile, err := seedProfile(ctx, cloner.runner(), dir, filepath.Join(staging, relative), relative, sourced, locals)
		if err != nil {
			return SeedPublication{}, err
		}
		seeded = append(seeded, profile)
	}
	if len(seeded) == 0 {
		return SeedPublication{}, fmt.Errorf("%s has no Cargo profile with a .fingerprint; build the workspace first", target)
	}
	excluded := make([]string, 0, len(locals))
	for name := range locals {
		excluded = append(excluded, name)
	}
	sort.Strings(excluded)
	record := seedRecord{OriginTarget: target, Created: time.Now().UTC(), Rustc: toolchain.Info, Registry: len(sourced), LocalExcluded: excluded, Profiles: seeded}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return SeedPublication{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, seedRecordName), append(encoded, '\n'), 0o644); err != nil {
		return SeedPublication{}, err
	}
	link := filepath.Join(version, "target")
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return SeedPublication{}, fmt.Errorf("%s is a directory, not a seed link; remove it explicitly", link)
	}
	// Atomic replace: readers resolve `target` to the old or the new complete snapshot.
	temporary := filepath.Join(version, fmt.Sprintf(".target.link.%d", os.Getpid()))
	_ = os.Remove(temporary)
	if err := os.Symlink(filepath.Base(staging), temporary); err != nil {
		return SeedPublication{}, err
	}
	if err := os.Rename(temporary, link); err != nil {
		return SeedPublication{}, err
	}
	published = true
	retireOldSnapshots(version)
	return SeedPublication{Release: toolchain.Release, Seed: link, Snapshot: staging, Profiles: seeded}, nil
}

// seedProfile stages one profile directory: the fingerprint directories of
// registry/git packages, their build-script directories, and the deps files
// carrying one of their unit hashes.
func seedProfile(ctx context.Context, runner execx.Runner, source, destination, name string, sourced, locals map[string]struct{}) (ProfileSeed, error) {
	entries, err := os.ReadDir(filepath.Join(source, ".fingerprint"))
	if err != nil {
		return ProfileSeed{}, err
	}
	hashes := map[string]struct{}{}
	var fingerprints, builds []string
	droppedLocal := 0
	for _, entry := range entries {
		match := cargoUnitFull.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		if _, ok := sourced[match[1]]; ok {
			hashes[match[2]] = struct{}{}
			fingerprints = append(fingerprints, filepath.Join(source, ".fingerprint", entry.Name()))
			build := filepath.Join(source, "build", entry.Name())
			if info, err := os.Stat(build); err == nil && info.IsDir() {
				builds = append(builds, build)
			}
		} else if _, ok := locals[match[1]]; ok {
			droppedLocal++
		}
	}
	var deps []string
	depEntries, err := os.ReadDir(filepath.Join(source, "deps"))
	if err != nil {
		return ProfileSeed{}, err
	}
	for _, entry := range depEntries {
		if match := seedUnitFile.FindStringSubmatch(entry.Name()); match != nil {
			if _, ok := hashes[match[1]]; ok {
				deps = append(deps, filepath.Join(source, "deps", entry.Name()))
			}
		}
	}
	for _, part := range []struct {
		sources []string
		name    string
	}{{fingerprints, ".fingerprint"}, {builds, "build"}, {deps, "deps"}} {
		if err := cloneInto(ctx, runner, part.sources, filepath.Join(destination, part.name)); err != nil {
			return ProfileSeed{}, err
		}
	}
	return ProfileSeed{Profile: name, Units: len(hashes), Files: len(deps), DroppedLocalUnits: droppedLocal}, nil
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

// retireOldSnapshots keeps the newest seedSnapshotsKept snapshots. Snapshots
// are APFS clones, so removing one frees only its unique blocks.
func retireOldSnapshots(version string) {
	snapshots, _ := filepath.Glob(filepath.Join(version, "target.*"))
	sort.Strings(snapshots) // names start with a sortable timestamp
	for len(snapshots) > seedSnapshotsKept {
		_ = os.RemoveAll(snapshots[0])
		snapshots = snapshots[1:]
	}
}

// applyDepSeed starts a workspace's target directory from the seed for its
// compiler. Every outcome is a named line for the workspace record: "seeded"
// or "unseeded: <reason>". An absent seed is not an error (the first build
// compiles dependencies itself) but it is said aloud.
func applyDepSeed(ctx context.Context, cloner Cloner, workspacePath string, rule config.LayerRule, requireCoW bool) (string, error) {
	destination := filepath.Join(workspacePath, filepath.FromSlash(rule.Path))
	unseeded := func(format string, args ...any) (string, error) {
		return fmt.Sprintf("%s: unseeded: %s", rule.Path, fmt.Sprintf(format, args...)), nil
	}
	if _, err := os.Lstat(destination); err == nil {
		return unseeded("%s already exists; a seed only starts an absent path", rule.Path)
	}
	// The seed is pruned against this workspace's own Cargo.lock, so a package
	// that is local here is rebuilt here whatever the seed's origin thought.
	lockfile := cargoLockfile(destination)
	locals, _, err := scanCargoLock(lockfile)
	if err != nil {
		return unseeded("no readable Cargo.lock beside the path (%v)", err)
	}
	toolchain, err := detectToolchain(ctx, cloner.runner(), workspacePath)
	if err != nil {
		return unseeded("%v", err)
	}
	root, err := SeedRoot()
	if err != nil {
		return unseeded("%v", err)
	}
	link := filepath.Join(root, toolchain.Release, "target")
	snapshot, err := filepath.EvalSymlinks(link)
	if err != nil {
		return unseeded("no seed for rustc %s; `cambium seed refresh` publishes one", toolchain.Release)
	}
	recorded, err := os.ReadFile(filepath.Join(snapshot, seedRecordName))
	var record seedRecord
	if err != nil || json.Unmarshal(recorded, &record) != nil {
		return unseeded("seed %s has no readable %s", snapshot, seedRecordName)
	}
	if strings.TrimSpace(record.Rustc) != strings.TrimSpace(toolchain.Info) {
		// Same release number, different build (commit or host): Cargo would
		// discard the units anyway.
		return unseeded("seed was built by a different rustc than %s", toolchain.Release)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if _, err := cloner.ClonePath(ctx, snapshot, destination, requireCoW); err != nil {
		_ = os.RemoveAll(destination)
		return "", fmt.Errorf("clone seed into %s: %w", rule.Path, err)
	}
	// pruneCargoTarget also drops the seed record, which sits at the top level.
	if err := pruneCargoTarget(destination, lockedLocals(locals)); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: seeded from rustc %s", rule.Path, toolchain.Release), nil
}
