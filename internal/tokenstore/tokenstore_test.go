package tokenstore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// envPath returns a fresh path inside a temp dir (the file itself does not yet
// exist) so each test drives SetAt/GetAt/… against an isolated store.
func envPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".env")
}

func TestSetAt_CreatesWithMode0600(t *testing.T) {
	p := envPath(t)
	if err := SetAt(p, "GH_TOKEN", "abc"); err != nil {
		t.Fatalf("SetAt: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %o, want 0600 (secrets file)", st.Mode().Perm())
	}
	got, ok, err := GetAt(p, "GH_TOKEN")
	if err != nil || !ok || got != "abc" {
		t.Fatalf("GetAt = (%q, %v, %v), want (abc, true, nil)", got, ok, err)
	}
}

func TestSetAt_UpsertPreservesOtherKeys(t *testing.T) {
	p := envPath(t)
	mustSet(t, p, "A", "1")
	mustSet(t, p, "B", "2")
	mustSet(t, p, "A", "9") // update A, leave B

	got, err := ListAt(p)
	if err != nil {
		t.Fatalf("ListAt: %v", err)
	}
	want := map[string]string{"A": "9", "B": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAt = %v, want %v", got, want)
	}
}

// TestSetAt_NoTrailingBlankLineGrowth pins the trailing-newline trim: repeated
// upserts must not accumulate blank lines (the read-split-rejoin loop trims the
// final empty element each time).
func TestSetAt_NoTrailingBlankLineGrowth(t *testing.T) {
	p := envPath(t)
	for range 5 {
		mustSet(t, p, "A", "1")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(b); got != "A=1\n" {
		t.Fatalf("file = %q, want %q", got, "A=1\n")
	}
}

func TestGetAt_AbsentAndMissingFile(t *testing.T) {
	p := envPath(t)
	// Missing file: not an error, just not-found.
	if _, ok, err := GetAt(p, "X"); err != nil || ok {
		t.Fatalf("GetAt(missing file) = (_, %v, %v), want (false, nil)", ok, err)
	}
	mustSet(t, p, "A", "1")
	if _, ok, err := GetAt(p, "X"); err != nil || ok {
		t.Fatalf("GetAt(absent key) = (_, %v, %v), want (false, nil)", ok, err)
	}
}

func TestListAt_SkipsCommentsBlanksAndStripsQuotes(t *testing.T) {
	p := envPath(t)
	content := "# a comment\n\nA=1\n  # indented comment\nB=\"quoted value\"\nC =  spaced  \nnotakeyvalue\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ListAt(p)
	if err != nil {
		t.Fatalf("ListAt: %v", err)
	}
	want := map[string]string{
		"A": "1",
		"B": "quoted value", // surrounding quotes stripped
		"C": "spaced",       // key + value trimmed
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAt = %v, want %v", got, want)
	}
}

func TestListAt_MissingFileEmpty(t *testing.T) {
	got, err := ListAt(envPath(t))
	if err != nil {
		t.Fatalf("ListAt(missing): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListAt(missing) = %v, want empty", got)
	}
}

func TestUnsetAt(t *testing.T) {
	p := envPath(t)
	mustSet(t, p, "A", "1")
	mustSet(t, p, "B", "2")

	if err := UnsetAt(p, "A"); err != nil {
		t.Fatalf("UnsetAt: %v", err)
	}
	got, _ := ListAt(p)
	if !reflect.DeepEqual(got, map[string]string{"B": "2"}) {
		t.Fatalf("after unset A: %v, want {B:2}", got)
	}

	// No-op on a missing key.
	if err := UnsetAt(p, "Z"); err != nil {
		t.Fatalf("UnsetAt(missing key): %v", err)
	}
	// Removing the last key leaves an (empty) file, not an error.
	if err := UnsetAt(p, "B"); err != nil {
		t.Fatalf("UnsetAt(last): %v", err)
	}
	if got, _ := ListAt(p); len(got) != 0 {
		t.Fatalf("after removing all keys: %v, want empty", got)
	}
	// No-op on a missing file.
	if err := UnsetAt(envPath(t), "A"); err != nil {
		t.Fatalf("UnsetAt(missing file): %v", err)
	}
}

func TestLoadInto_MergesAndOverwrites(t *testing.T) {
	p := envPath(t)
	mustSet(t, p, "A", "fromfile")
	mustSet(t, p, "C", "3")

	m := map[string]string{"A": "preexisting", "B": "keepme"}
	if err := LoadInto(p, m); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	want := map[string]string{"A": "fromfile", "B": "keepme", "C": "3"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("LoadInto result = %v, want %v", m, want)
	}
}

func TestParseLine(t *testing.T) {
	tests := []struct {
		in           string
		wantK, wantV string
		wantOK       bool
	}{
		{"A=1", "A", "1", true},
		{"  A = 1  ", "A", "1", true},
		{`A="quoted"`, "A", "quoted", true},
		{"# comment", "", "", false},
		{"   ", "", "", false},
		{"", "", "", false},
		{"noequals", "", "", false},
		{"=leadingeq", "", "", false}, // eq==0 → rejected
		{"A=", "A", "", true},         // empty value is valid
		{`A="only-one-quote`, "A", `"only-one-quote`, true},
	}
	for _, tt := range tests {
		k, v, ok := parseLine(tt.in)
		if k != tt.wantK || v != tt.wantV || ok != tt.wantOK {
			t.Errorf("parseLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.in, k, v, ok, tt.wantK, tt.wantV, tt.wantOK)
		}
	}
}

func mustSet(t *testing.T, path, k, v string) {
	t.Helper()
	if err := SetAt(path, k, v); err != nil {
		t.Fatalf("SetAt(%q=%q): %v", k, v, err)
	}
}
