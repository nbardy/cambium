//go:build darwin || linux

package bench

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

func freeBytes(path string) (int64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return int64(stats.Bavail) * int64(stats.Bsize), nil
}

func syncFilesystem() { syscall.Sync() }

func platformName() string { return runtime.GOOS + "/" + runtime.GOARCH }

func filesystemName(path string) (string, error) {
	if runtime.GOOS == "darwin" {
		output, err := exec.Command("diskutil", "info", path).CombinedOutput()
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(string(output), "\n") {
			if strings.Contains(line, "File System Personality") {
				_, value, _ := strings.Cut(line, ":")
				return strings.TrimSpace(value), nil
			}
		}
		return "", fmt.Errorf("filesystem not reported")
	}
	output, err := exec.Command("stat", "-f", "-c", "%T", path).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
