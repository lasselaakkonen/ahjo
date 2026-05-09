// Package idmap holds the workspace UID-mapping wiring described in
// CONTAINER-ISOLATION.md: the /etc/subuid + /etc/subgid grants the Incus
// daemon needs to honor a `raw.idmap` directive, and the raw.idmap value
// itself.
//
// COI v0.8.x implements `raw.idmap` for non-Lima environments but
// auto-disables it on Lima/Colima (it assumes the workspace is a virtiofs
// mount handled at the VM level). ahjo's containers live on the VM's local
// btrfs pool, so the assumption doesn't hold; Phase 1 ahjo applies
// raw.idmap itself in cli/repo.go (default container) and cli/new.go
// (COW-cloned branch containers).
package idmap

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/initflow"
)

// RawIdmapValue is the per-container raw.idmap value to apply: maps the
// in-VM host UID/GID onto the in-container `ubuntu` user (1000:1000). Emits
// two lines (`uid <hostuid> 1000` + `gid <hostgid> 1000`) rather than the
// shorter `both <hostid> 1000` because incus's `both` form takes a single
// host id and uses it for both UID and GID — fine when the in-VM user has
// uid == gid (the default 1000:1000), but wrong on Lima setups that
// propagate the macOS uid (e.g. 501) while keeping a separate gid (e.g.
// 1000): the resulting `gid 501 1000` then fails because /etc/subgid only
// grants the user's actual gid (1000), not the uid (501), so newgidmap
// rejects the mapping and `incus start` aborts.
func RawIdmapValue(uid, gid int) string {
	return fmt.Sprintf("uid %d 1000\ngid %d 1000", uid, gid)
}

// HasSubuidGrants reports whether /etc/subuid and /etc/subgid both already
// carry the expected `root:<id>:1` line for the given uid/gid. Used by the
// init/update step's Skip helper so re-runs detect "already done" without
// the side effect of EnsureSubuidGrants.
func HasSubuidGrants(uid, gid int) (bool, error) {
	subuid, err := fileContainsLine("/etc/subuid", fmt.Sprintf("root:%d:1", uid))
	if err != nil {
		return false, err
	}
	if !subuid {
		return false, nil
	}
	subgid, err := fileContainsLine("/etc/subgid", fmt.Sprintf("root:%d:1", gid))
	if err != nil {
		return false, err
	}
	return subgid, nil
}

// EnsureSubuidGrants makes sure /etc/subuid and /etc/subgid each carry a
// `root:<id>:1` line for the given uid/gid, so the (root-running) Incus
// daemon is permitted to delegate exactly those IDs into a container's
// userns. Without these grants, `newuidmap` rejects the mapping requested
// by `raw.idmap` and `incus start` fails.
//
// Idempotent: re-runs are no-ops once both lines are present. Returns
// changed=true when either file was modified, so the caller can decide
// whether to restart the Incus daemon.
func EnsureSubuidGrants(uid, gid int, out io.Writer) (changed bool, err error) {
	uidChanged, err := ensureLine("/etc/subuid", fmt.Sprintf("root:%d:1", uid), out)
	if err != nil {
		return false, err
	}
	gidChanged, err := ensureLine("/etc/subgid", fmt.Sprintf("root:%d:1", gid), out)
	if err != nil {
		return uidChanged, err
	}
	return uidChanged || gidChanged, nil
}

// ensureLine returns (changed, err). It returns changed=false (no-op) when
// path already contains line; otherwise it appends line via `sudo tee -a`.
func ensureLine(path, line string, out io.Writer) (bool, error) {
	present, err := fileContainsLine(path, line)
	if err != nil {
		return false, err
	}
	if present {
		fmt.Fprintf(out, "  → %s already has %q\n", path, line)
		return false, nil
	}
	if err := initflow.RunShell(out, line+"\n", "sudo", "tee", "-a", path); err != nil {
		return false, fmt.Errorf("append %q to %s: %w", line, path, err)
	}
	return true, nil
}

// fileContainsLine reports whether path has a line that equals line exactly
// (after trimming trailing whitespace). A missing file is treated as "not
// present" rather than an error so callers handle bring-up uniformly.
func fileContainsLine(path, line string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimRight(sc.Text(), " \t\r") == line {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("scan %s: %w", path, err)
	}
	return false, nil
}
