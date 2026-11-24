package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

// Host paths and count, variables so tests can point them at a temp dir.
var (
	loopSysfsRoot = "/sys/block"
	loopDevRoot   = "/dev"
)

const loopDeviceCount = 8

const (
	loopControlDevice = "ahjo-loop-control"
	loopDevicePrefix  = "ahjo-loop-"
)

func loopDeviceName(i int) string { return fmt.Sprintf("%s%d", loopDevicePrefix, i) }

// loopNodeExists reports whether host /dev/loopN exists.
func loopNodeExists(i int) bool {
	_, err := os.Stat(filepath.Join(loopDevRoot, fmt.Sprintf("loop%d", i)))
	return err == nil
}

// loopBound reports whether host loopN is attached to a backing file. The
// kernel only creates /sys/block/loopN/loop/backing_file while the device is
// bound. Fail closed: any error other than "not exist" counts as bound.
func loopBound(i int) bool {
	p := filepath.Join(loopSysfsRoot, fmt.Sprintf("loop%d", i), "loop", "backing_file")
	_, err := os.Stat(p)
	if err == nil {
		return true
	}
	return !errors.Is(err, os.ErrNotExist)
}

// planLoopDevices decides which ahjo-loop-N devices to remove and which to
// add. have holds the indices currently configured on the container;
// exists/bound describe host state. Every configured index that is bound or
// whose node is gone is removed; every index that exists, is unbound and is
// not configured is added. Indices are returned in ascending order.
func planLoopDevices(have map[int]bool, exists, bound func(int) bool) (remove, add []int) {
	for i := 0; i < loopDeviceCount; i++ {
		switch {
		case have[i] && (bound(i) || !exists(i)):
			remove = append(remove, i)
		case !have[i] && exists(i) && !bound(i):
			add = append(add, i)
		}
	}
	return remove, add
}

// wireLoopDevices attaches /dev/loop-control and the host's currently
// unbound /dev/loopN (N in 0..7) to a freshly created container as
// unix-char/unix-block devices. Bound devices are skipped: ahjo's own btrfs
// storage pool is normally backed by a loop file on one of them, and a
// read-write node for it would expose every other container's root
// filesystem. Returns an error when no unbound device is available.
//
// Also sets boot.autostart=false: loop indices are reassigned at each host
// boot, so Incus must not restart the container before ahjo can re-check
// (see reconcileLoopDevices / startContainer).
//
// Gated by customizations.ahjo.nested_incus and host-side consent. Residual
// risk: the passed devices stay host-global, so a device the host (or another
// container) binds while this container runs is readable and writable from
// inside it. See CONTAINER-ISOLATION.md.
//
// `incus copy` carries the device list and boot.autostart, so wire-up on the
// default container propagates to every COW branch container.
func wireLoopDevices(container string) error {
	_, add := planLoopDevices(nil, loopNodeExists, loopBound)
	if len(add) == 0 {
		return fmt.Errorf("no unused host loop device in %s/loop0..%d", loopDevRoot, loopDeviceCount-1)
	}
	if err := incus.ConfigSet(container, "boot.autostart", "false"); err != nil {
		return fmt.Errorf("set boot.autostart: %w", err)
	}
	if err := incus.AddUnixDevice(container, loopControlDevice, "unix-char", filepath.Join(loopDevRoot, "loop-control")); err != nil {
		return err
	}
	for _, i := range add {
		src := filepath.Join(loopDevRoot, fmt.Sprintf("loop%d", i))
		if err := incus.AddUnixDevice(container, loopDeviceName(i), "unix-block", src); err != nil {
			return fmt.Errorf("%s: %w", loopDeviceName(i), err)
		}
	}
	var skipped []string
	for i := 0; i < loopDeviceCount; i++ {
		if loopNodeExists(i) && loopBound(i) {
			skipped = append(skipped, fmt.Sprintf("loop%d", i))
		}
	}
	fmt.Fprintf(cobraOutErr(), "nested_incus: passed %s into %s", fmtLoopList(add), container)
	if len(skipped) > 0 {
		fmt.Fprintf(cobraOutErr(), "; skipped in-use %s", strings.Join(skipped, ", "))
	}
	fmt.Fprintln(cobraOutErr())
	return nil
}

func fmtLoopList(idx []int) string {
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = fmt.Sprintf("loop%d", n)
	}
	return strings.Join(parts, ", ")
}

// reconcileLoopDevices re-checks a stopped container's loop devices against
// the current host state: devices that are now bound (or gone) are removed,
// newly free ones are added. Loop indices are reassigned at every host boot,
// so a device that was free at `repo add` time may now back the storage pool.
//
// Containers without ahjo-loop-control are untouched (one incus call of
// overhead). Any container that has it gets boot.autostart=false, running or
// not. A running container's devices are left alone: devices it bound itself
// are legitimately "bound", so only a stopped container can be judged. A
// stale binding the container left behind (bound without autoclear, then
// stopped) is treated as foreign and the index is dropped — the safe
// direction. A removal error is returned so the caller does not start the
// container; an add error is only a warning.
func reconcileLoopDevices(container string) error {
	names, err := incus.ListDeviceNames(container)
	if err != nil {
		return err
	}
	have := map[int]bool{}
	hasControl := false
	for _, n := range names {
		if n == loopControlDevice {
			hasControl = true
			continue
		}
		var i int
		if strings.HasPrefix(n, loopDevicePrefix) {
			if _, err := fmt.Sscanf(strings.TrimPrefix(n, loopDevicePrefix), "%d", &i); err == nil && i >= 0 && i < loopDeviceCount && n == loopDeviceName(i) {
				have[i] = true
			}
		}
	}
	if !hasControl {
		return nil
	}
	// Containers wired before boot.autostart was set at repo add would
	// otherwise be restored by Incus at host boot, ahead of this re-check.
	// Idempotent, and valid on a running container.
	if err := incus.ConfigSet(container, "boot.autostart", "false"); err != nil {
		return fmt.Errorf("set boot.autostart: %w", err)
	}
	status, err := incus.ContainerStatus(container)
	if err != nil {
		return err
	}
	if strings.EqualFold(status, "Running") {
		return nil
	}
	remove, add := planLoopDevices(have, loopNodeExists, loopBound)
	for _, i := range remove {
		if err := incus.RemoveDevice(container, loopDeviceName(i)); err != nil {
			return fmt.Errorf("remove %s: %w", loopDeviceName(i), err)
		}
		fmt.Fprintf(cobraOutErr(), "warn: /dev/loop%d is in use on the host; not passing it to %s\n", i, container)
	}
	for _, i := range add {
		src := filepath.Join(loopDevRoot, fmt.Sprintf("loop%d", i))
		if err := incus.AddUnixDevice(container, loopDeviceName(i), "unix-block", src); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: add %s: %v\n", loopDeviceName(i), err)
		}
	}
	return nil
}

// startContainer re-checks loop devices, then starts the container. Use it
// for every ahjo-initiated start except the first start in repo add.
func startContainer(name string) error {
	if err := reconcileLoopDevices(name); err != nil {
		return fmt.Errorf("reconcile loop devices: %w", err)
	}
	return incus.Start(name)
}
