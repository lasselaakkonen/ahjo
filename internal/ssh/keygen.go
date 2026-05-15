package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureLocalKey ensures the user has at least one usable client SSH key
// at ~/.ssh/id_ed25519{,.pub}. When ~/.ssh contains no id_* private key
// at all it generates a passphrase-less ed25519 key; when any id_* key
// already exists it is a no-op (we never overwrite, never inspect agent
// state, never touch keys the user named themselves).
//
// Rationale: WriteAuthorizedKeys needs at least one pubkey source on the
// host running ahjo. Mac/Lima get this for free via the virtiofs window
// at paths.MacHostHome() — but that window does not propagate into Incus
// containers. Any layer beyond Lima must therefore have its own keypair,
// or it cannot author a child's authorized_keys. Auto-keygen is the
// minimal fix; users with their own conventions are untouched because
// the trigger only fires when ~/.ssh has no id_* private key at all.
func EnsureLocalKey() (pubPath string, created bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve $HOME: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	if existing, ok, err := findExistingIDKey(sshDir); err != nil {
		return "", false, err
	} else if ok {
		return existing, false, nil
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", sshDir, err)
	}
	priv := filepath.Join(sshDir, "id_ed25519")
	comment := "ahjo-" + safeHostname()
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", priv, "-C", comment)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("ssh-keygen %s: %w", priv, err)
	}
	if err := os.Chmod(priv, 0o600); err != nil {
		return "", false, err
	}
	return priv + ".pub", true, nil
}

// findExistingIDKey reports any ~/.ssh/id_* private key (the matching
// .pub need not exist — ssh-keygen -y can derive it, and pubKeyHomes
// already only cares about *.pub presence). Glob is lexicographic; we
// pick the first hit deterministically.
func findExistingIDKey(sshDir string) (string, bool, error) {
	matches, err := filepath.Glob(filepath.Join(sshDir, "id_*"))
	if err != nil {
		return "", false, fmt.Errorf("glob %s/id_*: %w", sshDir, err)
	}
	for _, m := range matches {
		if filepath.Ext(m) == ".pub" {
			continue
		}
		return m, true, nil
	}
	return "", false, nil
}

func safeHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "host"
	}
	return h
}
