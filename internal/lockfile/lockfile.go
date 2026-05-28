// Package lockfile provides a flock-based mutex on a single file under ~/.ahjo/.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const defaultTimeout = 30 * time.Second

// Acquire takes an exclusive flock on ~/.ahjo/.lock and returns a release func.
// It waits up to 30 seconds before returning a timeout error.
//
// This is intentionally a coarse, command-level mutex — NOT a registry
// read-modify-write lock, and deliberately not folded into a
// registry.Update(fn) helper. Call sites hold it across a whole critical
// section that spans several shared-state files at once: the registry plus
// ports.json, the per-branch host-keys dir, and the rendered ssh-config (see
// create.go::createReserveBranch), or across container create/delete
// operations (rm.go, repo_rm.go). A helper scoped to registry load/save would
// silently drop those other resources out of the lock, so the explicit
// acquire/defer-release at each call site is the correct shape here.
func Acquire() (func(), error) {
	return AcquireWithTimeout(defaultTimeout)
}

func AcquireWithTimeout(timeout time.Duration) (func(), error) {
	if err := paths.EnsureSkeleton(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(paths.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
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
