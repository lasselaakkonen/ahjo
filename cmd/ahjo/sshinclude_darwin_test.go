//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureSSHInclude_CreatesFileWhenMissing(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "config")

	added, err := ensureSSHIncludeAt(path, home)
	if err != nil {
		t.Fatalf("ensureSSHIncludeAt: %v", err)
	}
	if !added {
		t.Fatalf("expected added=true when file missing")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after ensure: %v", err)
	}
	if !strings.Contains(string(b), sshIncludeBody) {
		t.Fatalf("file missing Include body:\n%s", b)
	}
	if !strings.Contains(string(b), sshIncludeBeginMarker) || !strings.Contains(string(b), sshIncludeEndMarker) {
		t.Fatalf("file missing markers:\n%s", b)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o, want 0600", st.Mode().Perm())
	}
}

func TestEnsureSSHInclude_InsertsAboveFirstHost(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	pre := "Host myserver\n  Hostname example.com\n"
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	added, err := ensureSSHIncludeAt(path, home)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !added {
		t.Fatalf("expected added=true")
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	bIdx := strings.Index(got, sshIncludeBeginMarker)
	hostIdx := strings.Index(got, "Host myserver")
	if bIdx < 0 || hostIdx < 0 || bIdx >= hostIdx {
		t.Fatalf("ahjo block must sit ABOVE the first Host directive — Include lines below any Host pattern silently no-op; got:\n%s", got)
	}
	if !strings.Contains(got, "Host myserver") {
		t.Fatalf("user content lost; got:\n%s", got)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode not preserved: got %o, want 0644", st.Mode().Perm())
	}
}

// TestEnsureSSHInclude_PreservesLeadingIncludesAndCommentsAboveHost mirrors
// the OrbStack-style layout most users have: top-of-file comments and
// pre-existing top-level Include directives, then Host blocks. Our block
// must slot in AFTER the existing Includes/comments but BEFORE the first
// Host directive.
func TestEnsureSSHInclude_PreservesLeadingIncludesAndCommentsAboveHost(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	pre := "# Added by OrbStack\nInclude ~/.orbstack/ssh/config\n\nHost github.com\n  User git\n"
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureSSHIncludeAt(path, home); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	orbIdx := strings.Index(got, "Include ~/.orbstack")
	ahjoIdx := strings.Index(got, sshIncludeBeginMarker)
	hostIdx := strings.Index(got, "Host github.com")
	if orbIdx < 0 || ahjoIdx < 0 || hostIdx < 0 {
		t.Fatalf("missing one of OrbStack Include / ahjo marker / Host directive; got:\n%s", got)
	}
	if !(orbIdx < ahjoIdx && ahjoIdx < hostIdx) {
		t.Fatalf("ordering wrong — want OrbStack < ahjo < Host; got OrbStack@%d ahjo@%d Host@%d\n%s", orbIdx, ahjoIdx, hostIdx, got)
	}
}

func TestEnsureSSHInclude_AppendsWhenNoHostDirective(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	pre := "# user comment only, no Host blocks yet\n"
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureSSHIncludeAt(path, home); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.HasPrefix(got, pre) {
		t.Fatalf("user comment displaced; got:\n%s", got)
	}
	if !strings.Contains(got, sshIncludeBeginMarker) {
		t.Fatalf("block missing:\n%s", got)
	}
}

func TestEnsureSSHInclude_Idempotent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "config")
	if _, err := ensureSSHIncludeAt(path, home); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	added, err := ensureSSHIncludeAt(path, home)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Fatalf("second ensure returned added=true; expected no-op")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("content changed on repeat call:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestEnsureSSHInclude_DetectsManualTilde(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	pre := "Include ~/.ahjo-shared/ssh-config\n"
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := sshIncludeStatusAt(path, home)
	if err != nil {
		t.Fatal(err)
	}
	if state != sshIncludePresentManual {
		t.Fatalf("state = %v, want sshIncludePresentManual", state)
	}

	added, err := ensureSSHIncludeAt(path, home)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Fatalf("ensure added our marker block even though manual Include is present")
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), sshIncludeBeginMarker) {
		t.Fatalf("file gained ahjo markers despite manual Include:\n%s", b)
	}
}

func TestEnsureSSHInclude_DetectsManualAbsolutePath(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	abs := filepath.Join(home, ".ahjo-shared", "ssh-config")
	pre := "Include " + abs + "\n"
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := sshIncludeStatusAt(path, home)
	if err != nil {
		t.Fatal(err)
	}
	if state != sshIncludePresentManual {
		t.Fatalf("absolute-path manual Include not detected; state=%v", state)
	}
}

func TestRemoveSSHInclude_StripsBlock(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	pre := "Host myserver\n  Hostname example.com\n"
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSSHIncludeAt(path, home); err != nil {
		t.Fatal(err)
	}

	removed, err := removeSSHIncludeAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatalf("expected removed=true")
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if strings.Contains(got, sshIncludeBeginMarker) || strings.Contains(got, sshIncludeBody) {
		t.Fatalf("block survived removal:\n%s", got)
	}
	if !strings.Contains(got, "Host myserver") {
		t.Fatalf("user content lost during removal:\n%s", got)
	}
}

func TestRemoveSSHInclude_NoOpWhenAbsent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "config")
	removed, err := removeSSHIncludeAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatalf("expected removed=false when file missing")
	}
}

func TestRemoveSSHInclude_LeavesManualLineAlone(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "config")
	pre := "Include ~/.ahjo-shared/ssh-config\n"
	if err := os.WriteFile(path, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := removeSSHIncludeAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatalf("manual Include line was treated as ours")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "Include ~/.ahjo-shared/ssh-config") {
		t.Fatalf("manual Include line was removed:\n%s", b)
	}
}
