package native

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nbardy/cambium/internal/config"
)

// A cargo-target layer shares third-party build output between workspaces and
// nothing else.
//
// A plainly cloned target directory is unsound in another checkout: Cargo finds
// every unit fresh, so the new workspace runs code compiled from the old one's
// sources, with the old checkout's CARGO_MANIFEST_DIR baked in (measured
// 2026-11-12: zero crates recompiled and the binary still printed the source
// checkout's path). Units of registry and git packages do not have that
// problem: their sources are immutable and live under CARGO_HOME, outside any
// checkout. So the layer keeps Cargo's compile cache and removes the freshness
// records of every package that Cargo.lock lists without a `source`, which is
// every workspace member and path dependency. Cargo then rebuilds those in the
// workspace and reuses the rest. Nothing here decides freshness for Cargo; it
// only withholds records, so the worst outcome is a rebuild.

// cargoKept is what survives in a profile directory. Final binaries,
// `examples`, `incremental` and anything a project stores beside them are
// workspace-local or path-bound.
var cargoKept = map[string]bool{".fingerprint": true, "deps": true, "build": true}

// Cargo names a unit's fingerprint directory `<package>-<16 hex digits>`.
var cargoUnitDir = regexp.MustCompile(`^(.+)-[0-9a-f]{16}$`)

// localPackages answers which packages must be rebuilt in every workspace.
type localPackages interface {
	contains(name string) bool
}

// lockedLocals is the set of packages a readable Cargo.lock lists without a
// source.
type lockedLocals map[string]struct{}

func (l lockedLocals) contains(name string) bool {
	_, ok := l[name]
	return ok
}

// unknownLocals stands for a missing or unreadable Cargo.lock: with no way to
// tell local packages apart, every package is treated as local and the seed
// reuses nothing.
type unknownLocals struct{}

func (unknownLocals) contains(string) bool { return true }

// readCargoLocals classifies the lockfile once. Cargo.lock is a TOML file of
// `[[package]]` tables; only `name` and the presence of `source` matter here.
func readCargoLocals(lockfile string) localPackages {
	file, err := os.Open(lockfile)
	if err != nil {
		return unknownLocals{}
	}
	defer file.Close()
	locals := lockedLocals{}
	packages := 0
	name, sourced, open := "", false, false
	flush := func() {
		if open && name != "" {
			packages++
			if !sourced {
				locals[name] = struct{}{}
			}
		}
		name, sourced, open = "", false, false
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "[[package]]":
			flush()
			open = true
		case strings.HasPrefix(line, "["):
			flush()
		case open && strings.HasPrefix(line, "name = "):
			name = strings.Trim(strings.TrimPrefix(line, "name = "), `"`)
		case open && strings.HasPrefix(line, "source = "):
			sourced = true
		}
	}
	flush()
	if scanner.Err() != nil || packages == 0 {
		return unknownLocals{}
	}
	return locals
}

// cargoProfileDirs finds the directories that hold a `.fingerprint`: Cargo
// writes them at `<target>/<profile>` and `<target>/<triple>/<profile>`.
func cargoProfileDirs(target string) (profiles, triples []string, err error) {
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(target, entry.Name())
		if isCargoProfileDir(dir) {
			profiles = append(profiles, dir)
			continue
		}
		nested, err := os.ReadDir(dir)
		if err != nil {
			return nil, nil, err
		}
		found := false
		for _, inner := range nested {
			if inner.IsDir() && isCargoProfileDir(filepath.Join(dir, inner.Name())) {
				profiles = append(profiles, filepath.Join(dir, inner.Name()))
				found = true
			}
		}
		if found {
			triples = append(triples, dir)
		}
	}
	return profiles, triples, nil
}

func isCargoProfileDir(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".fingerprint"))
	return err == nil && info.IsDir()
}

