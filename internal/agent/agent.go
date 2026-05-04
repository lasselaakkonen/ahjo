// Package agent detects which ssh-agent socket on the host should be
// forwarded into the Lima VM. macOS's default $SSH_AUTH_SOCK points at a
// launchd-managed agent that's empty unless the user opted into Keychain
// integration; meanwhile real keys typically live behind 1Password,
// Secretive, gpg-agent, etc., which the user reaches via ~/.ssh/config
// IdentityAgent — a hop Lima doesn't honor when forwarding. This package
// probes the well-known agent sockets, lets `ahjo init` pick one, and
// resolves the chosen socket at every limactl invocation so the VM sees
// the right keys without the user having to edit their shell rc.
package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// LaunchdSockPrefix is the path prefix macOS uses for $SSH_AUTH_SOCK when
// nothing else has set it — a launchd-provided agent that's empty by default.
const LaunchdSockPrefix = "/private/tmp/com.apple.launchd."

// Candidate is one ssh-agent socket the user could choose.
type Candidate struct {
	Label  string // human-readable, e.g. "1Password"
	Socket string // absolute path to the unix socket
	Keys   int    // number of keys reported by `ssh-add -l`
}

// Detect probes the well-known agent sockets and returns those that are
// reachable and have at least one key loaded. Candidates are ordered so
// that the most likely intent comes first (current shell, then 1P, then
// Secretive, then gpg-agent, then anything in ~/.ssh/config).
func Detect() []Candidate {
	var cands []Candidate
	seen := map[string]bool{}

	add := func(label, sock string) {
		sock = paths.Expand(sock)
		if sock == "" || seen[sock] {
			return
		}
		seen[sock] = true
		if !isSocket(sock) {
			return
		}
		n, ok := agentKeyCount(sock)
		if !ok || n == 0 {
			return
		}
		cands = append(cands, Candidate{Label: label, Socket: sock, Keys: n})
	}

	if s := os.Getenv("SSH_AUTH_SOCK"); s != "" && !strings.HasPrefix(s, LaunchdSockPrefix) {
		add("current shell ($SSH_AUTH_SOCK)", s)
	}
	add("1Password", "~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock")
	add("Secretive", "~/Library/Containers/at.maxgoedjen.Secretive.SecretAgent/Data/socket.ssh")
	if s := gpgAgentSock(); s != "" {
		add("gpg-agent", s)
	}
	if s := identityAgentFromSSHConfig(); s != "" {
		add("ssh_config IdentityAgent", s)
	}
	return cands
}

// Configured returns the socket path persisted in ~/.ahjo/config.toml's
// [mac] section, or "" when nothing's configured / the config can't load.
func Configured() string {
	c, err := config.Load()
	if err != nil {
		return ""
	}
	return c.Mac.SSHAuthSock
}

// Resolve picks the agent socket ahjo should forward into the VM. Order:
//  1. The configured socket if it still has keys.
//  2. The single Detect() candidate, if there's exactly one.
//  3. Error — caller should point the user at `ahjo init` / `ahjo doctor`.
//
// Returns (socket, label, error). When the configured socket is set but
// no longer reachable, Resolve falls through to detection rather than
// returning a stale path; if detection finds nothing, it errors.
func Resolve() (string, string, error) {
	if cfg := Configured(); cfg != "" {
		if n, ok := agentKeyCount(cfg); ok && n > 0 {
			return cfg, labelForSocket(cfg), nil
		}
	}
	cands := Detect()
	switch len(cands) {
	case 0:
		return "", "", errors.New("no ssh-agent with keys was found on the host; load a key into 1Password / your agent and run `ahjo init`")
	case 1:
		return cands[0].Socket, cands[0].Label, nil
	default:
		return "", "", fmt.Errorf("multiple ssh-agents detected (%s); run `ahjo init` to choose one", labelsOf(cands))
	}
}

// labelForSocket maps a known socket path back to a human-readable agent
// name. Falls back to "configured" for paths we don't recognize (custom
// agents, ssh_config IdentityAgent values, etc.).
func labelForSocket(sock string) string {
	switch {
	case strings.Contains(sock, "2BUA8C4S2C.com.1password"):
		return "1Password"
	case strings.Contains(sock, "at.maxgoedjen.Secretive"):
		return "Secretive"
	case strings.Contains(sock, "/.gnupg/") || strings.HasSuffix(sock, "S.gpg-agent.ssh"):
		return "gpg-agent"
	}
	return "configured"
}

func labelsOf(cs []Candidate) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Label
	}
	return strings.Join(out, ", ")
}

// agentKeyCount runs `ssh-add -l` against sock. Returns (n, true) on success
// (n=0 means agent reachable but empty) or (0, false) when ssh-add can't be
// run or the agent is unreachable.
func agentKeyCount(sock string) (int, bool) {
	if _, err := exec.LookPath("ssh-add"); err != nil {
		return 0, false
	}
	cmd := exec.Command("ssh-add", "-l")
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	out, err := cmd.Output()
	if err == nil {
		return countLines(string(out)), true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return 0, true // reachable, empty
	}
	return 0, false
}

func isSocket(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeSocket != 0
}

func countLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// gpgAgentSock returns gpgconf's reported ssh-agent socket, or "" when
// gpg isn't installed or doesn't expose one.
func gpgAgentSock() string {
	if _, err := exec.LookPath("gpgconf"); err != nil {
		return ""
	}
	out, err := exec.Command("gpgconf", "--list-dirs", "agent-ssh-socket").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// identityAgentFromSSHConfig asks `ssh -G` for the effective IdentityAgent
// for github.com (the host the user is most likely cloning from). Returns
// "" when ssh isn't on PATH, the directive isn't set, or it points at the
// special value "none". The path is returned with ~ expanded.
func identityAgentFromSSHConfig() string {
	if _, err := exec.LookPath("ssh"); err != nil {
		return ""
	}
	out, err := exec.Command("ssh", "-G", "github.com").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "identityagent") {
			continue
		}
		v := strings.Trim(strings.Join(f[1:], " "), `"`)
		if v == "" || strings.EqualFold(v, "none") || strings.EqualFold(v, "SSH_AUTH_SOCK") {
			return ""
		}
		return paths.Expand(v)
	}
	return ""
}
