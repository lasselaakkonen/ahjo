//go:build darwin

// Mac-side block of `ahjo doctor`. Reports whether the host's ssh agent reaches
// the in-VM ssh agent — the one piece of state that crosses the Lima boundary
// and that no other check covers. The relayed in-VM doctor still runs after
// this block; this file only adds the host half.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/agent"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/preflight"
)

// runMacDoctor prints the host half of `ahjo doctor` and returns true if any
// check failed. Output style matches preflight.Format so the merged stdout
// from this block plus the relayed in-VM block reads as one report.
func runMacDoctor(w io.Writer) bool {
	fmt.Fprintln(w, "[host-side checks]")
	ps := []preflight.Problem{
		checkAgentConfigured(),
		checkSSHAuthSockKind(),
	}
	vmP, runVMCheck := checkVMRunning()
	ps = append(ps, vmP)
	if runVMCheck {
		ps = append(ps, checkInVMAgent())
	}
	for _, p := range ps {
		fmt.Fprintln(w, preflight.Format(p))
	}
	fmt.Fprintln(w)
	return preflight.Worst(ps) >= preflight.Fail
}

// checkAgentConfigured reports whether `ahjo init` has picked an agent
// socket and that the socket still has keys. This is the lever ahjo
// actually uses when invoking limactl, so it's the source of truth.
func checkAgentConfigured() preflight.Problem {
	sock, label, err := agent.Resolve()
	if err != nil {
		return preflight.Problem{
			Severity: preflight.Fail,
			Title:    "no ssh-agent picked for VM forwarding",
			Detail:   err.Error(),
			Fix:      sshAgentFix,
		}
	}
	return preflight.Problem{
		Severity: preflight.OK,
		Title:    fmt.Sprintf("ssh-agent forwarded into VM: %s (%s)", label, sock),
	}
}

// checkSSHAuthSockKind is informational once an agent is configured: ahjo
// overrides SSH_AUTH_SOCK in its own subprocess env, so the value in the
// caller's shell doesn't affect VM forwarding. We still flag the launchd
// default with a warn so users notice their *interactive* `git`/`ssh` might
// be reaching an empty agent.
func checkSSHAuthSockKind() preflight.Problem {
	sock := os.Getenv("SSH_AUTH_SOCK")
	configured := agent.Configured() != ""
	if sock == "" {
		if configured {
			return preflight.Problem{
				Severity: preflight.OK,
				Title:    "shell SSH_AUTH_SOCK unset (ok — ahjo uses its configured agent)",
			}
		}
		return preflight.Problem{
			Severity: preflight.Warn,
			Title:    "SSH_AUTH_SOCK not set in this shell",
			Fix:      sshAgentFix,
		}
	}
	if strings.HasPrefix(sock, agent.LaunchdSockPrefix) {
		sev := preflight.Warn
		title := "shell SSH_AUTH_SOCK points at macOS's launchd default agent"
		detail := sock + "  (your interactive `git`/`ssh` may be using an empty agent; ahjo's VM forwarding is unaffected when an agent is configured)"
		if configured {
			sev = preflight.OK
			title = "shell SSH_AUTH_SOCK is launchd default (ok — ahjo overrides it for limactl)"
		}
		return preflight.Problem{Severity: sev, Title: title, Detail: detail}
	}
	return preflight.Problem{
		Severity: preflight.OK,
		Title:    "shell SSH_AUTH_SOCK points at a custom agent socket",
	}
}

// checkVMRunning returns the VM-running Problem and a bool indicating whether
// the in-VM agent check is worth running (true only when the VM is up).
func checkVMRunning() (preflight.Problem, bool) {
	if !vmRunning() {
		return preflight.Problem{
			Severity: preflight.Fail,
			Title:    fmt.Sprintf("Lima VM %q not running", vmName),
			Fix:      "limactl start " + vmName,
		}, false
	}
	return preflight.Problem{
		Severity: preflight.OK,
		Title:    fmt.Sprintf("Lima VM %q running", vmName),
	}, true
}

// checkInVMAgent verifies that what reaches the VM matches what ahjo
// resolved on the host. Mismatch usually means the configured socket has
// changed since the running ssh session was opened, or the agent went
// away mid-session.
func checkInVMAgent() preflight.Problem {
	hostSock, _, hostErr := agent.Resolve()
	hostN := -1
	if hostErr == nil {
		if n, ok := agentKeyCountForSock(hostSock); ok {
			hostN = n
		}
	}
	out, err := lima.Cmd("shell", vmName, "--", "ssh-add", "-l").Output()
	vmN := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return preflight.Problem{
				Severity: preflight.Fail,
				Title:    "could not query in-VM ssh agent",
				Detail:   strings.TrimSpace(err.Error()),
			}
		}
	} else {
		vmN = countNonEmptyLines(string(out))
	}
	switch {
	case vmN == 0 && hostN > 0:
		return preflight.Problem{
			Severity: preflight.Fail,
			Title:    "in-VM ssh agent is empty (host has " + fmt.Sprint(hostN) + ")",
			Detail:   "the forwarded agent reached the VM as empty — try `limactl stop " + vmName + " && limactl start " + vmName + "` so the new socket gets picked up by any persistent session.",
			Fix:      sshAgentFix,
		}
	case hostN >= 0 && vmN != hostN:
		return preflight.Problem{
			Severity: preflight.Warn,
			Title:    fmt.Sprintf("in-VM ssh agent: %d key(s) (host has %d)", vmN, hostN),
		}
	default:
		return preflight.Problem{
			Severity: preflight.OK,
			Title:    fmt.Sprintf("in-VM ssh agent: %d key(s)", vmN),
		}
	}
}

// agentKeyCountForSock returns (n, true) when `ssh-add -l` against sock
// succeeded (n=0 means reachable-empty), or (0, false) when ssh-add can't
// run or the agent is unreachable.
func agentKeyCountForSock(sock string) (int, bool) {
	if _, err := exec.LookPath("ssh-add"); err != nil {
		return 0, false
	}
	cmd := exec.Command("ssh-add", "-l")
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	out, err := cmd.Output()
	if err == nil {
		return countNonEmptyLines(string(out)), true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return 0, true
	}
	return 0, false
}

// sshAgentFix is the multi-line fix block attached to checks that point
// at the agent picker: re-running `ahjo init` re-detects 1Password /
// Secretive / gpg-agent / ssh_config IdentityAgent and writes the chosen
// socket into ~/.ahjo/config.toml [mac].ssh_auth_sock. ahjo overrides
// SSH_AUTH_SOCK in its limactl subprocess env, so no shell rc edit is
// needed.
const sshAgentFix = "run `ahjo init` to (re-)detect host agents.\n" +
	"       Make sure 1Password (or your agent app) is running and has at\n" +
	"       least one key loaded; see CONTAINER-ISOLATION.md for details."

func countNonEmptyLines(s string) int {
	n := 0
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		if strings.TrimSpace(scan.Text()) != "" {
			n++
		}
	}
	return n
}
