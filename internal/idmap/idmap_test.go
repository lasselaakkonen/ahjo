package idmap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRawIdmapValue locks the exact wire format of the raw.idmap value. The
// two-line `uid <h> 1000` / `gid <h> 1000` form is load-bearing: the package
// doc explains why the shorter `both <id> 1000` form is wrong on Lima setups
// where the in-VM uid (e.g. 501) differs from the gid (1000). A regression
// that collapsed to `both`, swapped the operands, or emitted a host id of 0
// would corrupt the userns mapping (and, with uid 0, host file ownership), so
// we assert byte-for-byte output rather than just "contains".
func TestRawIdmapValue(t *testing.T) {
	tests := []struct {
		name     string
		uid, gid int
		want     string
	}{
		{"equal ids (default 1000:1000)", 1000, 1000, "uid 1000 1000\ngid 1000 1000"},
		{"lima asymmetry (uid 501, gid 1000)", 501, 1000, "uid 501 1000\ngid 1000 1000"},
		{"distinct ids", 502, 20, "uid 502 1000\ngid 20 1000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RawIdmapValue(tt.uid, tt.gid)
			if got != tt.want {
				t.Fatalf("RawIdmapValue(%d, %d) = %q, want %q", tt.uid, tt.gid, got, tt.want)
			}
		})
	}
}

// TestRawIdmapValueShape guards the structural invariants independently of the
// specific ids: exactly two lines, the literal `uid `/`gid ` prefixes, the
// container-side target of 1000 on both lines, and never the dangerous `both`
// form. These catch a refactor that keeps a plausible-looking string but
// breaks the contract.
func TestRawIdmapValueShape(t *testing.T) {
	got := RawIdmapValue(501, 1000)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "uid ") {
		t.Errorf("line 0 = %q, want a `uid ` mapping", lines[0])
	}
	if !strings.HasPrefix(lines[1], "gid ") {
		t.Errorf("line 1 = %q, want a `gid ` mapping", lines[1])
	}
	if strings.Contains(got, "both ") {
		t.Errorf("raw.idmap must not use the `both` form: %q", got)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, " 1000") {
			t.Errorf("line %q must map onto the in-container ubuntu user (1000)", l)
		}
	}
}

// TestFileContainsLine covers fileContainsLine, the matcher HasSubuidGrants
// relies on to decide whether /etc/subuid + /etc/subgid already carry a grant.
// The exact-line semantics matter: a substring match would let `root:1000:10`
// satisfy a probe for `root:1000:1`, falsely reporting a grant present and
// skipping the EnsureSubuidGrants append.
func TestFileContainsLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subuid")
	content := "root:100000:65536\n" +
		"root:1000:1\n" +
		"ubuntu:165536:65536\n" +
		"trailing:5:1   \t\n" // trailing whitespace, should still match after trim
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		line string
		want bool
	}{
		{"exact match mid-file", "root:1000:1", true},
		{"exact match first line", "root:100000:65536", true},
		{"match after trailing-whitespace trim", "trailing:5:1", true},
		{"no match — different id", "root:2000:1", false},
		{"no match — substring is not a full line", "root:1000", false},
		{"no match — superstring of a real line", "root:1000:10", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fileContainsLine(path, tt.line)
			if err != nil {
				t.Fatalf("fileContainsLine: %v", err)
			}
			if got != tt.want {
				t.Errorf("fileContainsLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestFileContainsLineMissingFile asserts a missing file is treated as
// "not present" rather than an error, so callers handle first-boot (no
// /etc/subuid yet) uniformly with "grant absent".
func TestFileContainsLineMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got, err := fileContainsLine(missing, "root:1000:1")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if got {
		t.Errorf("expected false for missing file, got true")
	}
}
