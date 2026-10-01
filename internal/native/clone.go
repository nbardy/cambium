package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nbardy/cambium/internal/execx"
	"github.com/nbardy/cambium/internal/model"
)

type CloneCapability struct {
	Mode      model.CloneMode `json:"mode"`
	Supported bool            `json:"supported"`
	Detail    string          `json:"detail"`
}

type MaterializationPlan struct {
	Requested   model.Materializer `json:"requested"`
	Resolved    model.Materializer `json:"resolved"`
	CloneMode   model.CloneMode    `json:"clone_mode"`
	UseBaseline bool               `json:"use_baseline"`
	Capability  CloneCapability    `json:"capability"`
}

type Cloner struct {
	Runner execx.Runner
}

// Probe reports whether directory supports Cambium's native copy-on-write
// primitive for a source and destination located on that same filesystem.
func (c Cloner) Probe(ctx context.Context, directory string) CloneCapability {
	return c.ProbeBetween(ctx, directory, directory)
}

// ProbeBetween verifies the complete source-to-destination route, not merely
// whether the destination filesystem supports reflinks in isolation. APFS,
// btrfs, and XFS cannot share extents across filesystem/volume boundaries, so
// a custom workspace on another volume must fall back before Git registration.
func (c Cloner) ProbeBetween(ctx context.Context, sourceDirectory, destinationDirectory string) CloneCapability {
	if err := os.MkdirAll(destinationDirectory, 0o755); err != nil {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: err.Error()}
	}
	if _, err := os.Stat(sourceDirectory); err != nil {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: fmt.Sprintf("inspect clone source: %v", err)}
	}
	same, err := sameFilesystem(sourceDirectory, destinationDirectory)
	if err != nil {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: err.Error()}
	}
	if !same {
		return CloneCapability{
			Mode:      model.CloneModeCopy,
			Supported: false,
			Detail:    "source and destination are on different filesystems or volumes; native CoW cannot share blocks across them",
		}
	}
	capability := c.probeLocal(ctx, destinationDirectory)
	if capability.Supported {
		capability.Detail = "source and destination share a filesystem and the native copy-on-write clone probe succeeded"
	}
	return capability
}

func (c Cloner) probeLocal(ctx context.Context, directory string) CloneCapability {
	if c.Runner == nil {
		c.Runner = execx.OSRunner{}
	}
	temporary, err := os.MkdirTemp(directory, ".cambium-clone-probe-*")
	if err != nil {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: err.Error()}
	}
	defer os.RemoveAll(temporary)
	source := filepath.Join(temporary, "source")
	destination := filepath.Join(temporary, "destination")
	if err := os.WriteFile(source, []byte("cambium-copy-on-write-probe"), 0o644); err != nil {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: err.Error()}
	}
	mode, command, args, ok := cloneFileCommand(source, destination)
	if !ok {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: "platform has no configured native clone primitive"}
	}
	if _, err := c.Runner.Run(ctx, execx.Command{Dir: temporary, Name: command, Args: args}); err != nil {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: err.Error()}
	}
	bytes, err := os.ReadFile(destination)
	if err != nil || string(bytes) != "cambium-copy-on-write-probe" {
		return CloneCapability{Mode: model.CloneModeCopy, Supported: false, Detail: "clone probe produced invalid content"}
	}
	return CloneCapability{Mode: mode, Supported: true, Detail: "native copy-on-write clone succeeded"}
}

// Plan deliberately falls back from auto to Git's own checkout, not to a
// baseline plus an ordinary recursive copy. This avoids making Cambium slower
// and larger than plain worktrees on ext4 and other non-reflink filesystems.
func (c Cloner) Plan(ctx context.Context, requested model.Materializer, sourceDirectory, destinationDirectory string, requireCoW bool) (MaterializationPlan, error) {
	return planMaterialization(requested, c.ProbeBetween(ctx, sourceDirectory, destinationDirectory), requireCoW)
}

