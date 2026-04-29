// Package ssh handles per-slug host keys, authorized_keys assembly,
// per-slug known_hosts, and atomic ssh-config regeneration.
package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureHostKeys generates ed25519 + rsa host keys in dir if missing.
func EnsureHostKeys(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := genIfMissing(dir, "ssh_host_ed25519_key", "ed25519", ""); err != nil {
		return err
	}
	return genIfMissing(dir, "ssh_host_rsa_key", "rsa", "4096")
}

func genIfMissing(dir, name, keyType, bits string) error {
	priv := filepath.Join(dir, name)
	if _, err := os.Stat(priv); err == nil {
		return nil
	}
	args := []string{"-t", keyType, "-N", "", "-q", "-f", priv}
	if bits != "" {
		args = append([]string{"-b", bits}, args...)
	}
	cmd := exec.Command("ssh-keygen", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh-keygen %s: %w", priv, err)
	}
	return os.Chmod(priv, 0o600)
}
