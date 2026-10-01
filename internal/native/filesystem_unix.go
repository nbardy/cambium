//go:build darwin || linux

package native

import (
	"fmt"
	"os"
	"syscall"
)

func sameFilesystem(source, destination string) (bool, error) {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return false, fmt.Errorf("stat clone source filesystem: %w", err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		return false, fmt.Errorf("stat clone destination filesystem: %w", err)
	}
	sourceStat, ok := sourceInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("clone source does not expose filesystem identity")
	}
	destinationStat, ok := destinationInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("clone destination does not expose filesystem identity")
	}
	return uint64(sourceStat.Dev) == uint64(destinationStat.Dev), nil
}
