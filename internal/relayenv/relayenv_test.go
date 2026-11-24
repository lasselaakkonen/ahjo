package relayenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// withTempHome points HOME at a temp dir so Dir() (under paths.SharedDir())
// resolves inside it. Skips inside the Mac Lima VM, where SharedDir follows
// the virtiofs mount instead of HOME.
func withTempHome(t *testing.T) {
	t.Helper()
	if _, isMac := paths.MacHostHome(); isMac {
		t.Skip("SharedDir follows the Mac virtiofs mount here, not HOME")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if !strings.HasPrefix(Dir(), home) {
		t.Fatalf("Dir() = %s, not under temp HOME %s", Dir(), home)
	}
}

func TestRoundTrip(t *testing.T) {
	withTempHome(t)
	name, err := Write([]string{"GH_TOKEN=ghp_abc=def"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !nameRE.MatchString(name) || strings.ContainsRune(name, '/') {
		t.Fatalf("name %q is not a bare r-… basename", name)
	}
	info, err := os.Stat(filepath.Join(Dir(), name))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	dinfo, err := os.Stat(Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got := dinfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}

	got, err := Consume(name)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got["GH_TOKEN"] != "ghp_abc=def" || len(got) != 1 {
		t.Fatalf("Consume = %v, want GH_TOKEN=ghp_abc=def", got)
	}
	if _, err := os.Lstat(filepath.Join(Dir(), name)); !os.IsNotExist(err) {
		t.Fatalf("file still present after Consume (err=%v)", err)
	}
	if _, err := Consume(name); err == nil {
		t.Fatal("second Consume succeeded; the file must be single-use")
	}
}

func TestWriteRejectsBadPairs(t *testing.T) {
	withTempHome(t)
	for _, p := range []string{"GH_TOKEN=a\nPATH=/evil", "GH_TOKEN=a\rb", "GH_TOKEN=a\x00b", "noequals", "=v"} {
		if _, err := Write([]string{p}); err == nil {
			t.Fatalf("Write(%q) accepted", p)
		} else if strings.Contains(err.Error(), "/evil") || strings.Contains(err.Error(), "a\rb") {
			t.Fatalf("error leaks the value: %v", err)
		}
	}
	entries, _ := os.ReadDir(Dir())
	if len(entries) != 0 {
		t.Fatalf("rejected Write left files behind: %v", entries)
	}
}

func TestConsumeRejectsBadNames(t *testing.T) {
	withTempHome(t)
	for _, n := range []string{"", "../x", "/etc/passwd", "r-", "r-abc/../x", "x-abc", "r-abc.def"} {
		if _, err := Consume(n); err == nil {
			t.Fatalf("Consume(%q) accepted", n)
		}
	}
}

func TestConsumeRejectsSymlink(t *testing.T) {
	withTempHome(t)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("GH_TOKEN=ghp_target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(Dir(), "r-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got, err := Consume("r-link"); err == nil {
		t.Fatalf("Consume followed a symlink: %v", got)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target touched: %v", err)
	}
}

func TestConsumeDropsNonAllowlistedKeys(t *testing.T) {
	withTempHome(t)
	name, err := Write([]string{"GH_TOKEN=ghp_abc", "PATH=/evil", "LD_PRELOAD=/evil.so", "GITHUB_TOKEN=ghp_abc"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Consume(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["GH_TOKEN"] != "ghp_abc" {
		t.Fatalf("Consume = %v, want only GH_TOKEN", got)
	}
}

func TestWriteSweepsStaleFiles(t *testing.T) {
	withTempHome(t)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(Dir(), "r-stale")
	fresh := filepath.Join(Dir(), "r-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("GH_TOKEN=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	name, err := Write([]string{"GH_TOKEN=y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale relay file not swept (err=%v)", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Fatalf("fresh relay file swept: %v", err)
	}
	Remove(name)
	if _, err := os.Lstat(filepath.Join(Dir(), name)); !os.IsNotExist(err) {
		t.Fatalf("Remove left the file (err=%v)", err)
	}
	Remove(name)     // not-exist is fine
	Remove("../etc") // invalid names are ignored
}
