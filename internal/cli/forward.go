package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// ahjoForwardDevicePrefix names the proxy devices `ahjo forward` manages, keyed
// by the in-container port they listen on. Mirrors autoExposeDevicePrefix so
// `ls` and auto-expose can recognise them by name.
const ahjoForwardDevicePrefix = "ahjo-forward-"

// forwardDeviceName is the device name for a forward listening on <cport>
// inside the container. Keying on the container port (the in-container address)
// makes `--off` and `ls` unambiguous and lets a re-`forward` to a different
// host port self-correct via the connect= diff in EnsureReverseProxy.
func forwardDeviceName(cport int) string {
	return fmt.Sprintf("%s%d", ahjoForwardDevicePrefix, cport)
}

func newForwardCmd() *cobra.Command {
	var off bool
	cmd := &cobra.Command{
		Use:   "forward <alias> <host-port> [<container-port>]",
		Short: "Pipe a host port into the container on 127.0.0.1 (inbound counterpart of expose)",
		Long: `Forward a service running on the HOST into the container, so a process inside
(e.g. Claude) can reach it on 127.0.0.1.

  ahjo forward foo 8000        # host :8000 -> container :8000
  ahjo forward foo 8000 3000   # host :8000 -> container :3000
  ahjo forward foo 8000 --off  # tear it down (the port names the in-container listener)

This is the inbound counterpart to 'ahjo expose' (which publishes a container
port out to the host). The default makes the host's localhost:<port> appear as
localhost:<port> inside the container, so code and configs that hardcode a
loopback address work unmodified.

The host app must listen on 127.0.0.1 or 0.0.0.0. On macOS, Lima's user-mode
network forwards the container's gateway to the Mac's loopback; an app bound to
a specific non-loopback interface (e.g. 192.168.x.x) may not be reachable. The
container must be running first ('ahjo shell <alias>').

A forward lives only as long as the container: it is dropped on stop/restart or
removal, so re-run 'ahjo forward' after restarting the container.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if off {
				// <alias> <container-port> — the port that names the device.
				return cobra.ExactArgs(2)(cmd, args)
			}
			return cobra.RangeArgs(2, 3)(cmd, args)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if off {
				cport, err := parseForwardPort(args[1])
				if err != nil {
					return err
				}
				return runForwardOff(args[0], cport)
			}
			hostPort, err := parseForwardPort(args[1])
			if err != nil {
				return err
			}
			cport := hostPort
			if len(args) == 3 {
				if cport, err = parseForwardPort(args[2]); err != nil {
					return err
				}
			}
			return runForwardOn(args[0], hostPort, cport)
		},
	}
	cmd.Flags().BoolVar(&off, "off", false, "remove the forward for <container-port> instead of adding one")
	return cmd
}

// parseForwardPort validates a port argument the same way `expose` does.
func parseForwardPort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

func runForwardOn(alias string, hostPort, cport int) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return fmt.Errorf("no branch with alias %q", alias)
	}
	containerName, err := resolveContainerName(br)
	if err != nil {
		return err
	}

	// Refuse if the container is stopped: forward must not become a hidden way
	// to start containers, and bind=container needs a live namespace.
	status, err := incus.ContainerStatus(containerName)
	if err != nil {
		return err
	}
	if !strings.EqualFold(status, "Running") {
		return fmt.Errorf("container %s is %q; run `ahjo shell %s` first", containerName, status, alias)
	}

	if err := ensureForwardDevice(containerName, hostPort, cport); err != nil {
		return err
	}
	fmt.Printf("forward: host 127.0.0.1:%d -> container :%d\n", hostPort, cport)
	return nil
}

func runForwardOff(alias string, cport int) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return fmt.Errorf("no branch with alias %q", alias)
	}
	containerName, err := resolveContainerName(br)
	if err != nil {
		return err
	}
	if err := incus.RemoveDevice(containerName, forwardDeviceName(cport)); err != nil {
		return err
	}
	fmt.Printf("forward off: container :%d\n", cport)
	return nil
}

// ensureForwardDevice wires (or refreshes) the bind=container proxy that makes
// the host's <hostPort> reachable as 127.0.0.1:<cport> inside the container.
// Kept standalone so a future auto-reattach path can loop it over persisted
// forwards on container start, exactly like attachPasteShim.
func ensureForwardDevice(containerName string, hostPort, cport int) error {
	connectIP, err := incus.ReverseConnectIP()
	if err != nil {
		return err
	}
	return incus.EnsureReverseProxy(containerName, forwardDeviceName(cport), cport, connectIP, hostPort)
}
