//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// isolateShim points HOME at a temp dir (so lookupRepoSlug reads a fake
// repo-aliases file, never the user's), writes aliases (alias→slug), and
// replaces the Keychain read with a fake. Returns a pointer to the list of
// (slug) lookups the fake saw.
func isolateShim(t *testing.T, aliases map[string]string, get func(slug, key string) (string, bool, error)) *[]string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(paths.SharedDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	var body string
	for a, slug := range aliases {
		body += a + "\t" + slug + "\n"
	}
	if err := os.WriteFile(filepath.Join(paths.SharedDir(), paths.RepoAliasesFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	old := keychainGet
	keychainGet = func(slug, key string) (string, bool, error) {
		calls = append(calls, slug)
		return get(slug, key)
	}
	t.Cleanup(func() { keychainGet = old })
	return &calls
}

func noKeychain(t *testing.T) func(string, string) (string, bool, error) {
	return func(slug, key string) (string, bool, error) {
		t.Errorf("unexpected Keychain read for %q", slug)
		return "", false, nil
	}
}

func TestFindRepoAddURL_ContainerConfig(t *testing.T) {
	cases := []struct {
		name string
		rest []string
		want string
	}{
		{"separate value", []string{"--container-config", "node", "https://github.com/o/r"}, "https://github.com/o/r"},
		{"equals form", []string{"--container-config=node", "https://github.com/o/r"}, "https://github.com/o/r"},
		{"after url", []string{"https://github.com/o/r", "--container-config", "node"}, "https://github.com/o/r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := findRepoAddURL(c.rest); got != c.want {
				t.Fatalf("findRepoAddURL(%v) = %q, want %q", c.rest, got, c.want)
			}
		})
	}
}

func TestFirstNonFlag_ContainerConfig(t *testing.T) {
	cases := []struct {
		name string
		rest []string
		want string
	}{
		{"separate value", []string{"--container-config", "node", "owner/repo"}, "owner/repo"},
		{"equals form", []string{"--container-config=node", "owner/repo"}, "owner/repo"},
		{"with --as", []string{"--as", "x", "--container-config", "node", "owner/repo"}, "owner/repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstNonFlag(c.rest); got != c.want {
				t.Fatalf("firstNonFlag(%v) = %q, want %q", c.rest, got, c.want)
			}
		})
	}
}

// A registered alias (repo or branch) gets its Keychain PAT injected into
// `create`, with no prompt.
func TestInterceptCreate_RegisteredAliasInjectsToken(t *testing.T) {
	for _, alias := range []string{"owner/repo", "owner/repo@feat", "myrepo"} {
		t.Run(alias, func(t *testing.T) {
			calls := isolateShim(t, map[string]string{alias: "owner-repo"},
				func(slug, key string) (string, bool, error) {
					if key != ghTokenKey {
						t.Errorf("key = %q, want %q", key, ghTokenKey)
					}
					return "tok123", true, nil
				})
			args := []string{"create", alias, "x"}
			newArgs, env, err := interceptCreate(args)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(newArgs, args) {
				t.Fatalf("args = %v, want unchanged", newArgs)
			}
			if want := []string{"GH_TOKEN=tok123"}; !reflect.DeepEqual(env, want) {
				t.Fatalf("env = %v, want %v", env, want)
			}
			if !reflect.DeepEqual(*calls, []string{"owner-repo"}) {
				t.Fatalf("Keychain lookups = %v, want [owner-repo]", *calls)
			}
		})
	}
}

func TestInterceptCreate_RegisteredAliasSkipsContainerConfigValue(t *testing.T) {
	isolateShim(t, map[string]string{"owner/repo": "owner-repo"},
		func(slug, key string) (string, bool, error) { return "tok", true, nil })
	_, env, err := interceptCreate([]string{"create", "--as", "x", "owner/repo", "b"})
	if err != nil || len(env) != 1 {
		t.Fatalf("env = %v, err = %v; want injected token", env, err)
	}
}

func TestInterceptCreate_RegisteredAliasMissIsSilent(t *testing.T) {
	isolateShim(t, map[string]string{"owner/repo": "owner-repo"},
		func(slug, key string) (string, bool, error) { return "", false, nil })
	args := []string{"create", "owner/repo", "x"}
	newArgs, env, err := interceptCreate(args)
	if err != nil || env != nil || !reflect.DeepEqual(newArgs, args) {
		t.Fatalf("got (%v, %v, %v), want unchanged args, nil env, nil err", newArgs, env, err)
	}
}

