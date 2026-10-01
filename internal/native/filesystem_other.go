//go:build !darwin && !linux

package native

import "fmt"

func sameFilesystem(_, _ string) (bool, error) {
	return false, fmt.Errorf("native copy-on-write probing is unsupported on this platform")
}