// pruneCargoTarget reduces a private copy of a Cargo target directory to the
// part another workspace may reuse. It deletes inside target only, so callers
// pass a staged layer payload or a freshly materialized workspace copy, never
// a directory a build is using. An unrecognized layout matches no keep rule
// and is emptied, which leaves a cold build rather than a wrong one.
func pruneCargoTarget(target string, locals localPackages) error {
	profiles, triples, err := cargoProfileDirs(target)
	if err != nil {
		return err
	}
	keepTop := map[string]bool{".rustc_info.json": true, "CACHEDIR.TAG": true}
	profileSet := make(map[string]bool, len(profiles))
	for _, dir := range profiles {
		profileSet[dir] = true
		if filepath.Dir(dir) == target {
			keepTop[filepath.Base(dir)] = true
		}
	}
	for _, dir := range triples {
		keepTop[filepath.Base(dir)] = true
		if err := removeEntriesExcept(dir, func(name string) bool { return profileSet[filepath.Join(dir, name)] }); err != nil {
			return err
		}
	}
	if err := removeEntriesExcept(target, func(name string) bool { return keepTop[name] }); err != nil {
		return err
	}
	for _, dir := range profiles {
		if err := removeEntriesExcept(dir, func(name string) bool { return cargoKept[name] }); err != nil {
			return err
		}
		// A unit directory with an unexpected name cannot be attributed to a
		// package, so it goes too.
		err := removeEntriesExcept(filepath.Join(dir, ".fingerprint"), func(name string) bool {
			match := cargoUnitDir.FindStringSubmatch(name)
			return match != nil && !locals.contains(match[1])
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func removeEntriesExcept(dir string, keep func(name string) bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if keep(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cargoLockfile is where Cargo keeps the lockfile for a target directory in
// its default place, beside the workspace manifest.
func cargoLockfile(target string) string {
	return filepath.Join(filepath.Dir(target), "Cargo.lock")
}

// errCargoBusy means a build holds a profile's lock, so a snapshot taken now
// could pair one unit's new output with another's old record.
var errCargoBusy = errors.New("a cargo build is using the target directory")

// holdCargoIdle takes Cargo's own build lock, shared, on every profile in
// target, so no build can start writing while the directory is cloned. It
// returns errCargoBusy when a build is already running. The caller releases
// the locks once the clone is complete.
func holdCargoIdle(target string) (release func(), err error) {
	profiles, _, err := cargoProfileDirs(target)
	if err != nil {
		return nil, err
	}
	var held []*os.File
	release = func() {
		for _, file := range held {
			_ = file.Close()
		}
	}
	for _, dir := range profiles {
		file, err := os.Open(filepath.Join(dir, ".cargo-lock"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, file)
		if err := lockSharedNonBlocking(file); err != nil {
			release()
			return nil, err
		}
	}
	return release, nil
}

// snapshotCargoTarget stages a reusable copy of a live target directory. It
// reports false, with no error, when a build is running: the layer is then
// published as absent and a later capture upgrades it.
func snapshotCargoTarget(ctx context.Context, source, payload string, cloner Cloner, requireCoW bool) (bool, error) {
	release, err := holdCargoIdle(source)
	if errors.Is(err, errCargoBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer release()
	if _, err := cloner.ClonePath(ctx, source, payload, requireCoW); err != nil {
		return false, err
	}
	return true, pruneCargoTarget(payload, readCargoLocals(cargoLockfile(source)))
}

// materializeCargoTarget clones a staged payload into a workspace. The payload
// was pruned against the lockfile of the workspace it came from; this
// workspace may list other local packages, so it is pruned again against its
// own lockfile.
func materializeCargoTarget(ctx context.Context, payload, destination string, cloner Cloner, requireCoW bool) error {
	if _, err := cloner.ClonePath(ctx, payload, destination, requireCoW); err != nil {
		return err
	}
	return pruneCargoTarget(destination, readCargoLocals(cargoLockfile(destination)))
}

// snapshotLayerPayload stages a layer's payload from a live path and reports
// whether there is one to publish.
func snapshotLayerPayload(ctx context.Context, rule config.LayerRule, source, payload string, cloner Cloner, requireCoW bool) (bool, error) {
	switch rule.Mode {
	case config.LayerCargoTarget:
		return snapshotCargoTarget(ctx, source, payload, cloner, requireCoW)
	default:
		if _, err := cloner.ClonePath(ctx, source, payload, requireCoW); err != nil {
			return false, err
		}
		return true, nil
	}
}
