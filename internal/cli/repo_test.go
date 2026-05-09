package cli

import "testing"

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
