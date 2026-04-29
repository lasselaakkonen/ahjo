package ssh

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteAuthorizedKeys concatenates every *.pub in ~/.ssh/ into <slugDir>/authorized_keys.
func WriteAuthorizedKeys(slugDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	if err != nil {
		return fmt.Errorf("glob ~/.ssh/*.pub: %w", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return fmt.Errorf("no public keys found in %s/.ssh/*.pub; create one with `ssh-keygen -t ed25519`", home)
	}
	var b strings.Builder
	for _, m := range matches {
		c, err := os.ReadFile(m)
		if err != nil {
			return fmt.Errorf("read %s: %w", m, err)
		}
		b.Write(c)
		if !strings.HasSuffix(string(c), "\n") {
			b.WriteByte('\n')
		}
	}
	dst := filepath.Join(slugDir, "authorized_keys")
	tmp, err := os.CreateTemp(slugDir, ".authorized_keys.tmp.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
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
