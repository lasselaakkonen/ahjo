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
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/agent"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/macsecret"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/preflight"
)

// runMacDoctor prints the host half of `ahjo doctor` and returns true if any
// check failed. Output style matches preflight.Format so the merged stdout
// from this block plus the relayed in-VM block reads as one report.
//
// When fix is true and the in-VM ssh-agent state mismatches what the host
// has (empty, unreachable, or fewer keys than the host), we close the
// cached Lima ssh ControlMaster and re-run the in-VM agent check. That's
// the only auto-fix wired in today; everything else prints its hint as
// usual. We only fix when the host agent itself has keys — closing the
// master while the host is locked would just rebind to nothing.
func runMacDoctor(w io.Writer, fix bool) bool {
	fmt.Fprintln(w, "[host-side checks]")
	hostP := checkAgentConfigured()
	ps := []preflight.Problem{
		hostP,
		checkSSHAuthSockKind(),
		checkKeychainPATs(),
	}
	vmP, runVMCheck := checkVMRunning()
	ps = append(ps, vmP)
	vmAgentIdx := -1
	if runVMCheck {
		vmAgentIdx = len(ps)
		ps = append(ps, checkInVMAgent())
	}
	fixed := false
	if fix && runVMCheck && hostP.Severity == preflight.OK && isStaleMasterSymptom(ps[vmAgentIdx]) {
		fp := fixStaleAgentForward()
		ps = append(ps, fp)
		fixed = fp.Severity == preflight.OK
	}
	for _, p := range ps {
		fmt.Fprintln(w, preflight.Format(p))
	}
	fmt.Fprintln(w)
	// When the fix succeeded, the pre-fix failure is superseded by the
	// post-fix OK; report exit-status against the current state, not
	// the historical one. Keep the [fail] line in the output so the
	// user can see what was wrong, but don't propagate it as an error.
	if fixed && vmAgentIdx >= 0 {
		ps[vmAgentIdx].Severity = preflight.OK
	}
	return preflight.Worst(ps) >= preflight.Fail
}

// isStaleMasterSymptom reports whether p looks like a Lima ssh
// ControlMaster pinned to a stale/wrong/no agent — the case where
// CloseSSHControlMaster is the right hammer. Three observed shapes:
//
//   - "in-VM ssh agent is empty (host has N)" — master pinned to an
//     empty agent (e.g. the launchd default at master-creation time).
//   - "in-VM ssh agent: M key(s) (host has N)" with M < N — master
//     pinned to a different/partial agent than the host now has.
//   - "could not query in-VM ssh agent" — master pinned to no
//     forwarding at all (e.g. SSH_AUTH_SOCK="" at creation, after
//     Change A's deterministic-empty path), so ssh-add in the VM
//     can't reach any agent.
//
// We only fire the fix when checkAgentConfigured() came back OK on the
// host side — see runMacDoctor — so a no-op rebind is impossible.
func isStaleMasterSymptom(p preflight.Problem) bool {
	if p.Severity == preflight.OK {
		return false
	}
	switch {
	case strings.HasPrefix(p.Title, "in-VM ssh agent is empty"):
		return true
	case strings.HasPrefix(p.Title, "in-VM ssh agent:") && strings.Contains(p.Title, "host has"):
		return true
	case strings.HasPrefix(p.Title, "could not query in-VM ssh agent"):
		return true
	}
	return false
}

// fixStaleAgentForward closes the Lima ssh ControlMaster (no-op when
// none is running) and re-runs the in-VM agent check. The new Problem
// summarizes the action and the post-fix state. Mac-only — the VM
// can't close the host's master from the inside.
func fixStaleAgentForward() preflight.Problem {
	if err := lima.CloseSSHControlMaster(vmName); err != nil {
		return preflight.Problem{
			Severity: preflight.Fail,
			Title:    "could not close stale ssh ControlMaster",
			Detail:   err.Error(),
			Fix:      "limactl stop " + vmName + " && limactl start " + vmName,
		}
	}
	post := checkInVMAgent()
	if post.Severity == preflight.OK {
		return preflight.Problem{
			Severity: preflight.OK,
			Title:    "fix applied: closed ssh ControlMaster, " + post.Title,
		}
	}
	return preflight.Problem{
		Severity: post.Severity,
		Title:    "fix applied: closed ssh ControlMaster, but " + post.Title,
		Detail:   post.Detail,
		Fix:      post.Fix,
	}
}

// checkKeychainPATs surveys per-repo PAT coverage in the macOS login
// Keychain. It reads the in-VM-written <SharedDir>/repo-aliases file to
// enumerate registered repo slugs and probes (without `-w`, so values are
// never read) each one's GH_TOKEN row. A miss means the user added the repo
// without pasting a PAT, or deleted it manually from Keychain Access.app.
//
// The "[ok]" baseline is "every registered repo has a Keychain entry, OR
// none are registered yet"; one or more misses is a Warn (matching the
// Linux-side "no GH_TOKEN" wording) rather than Fail because public-repo
// flows + ssh-agent still work.
func checkKeychainPATs() preflight.Problem {
	slugs, err := readRepoSlugsFromFile(paths.RepoAliasesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return preflight.Problem{Severity: preflight.OK, Title: "no repos registered; Keychain survey skipped"}
		}
		return preflight.Problem{
			Severity: preflight.Warn,
			Title:    "could not read repo-aliases for Keychain survey",
			Detail:   err.Error(),
		}
	}
	if len(slugs) == 0 {
		return preflight.Problem{Severity: preflight.OK, Title: "no repos registered; Keychain survey skipped"}
	}
	var present, missing []string
	for _, s := range slugs {
		ok, err := macsecret.Probe(s, ghTokenKey)
		if err != nil {
			return preflight.Problem{
				Severity: preflight.Fail,
				Title:    "could not probe Keychain for per-repo PATs",
				Detail:   err.Error(),
				Fix:      "unlock the login keychain in Keychain Access, then re-run `ahjo doctor`",
			}
		}
		if ok {
			present = append(present, s)
		} else {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return preflight.Problem{
			Severity: preflight.OK,
			Title:    fmt.Sprintf("per-repo PAT in Keychain for all %d repo(s)", len(present)),
		}
	}
	return preflight.Problem{
		Severity: preflight.Warn,
		Title:    fmt.Sprintf("per-repo PAT in Keychain: %d of %d repo(s)", len(present), len(slugs)),
		Detail:   "missing: " + strings.Join(missing, ", "),
		Fix:      "ahjo repo set-token <alias>  # prompt + Keychain write",
	}
}

// readRepoSlugsFromFile parses <SharedDir>/repo-aliases for unique repo slugs.
// Returns them in deterministic (alphabetic) order so doctor output is stable
// across runs.
func readRepoSlugsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		seen[parts[1]] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
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
