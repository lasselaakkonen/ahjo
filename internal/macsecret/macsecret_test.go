//go:build darwin

package macsecret

import (
	"strings"
	"testing"
)

// addCommand is the only part of Set that can be tested without touching
// the real Keychain: it renders the `security -i` stdin line.
func TestAddCommand(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		want    string
		wantErr bool
	}{
		{"plain PAT", "github_pat_11ABCdef_0123", `add-generic-password -U -s "ahjo.GH_TOKEN" -a "o-r" -w "github_pat_11ABCdef_0123"` + "\n", false},
		{"double quote escaped", `a"b`, `add-generic-password -U -s "ahjo.GH_TOKEN" -a "o-r" -w "a\"b"` + "\n", false},
		{"backslash escaped", `a\b`, `add-generic-password -U -s "ahjo.GH_TOKEN" -a "o-r" -w "a\\b"` + "\n", false},
		{"space kept inside quotes", "a b", `add-generic-password -U -s "ahjo.GH_TOKEN" -a "o-r" -w "a b"` + "\n", false},
		{"newline rejected", "ghp_a\nb", "", true},
		{"carriage return rejected", "ghp_a\rb", "", true},
		{"NUL rejected", "ghp_a\x00b", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := addCommand(service("GH_TOKEN"), "o-r", c.val)
			if c.wantErr {
				if err == nil {
					t.Fatalf("addCommand(%q) = %q, want error", c.val, got)
				}
				if strings.Contains(err.Error(), c.val) || strings.Contains(err.Error(), "ghp_a") {
					t.Fatalf("error leaks the value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("addCommand(%q): %v", c.val, err)
			}
			if got != c.want {
				t.Fatalf("addCommand(%q) =\n  %q\nwant\n  %q", c.val, got, c.want)
			}
		})
	}
}

func TestAddCommandRejectsNewlineInAccount(t *testing.T) {
	if _, err := addCommand(service("GH_TOKEN"), "o-r\nadd-generic-password", "v"); err == nil {
		t.Fatal("newline in account accepted; it would inject a second security -i command")
	}
}
