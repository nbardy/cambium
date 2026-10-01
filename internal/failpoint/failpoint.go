package failpoint

import (
	"errors"
	"fmt"
	"os"
)

var ErrInjectedCrash = errors.New("injected crash")

// Check is used only by integration tests and fault-injection benchmarks. It
// deliberately returns an error that callers treat like a process crash: the
// operation journal is retained and normal rollback is skipped.
func Check(name string) error {
	if os.Getenv("CAMBIUM_FAILPOINT") == name {
		return fmt.Errorf("%w at %s", ErrInjectedCrash, name)
	}
	return nil
}
