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

	"github.com/lasselaakkonen/ahjo/internal/preflight"
)

// launchdSockPrefix matches macOS's default $SSH_AUTH_SOCK, which points at a
// launchd-provided ssh-agent that has no keys unless the user opted into the
// system Keychain integration. Detecting it lets us tell users why their VM
// agent looks empty even though `git` works on the host (host `git` reaches
// 1Password etc. via ~/.ssh/config IdentityAgent, which Lima doesn't honor).
const launchdSockPrefix = "/private/tmp/com.apple.launchd."

// runMacDoctor prints the host half of `ahjo doctor` and returns true if any
// check failed. Output style matches preflight.Format so the merged stdout
// from this block plus the relayed in-VM block reads as one report.
func runMacDoctor(w io.Writer) bool {
	fmt.Fprintln(w, "[host-side checks]")
	ps := []preflight.Problem{
		checkHostAgent(),
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

// hostAgentKeyCount runs `ssh-add -l` against $SSH_AUTH_SOCK in the current
// shell. Exit semantics: 0 = keys present (one per line), 1 = agent reachable
// but empty, 2 = no agent.
func hostAgentKeyCount() (int, error) {
	out, err := exec.Command("ssh-add", "-l").Output()
	if err == nil {
		return countNonEmptyLines(string(out)), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return 0, nil
	}
	return 0, err
}

func checkHostAgent() preflight.Problem {
	n, err := hostAgentKeyCount()
	if err != nil {
		return preflight.Problem{
			Severity: preflight.Fail,
			Title:    "no ssh agent reachable from this shell",
			Detail:   strings.TrimSpace(err.Error()),
			Fix:      sshAgentFix,
		}
	}
	if n == 0 {
		return preflight.Problem{
			Severity: preflight.Warn,
			Title:    "ssh agent reachable but has no keys",
			Fix:      sshAgentFix,
		}
	}
	return preflight.Problem{
		Severity: preflight.OK,
		Title:    fmt.Sprintf("host ssh agent: %d key(s)", n),
	}
}

func checkSSHAuthSockKind() preflight.Problem {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return preflight.Problem{
			Severity: preflight.Warn,
			Title:    "SSH_AUTH_SOCK not set in this shell",
			Fix:      sshAgentFix,
		}
	}
	if strings.HasPrefix(sock, launchdSockPrefix) {
		return preflight.Problem{
			Severity: preflight.Warn,
			Title:    "SSH_AUTH_SOCK points at macOS's launchd default agent",
			Detail:   sock + "  (host `git` may still work via ~/.ssh/config IdentityAgent, but Lima only forwards $SSH_AUTH_SOCK)",
			Fix:      sshAgentFix,
		}
	}
	return preflight.Problem{
		Severity: preflight.OK,
		Title:    "SSH_AUTH_SOCK points at a custom agent socket",
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

func checkInVMAgent() preflight.Problem {
	hostN, hostErr := hostAgentKeyCount()
	out, err := exec.Command("limactl", "shell", vmName, "--", "ssh-add", "-l").Output()
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
	case vmN == 0 && hostErr == nil && hostN > 0:
		return preflight.Problem{
			Severity: preflight.Fail,
			Title:    "in-VM ssh agent is empty (host has " + fmt.Sprint(hostN) + ")",
			Detail:   "Lima's hostagent forwarded an empty agent — it was started in a shell where SSH_AUTH_SOCK pointed at the launchd default.",
			Fix:      sshAgentFix,
		}
	case hostErr == nil && vmN != hostN:
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

// sshAgentFix is the multi-line fix block we attach to every check whose
// remediation is the same: get $SSH_AUTH_SOCK pointing at a real agent, then
// bounce the VM so its hostagent rebuilds the forwarding.
const sshAgentFix = `if you use 1Password, add to your shell rc:
         export SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
       then bounce the VM so its hostagent picks up the new socket:
         limactl stop ` + vmName + ` && limactl start ` + vmName + `
       see CONTAINER-ISOLATION.md for non-1Password agents.`

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