func TestInterceptCreate_RegisteredAliasKeychainErrorIsFatal(t *testing.T) {
	boom := errors.New("keychain locked")
	isolateShim(t, map[string]string{"owner/repo": "owner-repo"},
		func(slug, key string) (string, bool, error) { return "", false, boom })
	_, _, err := interceptCreate([]string{"create", "owner/repo", "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
}

func TestKeychainEnvForSlug(t *testing.T) {
	isolateShim(t, nil, func(slug, key string) (string, bool, error) {
		switch slug {
		case "hit":
			return "t", true, nil
		case "empty":
			return "", true, nil
		}
		return "", false, nil
	})
	if env, err := keychainEnvForSlug("hit"); err != nil || !reflect.DeepEqual(env, []string{"GH_TOKEN=t"}) {
		t.Fatalf("hit: env=%v err=%v", env, err)
	}
	for _, slug := range []string{"empty", "miss"} {
		if env, err := keychainEnvForSlug(slug); err != nil || env != nil {
			t.Fatalf("%s: env=%v err=%v, want nil, nil", slug, env, err)
		}
	}
}

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

// TestFindRepoAddURL pins that the bool consent flags (--allow-nesting,
// --allow-nested-incus) are skipped like any other bool flag, so they
// pass through the shim's relay untouched and never get mistaken for the
// URL.
func TestFindRepoAddURL(t *testing.T) {
	const u = "https://github.com/x/y.git"
	cases := []struct {
		name string
		rest []string
		want string
	}{
		{"plain", []string{u}, u},
		{"--allow-nesting before url", []string{"--allow-nesting", u}, u},
		{"--allow-nested-incus before url", []string{"--allow-nested-incus", u}, u},
		{"both flags before url", []string{"--allow-nesting", "--allow-nested-incus", u}, u},
		{"flags after url", []string{u, "--allow-nesting", "--allow-nested-incus"}, u},
		{"with --yes and --as", []string{"-y", "--allow-nesting", "--as", "alias", u}, u},
		{"only flags", []string{"--allow-nesting"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := findRepoAddURL(c.rest); got != c.want {
				t.Fatalf("findRepoAddURL(%v) = %q, want %q", c.rest, got, c.want)
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
// non-owner/repo alias, or no positional, when the alias is unregistered. The
// alias file is faked (empty) and the Keychain is faked to fail on any read.
func TestInterceptCreate_DefersNonFirstAdd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
			isolateShim(t, nil, noKeychain(t))
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

// withRepoAliases points HOME at a temp dir and writes a repo-aliases file
// (alias<TAB>slug lines) under its .ahjo-shared, the file lookupRepoSlug
// reads.
func withRepoAliases(t *testing.T, lines ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".ahjo-shared")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(paths.RepoAliasesPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(paths.RepoAliasesPath(), dir) {
		t.Fatalf("RepoAliasesPath %s not under temp HOME", paths.RepoAliasesPath())
	}
}

func TestLookupRepoSlug_RepoAndBranchAliases(t *testing.T) {
	withRepoAliases(t, "# comment", "o/r\to-r", "o/r@feat\to-r")
	for _, alias := range []string{"o/r", "o/r@feat"} {
		slug, err := lookupRepoSlug(alias)
		if err != nil || slug != "o-r" {
			t.Fatalf("lookupRepoSlug(%q) = %q, %v; want o-r", alias, slug, err)
		}
	}
	if _, err := lookupRepoSlug("other/repo"); err == nil {
		t.Fatal("lookupRepoSlug(unknown) = nil error, want miss")
	}
}

// Alias extraction for the subcommands step 11 added: the alias each one
// resolves must be the positional after the subcommand, never a flag.
func TestAliasExtractionForTokenRelays(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"repo pull alias", firstNonFlag([]string{"o/r"}), "o/r"},
		{"rm alias", firstNonFlag([]string{"o/r@feat"}), "o/r@feat"},
		{"rm skips bool flags", firstNonFlag([]string{"--force", "--force-default", "o/r@feat"}), "o/r@feat"},
		{"rm help only", firstNonFlag([]string{"--help"}), ""},
		{"create registered branch alias form", findCreateAlias([]string{"--no-fetch", "o/r", "feat"}), "o/r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Fatalf("got %q, want %q", c.got, c.want)
			}
		})
	}
}

// Unknown aliases must pass through untouched with no env and no Keychain
// call, for every subcommand that now relays the PAT. The aliases file
// exists but does not list the alias.
func TestInterceptRepoSubcommand_UnknownAliasPassesThrough(t *testing.T) {
	withRepoAliases(t, "known/repo\tknown-repo")
	cases := [][]string{
		{"repo", "pull", "o/r"},
		{"rm", "o/r@feat"},
		{"rm", "--force", "o/r@feat"},
		{"rm"},
		{"create", "o/r@feat", "x"},
		{"create", "myrepo", "x"},
		{"shell", "o/r@feat"},
		{"claude", "o/r@feat"},
		{"repo", "pull"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			newArgs, env, err := interceptRepoSubcommand(args)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if env != nil {
				t.Fatalf("env = %v, want nil", env)
			}
			if !reflect.DeepEqual(newArgs, args) {
				t.Fatalf("args = %v, want unchanged %v", newArgs, args)
			}
		})
	}
}

func TestKeychainEnvForAlias_MissesReturnNil(t *testing.T) {
	withRepoAliases(t, "known/repo\tknown-repo")
	for _, alias := range []string{"", "unknown/repo", "unknown/repo@b"} {
		env, err := keychainEnvForAlias(alias)
		if err != nil || env != nil {
			t.Fatalf("keychainEnvForAlias(%q) = %v, %v; want nil, nil", alias, env, err)
		}
	}
}
