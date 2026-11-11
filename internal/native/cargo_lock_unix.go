//go:build darwin || linux

package native

import (
	"errors"
	"os"
	"syscall"
)

// lockSharedNonBlocking takes the advisory lock Cargo uses for a build
// directory. Cargo holds it exclusively for the length of a build.
func lockSharedNonBlocking(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errCargoBusy
	}
	return err
}
