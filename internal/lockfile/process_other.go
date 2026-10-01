//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package lockfile

// On platforms without a portable process probe, age remains the conservative
// stale-lock signal.
func processAlive(pid int) bool { return true }
