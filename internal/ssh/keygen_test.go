package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEnsureLocalKey_GeneratesWhenMissing(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	pub, created, err := EnsureLocalKey()
	if err != nil {
		t.Fatalf("EnsureLocalKey: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on empty HOME")
	}
	want := filepath.Join(home, ".ssh", "id_ed25519.pub")
	if pub != want {
		t.Fatalf("pub path = %q, want %q", pub, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "id_ed25519")); err != nil {
		t.Fatalf("private key not created: %v", err)
	}
	if _, err := os.Stat(pub); err != nil {
		t.Fatalf("public key not created: %v", err)
	}
}

func TestEnsureLocalKey_NoopWhenAnyIDKeyExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(sshDir, "id_rsa")
	if err := os.WriteFile(existing, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime0 := mustStat(t, existing).ModTime()

	pub, created, err := EnsureLocalKey()
	if err != nil {
		t.Fatalf("EnsureLocalKey: %v", err)
	}
	if created {
		t.Fatalf("expected created=false when id_rsa already present")
	}
	if pub != existing {
		t.Fatalf("pub path = %q, want %q (should reuse the existing key)", pub, existing)
	}
	if !mustStat(t, existing).ModTime().Equal(mtime0) {
		t.Fatalf("existing key was modified")
	}
	if _, err := os.Stat(filepath.Join(sshDir, "id_ed25519")); !os.IsNotExist(err) {
		t.Fatalf("ed25519 key generated despite id_rsa being present (err=%v)", err)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi
}
