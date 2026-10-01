//go:build !darwin && !linux

package bench

import (
	"errors"
	"runtime"
)

func freeBytes(string) (int64, error) {
	return 0, errors.New("physical allocation measurement is unsupported on this platform")
}
func syncFilesystem()      {}
func platformName() string { return runtime.GOOS + "/" + runtime.GOARCH }
func filesystemName(string) (string, error) {
	return "", errors.New("filesystem detection is unsupported")
}
