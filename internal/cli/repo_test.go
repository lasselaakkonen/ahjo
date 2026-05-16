package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestIsPathLike(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"node", false},
		{"my-stack", false},
		{"ci_2", false},
		{"./foo.json", true},
		{"../shared/dev.json", true},
		{"/abs/path/cfg.json", true},
		{"~/cfg.json", true},
		{"some/relative.json", true},
		{"bare.json", true}, // .json suffix flags it as a path even without separator
		{"", false},         // empty isn't path-like; resolver short-circuits before isPathLike anyway
		{"node:v1", false},  // colon — not a separator on POSIX; treated as identifier (rejected by identifier regex later)
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			if got := isPathLike(c.input); got != c.want {
				t.Fatalf("isPathLike(%q) = %v, want %v", c.input, got, c.want)
			}
		})
	}
}

func TestResolveContainerConfig_NameInputs(t *testing.T) {
	// The container-touching branches (repo-local lookup) need a real
	// container, so we exercise the parts that don't: empty input, the
	// "bare" reserved name, and the path-error / invalid-identifier paths.
	// Real container-backed integration is covered manually per the plan's
	// verification section.
	t.Run("empty returns (nil, false, nil)", func(t *testing.T) {
		cfg, ok, err := resolveContainerConfig("unused", "")
		if cfg != nil || ok || err != nil {
			t.Fatalf("got (%v, %v, %v), want (nil, false, nil)", cfg, ok, err)
		}
	})
	t.Run("bare returns (nil, true, nil)", func(t *testing.T) {
		cfg, ok, err := resolveContainerConfig("unused", "bare")
		if cfg != nil || !ok || err != nil {
			t.Fatalf("got (%v, %v, %v), want (nil, true, nil)", cfg, ok, err)
		}
	})
	t.Run("nonexistent path errors with the path in the message", func(t *testing.T) {
		_, _, err := resolveContainerConfig("unused", "./does-not-exist.json")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "does-not-exist.json") {
			t.Fatalf("error %q lacks the path", err)
		}
	})
	t.Run("invalid identifier errors", func(t *testing.T) {
		_, _, err := resolveContainerConfig("unused", "node!!")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestResolveContainerConfig_HostPath(t *testing.T) {
	// Write a tiny valid ahjocontainer.json to a temp dir and resolve
	// against the absolute path — exercises the host-path branch end to
	// end without needing a container.
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	body := []byte(`{"features": {"ghcr.io/devcontainers/features/go:1": {}}}`)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, ok, err := resolveContainerConfig("unused", p)
	if err != nil {
		t.Fatalf("resolveContainerConfig: %v", err)
	}
	if !ok || cfg == nil {
		t.Fatalf("got (%v, %v), want (non-nil, true)", cfg, ok)
	}
	if _, hasGo := cfg.Features["ghcr.io/devcontainers/features/go:1"]; !hasGo {
		t.Fatalf("parsed cfg missing the go Feature: %#v", cfg.Features)
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
