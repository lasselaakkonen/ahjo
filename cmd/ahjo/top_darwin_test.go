//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/relayenv"
)

// fakeTopLima replaces topRunLima and the Keychain cache for one test. run
// sees the argv macLoadBranchStatus relays; it returns the stdout to decode.
func fakeTopLima(t *testing.T, get func(slug, key string) (string, bool, error), run func(argv []string) (string, error)) *[]string {
	t.Helper()
	calls := isolateShim(t, nil, get)
	oldRun, oldCache := topRunLima, topRepoTokens
	topRepoTokens = &repoTokenCache{}
	topRunLima = func(argv ...string) ([]byte, string, error) {
		out, err := run(argv)
		return []byte(out), "", err
	}
	t.Cleanup(func() { topRunLima, topRepoTokens = oldRun, oldCache })
	return calls
}

// TestMacLoadBranchStatusRelaysToken: a Keychain hit for br.Repo reaches the
// in-VM branch-status through a relay file, never on the argv, and the file
// is gone once the call returns.
func TestMacLoadBranchStatusRelaysToken(t *testing.T) {
	var relayed string
	calls := fakeTopLima(t,
		func(slug, key string) (string, bool, error) { return "secret-pat", true, nil },
		func(argv []string) (string, error) {
			if strings.Contains(strings.Join(argv, " "), "secret-pat") {
				t.Errorf("token on argv: %q", argv)
			}
			if len(argv) != 5 || argv[0] != "env" || argv[2] != "ahjo" || argv[3] != "branch-status" || argv[4] != "o-r-feat" {
				t.Fatalf("argv = %q", argv)
			}
			name, ok := strings.CutPrefix(argv[1], relayenv.EnvVar+"=")
			if !ok {
				t.Fatalf("argv[1] = %q, want %s=<name>", argv[1], relayenv.EnvVar)
			}
			b, err := os.ReadFile(filepath.Join(relayenv.Dir(), name))
			if err != nil {
				t.Fatal(err)
			}
			relayed = filepath.Join(relayenv.Dir(), name)
			if got := strings.TrimSpace(string(b)); got != "GH_TOKEN=secret-pat" {
				t.Errorf("relay file = %q", got)
			}
			return `{"pr_checked":true}`, nil
		})

	br := registry.Branch{Repo: "o-r", Slug: "o-r-feat"}
	for i := 0; i < 3; i++ {
		if _, err := macLoadBranchStatus(br); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"o-r"}; !reflect.DeepEqual(*calls, want) {
		t.Errorf("Keychain reads = %q, want %q (cached after the first)", *calls, want)
	}
	if _, err := os.Stat(relayed); !os.IsNotExist(err) {
		t.Errorf("relay file %s left behind (stat err %v)", relayed, err)
	}
}

// TestMacLoadBranchStatusMissRelaysNothing: no Keychain row → plain relay,
// so public repos keep working unauthenticated.
func TestMacLoadBranchStatusMissRelaysNothing(t *testing.T) {
	fakeTopLima(t,
		func(slug, key string) (string, bool, error) { return "", false, nil },
		func(argv []string) (string, error) {
			if want := []string{"ahjo", "branch-status", "o-r-feat"}; !reflect.DeepEqual(argv, want) {
				t.Errorf("argv = %q, want %q", argv, want)
			}
			return `{}`, nil
		})
	if _, err := macLoadBranchStatus(registry.Branch{Repo: "o-r", Slug: "o-r-feat"}); err != nil {
		t.Fatal(err)
	}
}

// TestMacLoadBranchStatusKeychainError: a failed Keychain read still loads
// the git status and explains the resulting gh failure in the PR row.
func TestMacLoadBranchStatusKeychainError(t *testing.T) {
	fakeTopLima(t,
		func(slug, key string) (string, bool, error) { return "", false, errors.New("locked") },
		func(argv []string) (string, error) {
			return `{"git_checked":true,"pr_err":"gh: not logged in"}`, nil
		})
	bs, err := macLoadBranchStatus(registry.Branch{Repo: "o-r", Slug: "o-r-feat"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(bs.PRErr, "read Keychain for o-r: ") || !strings.HasSuffix(bs.PRErr, "; gh: not logged in") {
		t.Errorf("PRErr = %q", bs.PRErr)
	}
}
