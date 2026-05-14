package cli

import (
	"errors"
	"testing"
)

func TestSplitRepoAlias(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{"github/ahjo", "github", "ahjo", true},
		{"acme/api-server", "acme", "api-server", true},
		{"github/ahjo@main", "", "", false},
		{"github", "", "", false},
		{"github/", "", "", false},
		{"/ahjo", "", "", false},
		{"github/ahjo/extra", "", "", false},
		{"", "", "", false},
		{"foo@bar", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			owner, repo, ok := splitRepoAlias(c.in)
			if ok != c.wantOK || owner != c.wantOwner || repo != c.wantRepo {
				t.Fatalf("splitRepoAlias(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.in, owner, repo, ok, c.wantOwner, c.wantRepo, c.wantOK)
			}
		})
	}
}

func TestSplitBranchAlias(t *testing.T) {
	cases := []struct {
		in         string
		wantRepo   string
		wantBranch string
		wantOK     bool
	}{
		{"github/ahjo@main", "github/ahjo", "main", true},
		{"acme/api@feature/x", "acme/api", "feature/x", true},
		{"github/ahjo", "", "", false},
		{"github/ahjo@", "", "", false},
		{"@main", "", "", false},
		{"github/ahjo@a@b", "", "", false},
		{"github@main", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			repo, branch, ok := splitBranchAlias(c.in)
			if ok != c.wantOK || repo != c.wantRepo || branch != c.wantBranch {
				t.Fatalf("splitBranchAlias(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.in, repo, branch, ok, c.wantRepo, c.wantBranch, c.wantOK)
			}
		})
	}
}

func TestInstallRepoToken_SetsBothEnvKeys(t *testing.T) {
	got := map[string]string{}
	setter := func(k, v string) error {
		got[k] = v
		return nil
	}
	if err := installRepoToken(setter, "ghp_abc"); err != nil {
		t.Fatalf("installRepoToken: %v", err)
	}
	want := map[string]string{
		"environment.GH_TOKEN":     "ghp_abc",
		"environment.GITHUB_TOKEN": "ghp_abc",
	}
	if len(got) != len(want) {
		t.Fatalf("setter calls = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("setter[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestInstallRepoToken_PropagatesSetterError(t *testing.T) {
	// First setter call fails — installRepoToken returns immediately,
	// so the second key is never written. Mirrors how a config-set
	// against a stopped/missing container would surface.
	calls := 0
	wantErr := errors.New("incus config set: exit 1")
	setter := func(_, _ string) error {
		calls++
		return wantErr
	}
	if err := installRepoToken(setter, "ghp_abc"); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("setter called %d times, want 1 (early return on error)", calls)
	}
}

func TestPickGitHubURL_FormatsBothSchemes(t *testing.T) {
	// We can't deterministically assert which scheme is picked (depends on
	// the host's SSH access to github.com), but we can confirm the chosen
	// URL is one of the two well-formed shapes for the given owner/repo.
	got := pickGitHubURL("acme", "widget")
	wantSSH := "git@github.com:acme/widget.git"
	wantHTTPS := "https://github.com/acme/widget.git"
	if got != wantSSH && got != wantHTTPS {
		t.Fatalf("pickGitHubURL = %q, want one of %q or %q", got, wantSSH, wantHTTPS)
	}
}
