// Package lockfile provides a flock-based mutex on a single file under ~/.ahjo/.
package lockfile

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const defaultTimeout = 30 * time.Second

// Acquire takes an exclusive flock on ~/.ahjo/.lock and returns a release func.
// It waits up to 30 seconds before returning a timeout error.
func Acquire() (func(), error) {
	return AcquireWithTimeout(defaultTimeout)
}

func AcquireWithTimeout(timeout time.Duration) (func(), error) {
	if err := paths.EnsureSkeleton(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(paths.LockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("flock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("flock %s: timed out after %s", paths.LockPath(), timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}

	release := func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}
	return release, nil
}
