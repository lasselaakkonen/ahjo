package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAuthorizedKeys_StagesRelayDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "") // disable agent source for deterministic test

	const (
		key1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItestkey1 user@a"
		key2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItestkey2 user@b"
	)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(key1+"\n"+key2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	slug := t.TempDir()
	if err := WriteAuthorizedKeys(slug); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	auth, err := os.ReadFile(filepath.Join(slug, "authorized_keys"))
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	for _, want := range []string{key1, key2} {
		if !strings.Contains(string(auth), want) {
			t.Errorf("authorized_keys missing %q\n--- got ---\n%s", want, auth)
		}
	}

	relay := filepath.Join(slug, "ancestor-pubkeys")
	entries, err := os.ReadDir(relay)
	if err != nil {
		t.Fatalf("read relay dir: %v", err)
	}
	if got := len(entries); got != 2 {
		t.Errorf("relay dir has %d entries, want 2", got)
	}
	var relayContents []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pub") {
			t.Errorf("relay entry %q does not have .pub suffix", e.Name())
		}
		b, err := os.ReadFile(filepath.Join(relay, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		relayContents = append(relayContents, strings.TrimSpace(string(b)))
	}
	joined := strings.Join(relayContents, "\n")
	for _, want := range []string{key1, key2} {
		if !strings.Contains(joined, want) {
			t.Errorf("relay missing %q\n--- got ---\n%s", want, joined)
		}
	}
}

func TestWriteAuthorizedKeys_DedupesAcrossSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIdupkey user@host"
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Same key in two different files — should land once.
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(key+" comment-A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_alt.pub"), []byte(key+" comment-B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	slug := t.TempDir()
	if err := WriteAuthorizedKeys(slug); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	auth, err := os.ReadFile(filepath.Join(slug, "authorized_keys"))
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if got := strings.Count(string(auth), "AAAAC3NzaC1lZDI1NTE5AAAAIdupkey"); got != 1 {
		t.Errorf("expected 1 instance of the key body, got %d\n%s", got, auth)
	}

	entries, err := os.ReadDir(filepath.Join(slug, "ancestor-pubkeys"))
	if err != nil {
		t.Fatalf("read relay dir: %v", err)
	}
	if got := len(entries); got != 1 {
		t.Errorf("relay has %d entries, want 1 (dedup)", got)
	}
}

func TestWriteAuthorizedKeys_FailsWhenNoSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	slug := t.TempDir()
	err := WriteAuthorizedKeys(slug)
	if err == nil {
		t.Fatalf("expected error when no key sources are available")
	}
	if !strings.Contains(err.Error(), "no public keys available") {
		t.Errorf("error message changed: %v", err)
	}
}

func TestWriteAuthorizedKeys_ClearsStaleRelayFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIfreshkey user@host"
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	slug := t.TempDir()
	// Pre-populate stale entry.
	relay := filepath.Join(slug, "ancestor-pubkeys")
	if err := os.MkdirAll(relay, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relay, "stale.pub"), []byte("ssh-ed25519 STALE comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteAuthorizedKeys(slug); err != nil {
		t.Fatalf("WriteAuthorizedKeys: %v", err)
	}

	if _, err := os.Stat(filepath.Join(relay, "stale.pub")); !os.IsNotExist(err) {
		t.Fatalf("stale relay file survived; err=%v", err)
	}
}
