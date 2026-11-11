//go:build !darwin && !linux

package native

import "os"

// Without Cargo's lock there is no way to know a build is idle, so every
// target directory counts as busy and is never snapshotted.
func lockSharedNonBlocking(*os.File) error { return errCargoBusy }