func planMaterialization(requested model.Materializer, capability CloneCapability, requireCoW bool) (MaterializationPlan, error) {
	if requireCoW {
		requested = model.MaterializerCoW
	}
	plan := MaterializationPlan{Requested: requested, Capability: capability}
	switch requested {
	case model.MaterializerAuto:
		if capability.Supported {
			plan.Resolved = model.MaterializerCoW
			plan.CloneMode = capability.Mode
			plan.UseBaseline = true
		} else {
			plan.Resolved = model.MaterializerGit
			plan.CloneMode = model.CloneModeGitCheckout
		}
	case model.MaterializerCoW:
		if !capability.Supported {
			return MaterializationPlan{}, fmt.Errorf("copy-on-write is required but unavailable: %s", capability.Detail)
		}
		plan.Resolved = model.MaterializerCoW
		plan.CloneMode = capability.Mode
		plan.UseBaseline = true
	case model.MaterializerCopy:
		plan.Resolved = model.MaterializerCopy
		plan.CloneMode = model.CloneModeCopy
		plan.UseBaseline = true
	case model.MaterializerGit:
		plan.Resolved = model.MaterializerGit
		plan.CloneMode = model.CloneModeGitCheckout
	default:
		return MaterializationPlan{}, fmt.Errorf("unsupported materializer %q", requested)
	}
	return plan, nil
}

func (c Cloner) CloneTree(ctx context.Context, source, destination string, mode model.Materializer) (model.CloneMode, error) {
	if c.Runner == nil {
		c.Runner = execx.OSRunner{}
	}
	switch mode {
	case model.MaterializerCoW:
		capability := c.ProbeBetween(ctx, filepath.Dir(filepath.Clean(source)), destination)
		if !capability.Supported {
			return "", fmt.Errorf("copy-on-write is unavailable: %s", capability.Detail)
		}
		command, args := cloneTreeCommand(source, destination, true)
		if _, err := c.Runner.Run(ctx, execx.Command{Dir: destination, Name: command, Args: args}); err != nil {
			return "", fmt.Errorf("native CoW tree clone failed: %w", err)
		}
		return capability.Mode, nil
	case model.MaterializerCopy:
		command, args := cloneTreeCommand(source, destination, false)
		if _, err := c.Runner.Run(ctx, execx.Command{Dir: destination, Name: command, Args: args}); err != nil {
			return "", fmt.Errorf("tree copy failed: %w", err)
		}
		return model.CloneModeCopy, nil
	default:
		return "", fmt.Errorf("materializer %q cannot clone a baseline tree", mode)
	}
}

func (c Cloner) ClonePath(ctx context.Context, source, destination string, requireCoW bool) (model.CloneMode, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return "", err
		}
		mode := model.MaterializerCopy
		if c.ProbeBetween(ctx, filepath.Dir(filepath.Clean(source)), destination).Supported {
			mode = model.MaterializerCoW
		} else if requireCoW {
			return "", errors.New("copy-on-write is required but unavailable for the source-to-destination route")
		}
		return c.CloneTree(ctx, source, destination, mode)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return "", err
		}
		if err := os.Symlink(target, destination); err != nil {
			return "", err
		}
		return model.CloneModeCopy, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("cannot clone special file %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	capability := c.ProbeBetween(ctx, filepath.Dir(filepath.Clean(source)), filepath.Dir(destination))
	if capability.Supported {
		_, command, args, _ := cloneFileCommand(source, destination)
		if _, err := c.Runner.Run(ctx, execx.Command{Dir: filepath.Dir(destination), Name: command, Args: args}); err == nil {
			return capability.Mode, nil
		} else if requireCoW {
			return "", err
		}
	}
	if requireCoW {
		return "", fmt.Errorf("copy-on-write is required but unavailable: %s", capability.Detail)
	}
	command, args := copyFileCommand(source, destination)
	if _, err := c.Runner.Run(ctx, execx.Command{Dir: filepath.Dir(destination), Name: command, Args: args}); err != nil {
		return "", err
	}
	return model.CloneModeCopy, nil
}

func cloneFileCommand(source, destination string) (model.CloneMode, string, []string, bool) {
	switch runtime.GOOS {
	case "darwin":
		return model.CloneModeAPFS, "cp", []string{"-c", "-p", source, destination}, true
	case "linux":
		return model.CloneModeReflink, "cp", []string{"--reflink=always", "-a", source, destination}, true
	default:
		return model.CloneModeCopy, "", nil, false
	}
}

func cloneTreeCommand(source, destination string, cow bool) (string, []string) {
	from := filepath.Clean(source) + string(os.PathSeparator) + "."
	switch runtime.GOOS {
	case "darwin":
		if cow {
			return "cp", []string{"-c", "-p", "-R", from, destination}
		}
		return "cp", []string{"-p", "-R", from, destination}
	default:
		if cow {
			return "cp", []string{"--reflink=always", "-a", from, destination}
		}
		return "cp", []string{"-a", from, destination}
	}
}

func copyFileCommand(source, destination string) (string, []string) {
	if runtime.GOOS == "darwin" {
		return "cp", []string{"-p", source, destination}
	}
	return "cp", []string{"-a", source, destination}
}
