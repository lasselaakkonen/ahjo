//go:build darwin

package main

import (
	"reflect"
	"testing"
)

func TestFindCreateAlias(t *testing.T) {
	cases := []struct {
		name string
		rest []string
		want string
	}{
		{"plain", []string{"owner/repo", "branch"}, "owner/repo"},
		{"skips --base value", []string{"--base", "main", "owner/repo", "b"}, "owner/repo"},
		{"skips --as value", []string{"--as", "x", "owner/repo", "b"}, "owner/repo"},
		{"skips bool --no-fetch", []string{"--no-fetch", "owner/repo", "b"}, "owner/repo"},
		{"double dash", []string{"--", "owner/repo"}, "owner/repo"},
		{"empty", []string{}, ""},
		{"only flags", []string{"--no-fetch"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := findCreateAlias(c.rest); got != c.want {
				t.Fatalf("findCreateAlias(%v) = %q, want %q", c.rest, got, c.want)
			}
		})
	}
}

func TestIsBareOwnerRepo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"owner/repo", true},
		{"acme/api-server", true},
		{"owner/repo@branch", false},
		{"repo@feat", false},
		{"myrepo", false},
		{"owner/repo/extra", false},
		{"owner/", false},
		{"/repo", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := isBareOwnerRepo(c.in); got != c.want {
				t.Fatalf("isBareOwnerRepo(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// interceptCreate must defer (args unchanged, nil env, no Keychain/file access)
// for anything that isn't a bare-owner/repo first-add: a branch alias, a
// non-owner/repo alias, or no positional. These all return at the
// isBareOwnerRepo gate before any lookupRepoSlug/Keychain call.
func TestInterceptCreate_DefersNonFirstAdd(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"branch alias", []string{"create", "repo@feat", "x"}},
		{"non-owner/repo alias", []string{"create", "myrepo", "x"}},
		{"help", []string{"create", "--help"}},
		{"no positional", []string{"create"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			newArgs, env, err := interceptCreate(c.args)
			if err != nil {
				t.Fatalf("interceptCreate(%v) error: %v", c.args, err)
			}
			if env != nil {
				t.Fatalf("interceptCreate(%v) env = %v, want nil", c.args, env)
			}
			if !reflect.DeepEqual(newArgs, c.args) {
				t.Fatalf("interceptCreate(%v) args = %v, want unchanged", c.args, newArgs)
			}
		})
	}
}
