package ssh

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// WriteAuthorizedKeys assembles <slugDir>/authorized_keys from three
// sources: the forwarded ssh-agent (preferred — keeps key material off
// the VM disk and matches CONTAINER-ISOLATION.md's "no ~/.ssh/ crosses"
// rule), ~/.ssh/*.pub on the host running ahjo (also includes the Mac
// host's ~/.ssh/*.pub via the virtiofs mount when running inside Lima),
// and the ancestor-pubkey relay at paths.AncestorPubkeysMount — which is
// populated by the parent ahjo layer when nesting (ahjo-in-ahjo).
// Dedupes by key bytes so a key loaded from multiple sources lands once.
//
// In addition to authorized_keys, the same dedup'd set is staged as one
// .pub file per key under <slugDir>/ancestor-pubkeys/. That directory is
// read-only bind-mounted into the new container at
// paths.AncestorPubkeysMount, so the child's ahjo (if any) sees this
// layer's pubkey set when it goes to author its own grandchild's
// authorized_keys. This is what makes the recursion self-sustaining at
// arbitrary depth — no layer relies on a magic Mac virtiofs window.
//
// Writes authorized_keys in place (O_TRUNC) rather than via
// tempfile+rename so incus single-file bind mounts (path
// /home/ubuntu/.ssh/authorized_keys) keep pointing at the same inode and
// observe the new content live.
func WriteAuthorizedKeys(slugDir string) error {
	body, lines, sources, err := collectAuthorizedKeys()
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
	if err := f.Close(); err != nil {
		return err
	}
	return writeAncestorRelay(slugDir, lines)
}

// writeAncestorRelay materialises one .pub file per pubkey under
// <slugDir>/ancestor-pubkeys/, named by a stable hash of the key body so
// the directory is idempotent across re-runs and naturally dedupes if
// called twice with overlapping sets. Stale files from previous runs are
// removed first so a key removed from the user's agent / ~/.ssh doesn't
// linger.
func writeAncestorRelay(slugDir string, lines []string) error {
	relay := filepath.Join(slugDir, "ancestor-pubkeys")
	if err := os.RemoveAll(relay); err != nil {
		return fmt.Errorf("clear %s: %w", relay, err)
	}
	if err := os.MkdirAll(relay, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", relay, err)
	}
	for _, line := range lines {
		key := dedupeKey(line)
		if key == "" {
			continue
		}
		sum := sha256.Sum256([]byte(key))
		name := hex.EncodeToString(sum[:8]) + ".pub"
		if err := os.WriteFile(filepath.Join(relay, name), []byte(line+"\n"), 0o644); err != nil {
			return fmt.Errorf("write %s/%s: %w", relay, name, err)
		}
	}
	return nil
}

// collectAuthorizedKeys merges agent, file-backed, and ancestor-relay
// public keys, dedupes, and returns the concatenated body, the deduped
// lines (in insertion order, for relay staging), and a per-source status
// string (used in the error message when nothing was found).
func collectAuthorizedKeys() (body string, lines []string, sources []string, err error) {
	seen := map[string]bool{}
	var b strings.Builder

	agentLines, agentStatus := agentPublicKeys()
	sources = append(sources, "ssh-agent: "+agentStatus)
	for _, line := range agentLines {
		if appendUnique(&b, seen, line) {
			lines = append(lines, line)
		}
	}

	fileLines, fileStatus, err := filePublicKeys()
	if err != nil {
		return "", nil, nil, err
	}
	sources = append(sources, "~/.ssh/*.pub: "+fileStatus)
	for _, line := range fileLines {
		if appendUnique(&b, seen, line) {
			lines = append(lines, line)
		}
	}

	relayLines, relayStatus, err := relayPublicKeys()
	if err != nil {
		return "", nil, nil, err
	}
	sources = append(sources, "ancestor-pubkeys: "+relayStatus)
	for _, line := range relayLines {
		if appendUnique(&b, seen, line) {
			lines = append(lines, line)
		}
	}

	return b.String(), lines, sources, nil
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

// relayPublicKeys reads the ancestor-pubkey relay mounted by the parent
// ahjo layer at paths.AncestorPubkeysMount. Absent on the topmost layer
// (Mac/Lima); present from layer 2 down once Piece B has wired the disk
// device. Same multi-line-per-file split as filePublicKeys so dedup
// works uniformly across all three sources.
func relayPublicKeys() ([]string, string, error) {
	if _, err := os.Stat(paths.AncestorPubkeysMount); err != nil {
		if os.IsNotExist(err) {
			return nil, "not mounted (topmost layer)", nil
		}
		return nil, "", fmt.Errorf("stat %s: %w", paths.AncestorPubkeysMount, err)
	}
	matches, err := filepath.Glob(filepath.Join(paths.AncestorPubkeysMount, "*.pub"))
	if err != nil {
		return nil, "", fmt.Errorf("glob %s/*.pub: %w", paths.AncestorPubkeysMount, err)
	}
	sort.Strings(matches)
	var lines []string
	for _, m := range matches {
		c, err := os.ReadFile(m)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", m, err)
		}
		lines = append(lines, splitNonEmpty(string(c))...)
	}
	return lines, fmt.Sprintf("%d file(s)", len(matches)), nil
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
