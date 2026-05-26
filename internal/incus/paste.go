package incus

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
)

//go:embed paste_shim_xclip.sh
var pasteShimXclip []byte

//go:embed paste_shim_wlpaste.sh
var pasteShimWlpaste []byte

// pasteDaemonProxyDevice is the device name. Kept in one place so
// EnsurePasteDaemonProxy and any future cleanup paths agree.
const pasteDaemonProxyDevice = "ahjo-paste-daemon"

// PasteDaemonContainerPort is the TCP port the paste-daemon proxy device
// listens on inside the container. Exported so autoexpose can skip it —
// otherwise reconcileAutoExpose sees the synthetic listener via `ss -tlnH`
// and helpfully forwards it to a real host port, cluttering the device
// list and burning a port allocation. Shim scripts can't import this
// constant (they're POSIX sh); they keep the literal in sync by hand.
const PasteDaemonContainerPort = 18340

// EnsurePasteDaemonProxy wires a TCP proxy device that lets in-container
// callers reach the macOS host's paste-daemon (listens on
// 127.0.0.1:18340 from the Mac). The proxy listens on 127.0.0.1:18340
// *inside* the container (bind=container) so the xclip/wl-paste shims can
// curl loopback without knowing anything about the VM topology.
//
// Incus's proxy device requires an IP literal for `connect=`, not a
// hostname. We resolve host.lima.internal (Lima's pinned alias for the Mac
// gateway) at every call so a Lima restart that hands out a new gateway IP
// self-corrects on the next ahjo shell / ahjo claude — ensureReverseProxy
// diffs the resolved address against the cached device and only re-adds when
// it changed.
//
// This deliberately requires Lima: resolveLimaHostIP errors on a native
// Linux host, where there is no Mac paste-daemon to reach. (Contrast
// ReverseConnectIP, which falls back to 127.0.0.1 for `ahjo forward`.)
//
// Caller must invoke this AFTER `incus start` — bind=container proxy
// devices need a live container namespace to create the listen socket.
func EnsurePasteDaemonProxy(container string) error {
	ip, err := resolveLimaHostIP()
	if err != nil {
		return fmt.Errorf("resolve host.lima.internal: %w", err)
	}
	return EnsureReverseProxy(container, pasteDaemonProxyDevice, PasteDaemonContainerPort, ip, PasteDaemonContainerPort)
}

// resolveLimaHostIP returns the IPv4 address of host.lima.internal — the
// hostname Lima writes into /etc/hosts in the VM pointing at the macOS
// gateway. Prefers an IPv4 result; Incus's proxy connect target is
// happiest with a literal v4 address. On a non-Lima host this returns
// the local loopback miss and bubbles an error up.
func resolveLimaHostIP() (string, error) {
	addrs, err := net.LookupHost("host.lima.internal")
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	if len(addrs) > 0 {
		return addrs[0], nil
	}
	return "", fmt.Errorf("host.lima.internal resolved to no addresses")
}

// WritePasteShims installs the xclip/wl-paste shims into the container at
// /usr/local/bin/{xclip,wl-paste} with mode 0755. Re-pushed every time so
// a shim update in ahjo lands on existing containers without an explicit
// migration step.
//
// Implementation: write the embedded bytes to a host tempfile and use
// `incus file push --mode 0755` to land each in place. The tempfiles are
// removed eagerly — they exist only long enough for the push.
func WritePasteShims(container string) error {
	if err := pushShim(container, pasteShimXclip, "/usr/local/bin/xclip"); err != nil {
		return fmt.Errorf("install xclip shim: %w", err)
	}
	if err := pushShim(container, pasteShimWlpaste, "/usr/local/bin/wl-paste"); err != nil {
		return fmt.Errorf("install wl-paste shim: %w", err)
	}
	return nil
}

func pushShim(container string, content []byte, containerPath string) error {
	tmp, err := os.CreateTemp("", "ahjo-paste-shim-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	cmd := execCommand("incus", "file", "push", "--mode", "0755", tmpPath, container+containerPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	os.Stderr.Write(out)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("incus file push %s %s%s: exit %d", tmpPath, container, containerPath, ee.ExitCode())
	}
	return fmt.Errorf("incus file push %s %s%s: %w", tmpPath, container, containerPath, err)
}
