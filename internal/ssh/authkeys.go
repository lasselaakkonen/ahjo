package ssh

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// WriteAuthorizedKeys assembles <slugDir>/authorized_keys from two sources:
// the forwarded ssh-agent (preferred — keeps key material off the VM disk
// and matches CONTAINER-ISOLATION.md's "no ~/.ssh/ crosses" rule) and
// ~/.ssh/*.pub (fallback for native-Linux runs without an agent). On Lima
// it also includes the Mac host's ~/.ssh/*.pub via the virtiofs mount, so
// the user's Mac-side key authenticates without agent dependency. Dedupes
// by key bytes so a key loaded from multiple sources lands once.
//
// Writes in place (O_TRUNC) rather than via tempfile+rename so incus
// single-file bind mounts (path /home/code/.ssh/authorized_keys) keep
// pointing at the same inode and observe the new content live.
func WriteAuthorizedKeys(slugDir string) error {
	body, sources, err := collectAuthorizedKeys()
	if err != nil {
		return err
	}
	if body == "" {
		return fmt.Errorf("no public keys available: %s; load a key into your ssh-agent (1Password etc.) or run `ssh-keygen -t ed25519`", strings.Join(sources, "; "))
	}
	dst := filepath.Join(slugDir, "authorized_keys")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// collectAuthorizedKeys merges agent and file-backed public keys, dedupes,
// and returns the concatenated body plus a per-source status (used in the
// error message when nothing was found).
func collectAuthorizedKeys() (body string, sources []string, err error) {
	seen := map[string]bool{}
	var b strings.Builder

	agentLines, agentStatus := agentPublicKeys()
	sources = append(sources, "ssh-agent: "+agentStatus)
	for _, line := range agentLines {
		appendUnique(&b, seen, line)
	}

	fileLines, fileStatus, err := filePublicKeys()
	if err != nil {
		return "", nil, err
	}
	sources = append(sources, "~/.ssh/*.pub: "+fileStatus)
	for _, line := range fileLines {
		appendUnique(&b, seen, line)
	}

	return b.String(), sources, nil
}

// agentPublicKeys runs `ssh-add -L` against $SSH_AUTH_SOCK. Returns the
// public-key lines and a short status string for diagnostics. Never errors:
// a missing/empty/unreachable agent simply yields zero lines.
func agentPublicKeys() ([]string, string) {
	if _, err := exec.LookPath("ssh-add"); err != nil {
		return nil, "ssh-add not on PATH"
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		return nil, "SSH_AUTH_SOCK not set"
	}
	out, err := exec.Command("ssh-add", "-L").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			switch ee.ExitCode() {
			case 1:
				return nil, "agent reachable but empty"
			case 2:
				return nil, "agent unreachable"
			}
		}
		return nil, "ssh-add failed: " + strings.TrimSpace(err.Error())
	}
	lines := splitNonEmpty(string(out))
	return lines, fmt.Sprintf("%d key(s)", len(lines))
}

// filePublicKeys reads ~/.ssh/*.pub plus, when running inside the Lima VM,
// the Mac host's ~/.ssh/*.pub via the virtiofs mount. Each file may contain
// multiple keys; we split on lines so dedup works against agent output.
func filePublicKeys() ([]string, string, error) {
	homes, err := pubKeyHomes()
	if err != nil {
		return nil, "", err
	}
	var (
		lines    []string
		statuses []string
	)
	for _, h := range homes {
		matches, err := filepath.Glob(filepath.Join(h.dir, ".ssh", "*.pub"))
		if err != nil {
			return nil, "", fmt.Errorf("glob %s/.ssh/*.pub: %w", h.dir, err)
		}
		sort.Strings(matches)
		for _, m := range matches {
			c, err := os.ReadFile(m)
			if err != nil {
				return nil, "", fmt.Errorf("read %s: %w", m, err)
			}
			lines = append(lines, splitNonEmpty(string(c))...)
		}
		statuses = append(statuses, fmt.Sprintf("%s: %d file(s)", h.label, len(matches)))
	}
	return lines, strings.Join(statuses, ", "), nil
}

type homeDir struct{ dir, label string }

func pubKeyHomes() ([]homeDir, error) {
	var hs []homeDir
	if h, err := os.UserHomeDir(); err == nil {
		hs = append(hs, homeDir{dir: h, label: "~/.ssh"})
	} else {
		return nil, err
	}
	if mac, ok := paths.MacHostHome(); ok && mac != hs[0].dir {
		hs = append(hs, homeDir{dir: mac, label: mac + "/.ssh"})
	}
	return hs, nil
}

// appendUnique writes line to b, keyed on type+base64 so two entries that
// differ only in trailing comment dedupe. Returns whether a new key landed.
func appendUnique(b *strings.Builder, seen map[string]bool, line string) bool {
	key := dedupeKey(line)
	if key == "" || seen[key] {
		return false
	}
	seen[key] = true
	b.WriteString(strings.TrimRight(line, "\n"))
	b.WriteByte('\n')
	return true
}

// dedupeKey returns "<type> <base64>" — the part of an authorized_keys line
// that identifies the key. Returns "" for malformed lines (skipped).
func dedupeKey(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[0] + " " + fields[1]
}

func splitNonEmpty(s string) []string {
	var out []string
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		if t := strings.TrimSpace(scan.Text()); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// WriteKnownHosts builds a per-slug known_hosts pinning host:port to the
// generated host pubkeys, so port reuse after rm doesn't trigger warnings.
func WriteKnownHosts(slugDir string, port int) error {
	var lines []string
	for _, name := range []string{"ssh_host_ed25519_key.pub", "ssh_host_rsa_key.pub"} {
		c, err := os.ReadFile(filepath.Join(slugDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		entry := strings.TrimRight(string(c), "\n")
		lines = append(lines, fmt.Sprintf("[127.0.0.1]:%d %s", port, entry))
	}
	dst := filepath.Join(slugDir, "known_hosts")
	tmp, err := os.CreateTemp(slugDir, ".known_hosts.tmp.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
