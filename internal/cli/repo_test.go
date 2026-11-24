package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

func TestShouldForwardAgent(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	https := &registry.Repo{Remote: "https://github.com/o/r.git"}
	ssh := &registry.Repo{Remote: "git@github.com:o/r.git"}
	local := &registry.Repo{Remote: "/srv/git/r"}
	mixedCase := &registry.Repo{Remote: "https://GitHub.com/o/r"}
	cases := []struct {
		name     string
		repo     *registry.Repo
		hasToken bool
		forward  *bool
		want     bool
	}{
		{"https+token+auto suppresses", https, true, nil, false},
		{"https+no-token forwards", https, false, nil, true},
		{"ssh+token forwards", ssh, true, nil, true},
		{"ssh+no-token forwards", ssh, false, nil, true},
		{"local remote+token forwards", local, true, nil, true},
		{"nil repo forwards", nil, true, nil, true},
		{"explicit true forwards even when covered", https, true, boolPtr(true), true},
		{"explicit false never forwards even on ssh", ssh, false, boolPtr(false), false},
		{"mixed-case https host+token suppresses", mixedCase, true, nil, false},
		{"mixed-case https host+no-token forwards", mixedCase, false, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldForwardAgent(c.repo, c.hasToken, &config.Config{ForwardSSHAgent: c.forward})
			if got != c.want {
				t.Fatalf("shouldForwardAgent(%+v, token=%v, forward=%v) = %v, want %v",
					c.repo, c.hasToken, c.forward, got, c.want)
			}
		})
	}
}

// forwardAgentForSSHConfig must decide from the registry flag alone: a
// GH_TOKEN in the environment belongs to whichever repo the Mac shim resolved,
// not to every repo rendered into the shared ssh-config.
func TestForwardAgentForSSHConfig(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_belongs_to_some_other_repo")
	boolPtr := func(b bool) *bool { return &b }
	covered := &registry.Repo{Remote: "https://github.com/o/r.git", HasToken: true}
	uncovered := &registry.Repo{Remote: "https://github.com/o/r.git"}
	ssh := &registry.Repo{Remote: "git@github.com:o/r.git", HasToken: true}
	cases := []struct {
		name    string
		repo    *registry.Repo
		forward *bool
		want    bool
	}{
		{"https+has_token suppresses", covered, nil, false},
		{"https without has_token forwards despite env GH_TOKEN", uncovered, nil, true},
		{"ssh+has_token forwards", ssh, nil, true},
		{"forward_ssh_agent=false wins on ssh", ssh, boolPtr(false), false},
		{"forward_ssh_agent=true wins on covered https", covered, boolPtr(true), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := forwardAgentForSSHConfig(&config.Config{ForwardSSHAgent: c.forward})
			if got := f(c.repo); got != c.want {
				t.Fatalf("forwardAgentForSSHConfig(forward=%v)(%+v) = %v, want %v", c.forward, c.repo, got, c.want)
			}
		})
	}
	if !forwardAgentForSSHConfig(nil)(uncovered) {
		t.Fatalf("nil cfg must mean the auto policy (forward an uncovered repo)")
	}
}

// stubConfigGet replaces the Incus config probe repoHasToken falls back to.
func stubConfigGet(t *testing.T, f func(container, key string) (string, error)) {
	t.Helper()
	orig := configGet
	configGet = f
	t.Cleanup(func() { configGet = orig })
}

func TestRepoHasToken(t *testing.T) {
	// Isolate every token source repoToken consults: the env var, and (off
	// macOS-in-VM) the per-repo env file under $HOME/.ahjo-shared.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	var probed []string
	stubConfigGet(t, func(container, key string) (string, error) {
		probed = append(probed, container+" "+key)
		switch container {
		case "ahjo-pat":
			return "ghp_x", nil
		case "ahjo-broken":
			return "", errors.New("incus unreachable")
		}
		return "", nil
	})

	if repoHasToken(nil) {
		t.Fatalf("nil repo: want false")
	}
	if !repoHasToken(&registry.Repo{Name: "flagged", HasToken: true}) {
		t.Fatalf("HasToken=true: want true")
	}
	if repoHasToken(&registry.Repo{Name: "plain"}) {
		t.Fatalf("no flag, no env, no base: want false")
	}
	if len(probed) != 0 {
		t.Fatalf("repo without BaseContainerName must not probe Incus, got %q", probed)
	}
	if !repoHasToken(&registry.Repo{Name: "legacy", BaseContainerName: "ahjo-pat"}) {
		t.Fatalf("base container with environment.GH_TOKEN: want true")
	}
	if probed[0] != "ahjo-pat environment.GH_TOKEN" {
		t.Fatalf("probed %q, want the base container's environment.GH_TOKEN", probed[0])
	}
	if repoHasToken(&registry.Repo{Name: "nopat", BaseContainerName: "ahjo-nopat"}) {
		t.Fatalf("base container without GH_TOKEN: want false")
	}
	if repoHasToken(&registry.Repo{Name: "broken", BaseContainerName: "ahjo-broken"}) {
		t.Fatalf("probe error must count as no token")
	}

	t.Setenv("GH_TOKEN", "x")
	if !repoHasToken(&registry.Repo{Name: "plain"}) {
		t.Fatalf("flag false with GH_TOKEN in env: want true")
	}
}

func TestIsHTTPSRemote(t *testing.T) {
	cases := []struct {
		remote string
		want   bool
	}{
		{"https://github.com/o/r.git", true},
		{"https://GitHub.com/o/r", true},
		{"HTTPS://github.com/o/r", true},
		{"https://github.com:443/o/r", true},
		{"https://user@github.com/o/r", true},
		{"git@github.com:o/r.git", false},
		{"ssh://git@github.com/o/r", false},
		{"http://github.com/o/r", false},
		{"https://github.com.evil.example/o/r", false},
		{"https://github.com:8443/o/r", false},
		{"https://github.com", false},
		{"https://github.com/", false},
		{"https://gitlab.com/o/r", false},
		{"/srv/git/r", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.remote, func(t *testing.T) {
			if got := isHTTPSRemote(c.remote); got != c.want {
				t.Fatalf("isHTTPSRemote(%q) = %v, want %v", c.remote, got, c.want)
			}
		})
	}
}

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

func TestWithRepoToken(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		tok  string
		want map[string]string
	}{
		{"empty token leaves env unchanged", map[string]string{"A": "1"}, "", map[string]string{"A": "1"}},
		{"empty token nil env stays nil", nil, "", nil},
		{"nil env allocates", nil, "ghp_abc", map[string]string{"GH_TOKEN": "ghp_abc", "GITHUB_TOKEN": "ghp_abc"}},
		{"sets both alongside others", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "ghp_abc",
			map[string]string{"GIT_TERMINAL_PROMPT": "0", "GH_TOKEN": "ghp_abc", "GITHUB_TOKEN": "ghp_abc"}},
		{"existing GH_TOKEN kept, GITHUB_TOKEN follows it", map[string]string{"GH_TOKEN": "ghp_mine"}, "ghp_abc",
			map[string]string{"GH_TOKEN": "ghp_mine", "GITHUB_TOKEN": "ghp_mine"}},
		{"existing GITHUB_TOKEN kept", map[string]string{"GITHUB_TOKEN": "ghp_other"}, "ghp_abc",
			map[string]string{"GH_TOKEN": "ghp_abc", "GITHUB_TOKEN": "ghp_other"}},
		{"existing empty GH_TOKEN replaced", map[string]string{"GH_TOKEN": ""}, "ghp_abc",
			map[string]string{"GH_TOKEN": "ghp_abc", "GITHUB_TOKEN": "ghp_abc"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := withRepoToken(c.env, c.tok)
			if c.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("got[%q] = %q, want %q (full: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

func TestRepoTokenEnvArgs(t *testing.T) {
	if got := repoTokenEnvArgs(""); got != nil {
		t.Fatalf("empty token: got %v, want nil", got)
	}
	got := strings.Join(repoTokenEnvArgs("ghp_abc"), " ")
	if want := "--env GH_TOKEN=ghp_abc --env GITHUB_TOKEN=ghp_abc"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// fakeConfig is an in-memory stand-in for one container's Incus config,
// feeding scrubPersistedToken's get/unset accessors.
type fakeConfig struct {
	vals     map[string]string
	getErr   error
	unsetErr error
	unsets   []string
}

func (f *fakeConfig) get(k string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.vals[k], nil
}

func (f *fakeConfig) unset(k string) error {
	f.unsets = append(f.unsets, k)
	if f.unsetErr != nil {
		return f.unsetErr
	}
	delete(f.vals, k)
	return nil
}

func TestScrubPersistedToken_NothingSet(t *testing.T) {
	f := &fakeConfig{vals: map[string]string{"environment.HOME": "/home/ubuntu"}}
	removed, err := scrubPersistedToken(f.get, f.unset)
	if err != nil || removed {
		t.Fatalf("removed=%v err=%v, want false, nil", removed, err)
	}
	if len(f.unsets) != 0 {
		t.Fatalf("unset calls = %v, want none", f.unsets)
	}
}

func TestScrubPersistedToken_OneSet(t *testing.T) {
	f := &fakeConfig{vals: map[string]string{"environment.GITHUB_TOKEN": "ghp_abc"}}
	removed, err := scrubPersistedToken(f.get, f.unset)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v, want true, nil", removed, err)
	}
	if len(f.unsets) != 1 || f.unsets[0] != "environment.GITHUB_TOKEN" {
		t.Fatalf("unset calls = %v, want [environment.GITHUB_TOKEN]", f.unsets)
	}
}

func TestScrubPersistedToken_BothSet(t *testing.T) {
	f := &fakeConfig{vals: map[string]string{
		"environment.GH_TOKEN":     "ghp_abc",
		"environment.GITHUB_TOKEN": "ghp_abc",
	}}
	removed, err := scrubPersistedToken(f.get, f.unset)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v, want true, nil", removed, err)
	}
	if len(f.unsets) != 2 || len(f.vals) != 0 {
		t.Fatalf("unset calls = %v, remaining = %v", f.unsets, f.vals)
	}
}

func TestScrubPersistedToken_GetError(t *testing.T) {
	wantErr := errors.New("incus config get: exit 1")
	f := &fakeConfig{getErr: wantErr}
	removed, err := scrubPersistedToken(f.get, f.unset)
	if !errors.Is(err, wantErr) || removed {
		t.Fatalf("removed=%v err=%v, want false, %v", removed, err, wantErr)
	}
	if len(f.unsets) != 0 {
		t.Fatalf("unset calls = %v, want none after get error", f.unsets)
	}
}

func TestScrubPersistedToken_UnsetError(t *testing.T) {
	wantErr := errors.New("incus config unset: exit 1")
	f := &fakeConfig{vals: map[string]string{"environment.GH_TOKEN": "ghp_abc"}, unsetErr: wantErr}
	removed, err := scrubPersistedToken(f.get, f.unset)
	if !errors.Is(err, wantErr) || removed {
		t.Fatalf("removed=%v err=%v, want false, %v", removed, err, wantErr)
	}
	if strings.Contains(err.Error(), "ghp_abc") {
		t.Fatalf("error leaks token: %v", err)
	}
}

// TestRepoTokenPrecedence pins that a per-repo PAT beats a global GH_TOKEN
// in the process env, and that the env is the fallback. Runs only where
// paths.MacHostHome() is false (always on darwin, and on a Linux host
// without the Lima virtiofs mount).
func TestRepoTokenPrecedence(t *testing.T) {
	if _, isMac := paths.MacHostHome(); isMac {
		t.Skip("inside the Mac Lima VM the per-repo file is never consulted")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "")

	if _, found, err := repoToken("o-r"); err != nil || found {
		t.Fatalf("no sources: found=%v err=%v, want false, nil", found, err)
	}

	t.Setenv("GH_TOKEN", "ghp_global")
	if tok, found, err := repoToken("o-r"); err != nil || !found || tok != "ghp_global" {
		t.Fatalf("env only: tok=%q found=%v err=%v, want ghp_global", tok, found, err)
	}

	if err := os.MkdirAll(paths.RepoEnvDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tokenstore.SetAt(paths.SlugEnvPath("o-r"), tokenstore.GHTokenEnv, "ghp_repo"); err != nil {
		t.Fatal(err)
	}
	if tok, found, err := repoToken("o-r"); err != nil || !found || tok != "ghp_repo" {
		t.Fatalf("per-repo + env: tok=%q found=%v err=%v, want ghp_repo", tok, found, err)
	}
	// Another repo without its own file still falls back to the env.
	if tok, _, _ := repoToken("o-other"); tok != "ghp_global" {
		t.Fatalf("other repo: tok=%q, want ghp_global", tok)
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

func TestParseRepoSource(t *testing.T) {
	cases := []struct {
		in          string
		wantExplict string // "" means inferred GitHub alias
		wantOwner   string
		wantName    string
	}{
		{"acme/widget", "", "acme", "widget"},
		{"git@github.com:acme/widget.git", "git@github.com:acme/widget.git", "", ""},
		{"https://github.com/acme/widget.git", "https://github.com/acme/widget.git", "", ""},
		{"ssh://git@github.com/acme/widget.git", "ssh://git@github.com/acme/widget.git", "", ""},
		{"/path/to/local/repo", "/path/to/local/repo", "", ""},
	}
	for _, c := range cases {
		got := parseRepoSource(c.in)
		if got.explicitURL != c.wantExplict || got.owner != c.wantOwner || got.name != c.wantName {
			t.Errorf("parseRepoSource(%q) = %+v, want explicit=%q owner=%q name=%q",
				c.in, got, c.wantExplict, c.wantOwner, c.wantName)
		}
	}
}

func TestRepoSourceCloneURL(t *testing.T) {
	// An explicit URL is verbatim regardless of token state — ahjo never
	// rewrites SSH↔HTTPS for a URL the user typed.
	ssh := repoSource{explicitURL: "git@github.com:acme/widget.git"}
	if got := ssh.cloneURL(true); got != "git@github.com:acme/widget.git" {
		t.Errorf("explicit SSH with token: cloneURL = %q, want verbatim", got)
	}
	if got := ssh.cloneURL(false); got != "git@github.com:acme/widget.git" {
		t.Errorf("explicit SSH without token: cloneURL = %q, want verbatim", got)
	}

	// An inferred alias with a PAT in hand clones over HTTPS so the token
	// authenticates the clone and every later fetch/push — this is the
	// regression target for `ahjo create owner/repo branch`.
	alias := repoSource{owner: "acme", name: "widget"}
	if got := alias.cloneURL(true); got != "https://github.com/acme/widget.git" {
		t.Errorf("inferred alias with token: cloneURL = %q, want HTTPS", got)
	}

	// canonicalURL (used only for alias/slug allocation) is protocol-
	// independent and never depends on token state.
	if got := alias.canonicalURL(); got != "https://github.com/acme/widget.git" {
		t.Errorf("inferred alias canonicalURL = %q, want HTTPS", got)
	}

	// Without a token an inferred alias falls back to the SSH-then-HTTPS
	// probe — assert only that it yields a well-formed shape, since the
	// scheme depends on the host's SSH reachability to github.com.
	got := alias.cloneURL(false)
	wantSSH := "git@github.com:acme/widget.git"
	wantHTTPS := "https://github.com/acme/widget.git"
	if got != wantSSH && got != wantHTTPS {
		t.Errorf("inferred alias without token: cloneURL = %q, want one of %q or %q", got, wantSSH, wantHTTPS)
	}
}

// TestRunWarmInstallWith_SkipsWhenBinaryAbsent covers the polyglot-repo
// case: detection produced a match but the installer binary isn't on
// PATH inside the container. The precheck must short-circuit, log a
// `not found ... skipping` line, and NOT invoke runCmd. This is the
// regression target — the previous implementation routed `command -v`
// through execve, which always missed the shell builtin and silently
// skipped every warm-install.
func TestRunWarmInstallWith_SkipsWhenBinaryAbsent(t *testing.T) {
	match := detectMatch{
		entry: detectEntry{name: "node", cmd: []string{"yarn", "install", "--frozen-lockfile"}},
		hit:   "yarn.lock",
	}
	var probed []string
	var ran [][]string
	var out bytes.Buffer

	err := runWarmInstallWith(
		[]detectMatch{match},
		func(bin string) bool { probed = append(probed, bin); return false },
		func(argv []string) error { ran = append(ran, argv); return nil },
		&out,
	)
	if err != nil {
		t.Fatalf("runWarmInstallWith returned err: %v", err)
	}
	if len(probed) != 1 || probed[0] != "yarn" {
		t.Fatalf("probeBin called with %v, want [yarn]", probed)
	}
	if len(ran) != 0 {
		t.Fatalf("runCmd should not be invoked when probe misses; got %v", ran)
	}
	want := "→ yarn not found in container; skipping yarn install --frozen-lockfile\n"
	if out.String() != want {
		t.Fatalf("out=%q, want %q", out.String(), want)
	}
}

// TestRunWarmInstallWith_InvokesCmdWhenBinaryPresent covers the happy
// path: probe hit, installer invoked with the exact argv the detect
// table specifies, and the user-visible line names both the command
// and the detected lockfile.
func TestRunWarmInstallWith_InvokesCmdWhenBinaryPresent(t *testing.T) {
	match := detectMatch{
		entry: detectEntry{name: "go", cmd: []string{"go", "mod", "download"}},
		hit:   "go.sum",
	}
	var ran [][]string
	var out bytes.Buffer

	err := runWarmInstallWith(
		[]detectMatch{match},
		func(string) bool { return true },
		func(argv []string) error { ran = append(ran, argv); return nil },
		&out,
	)
	if err != nil {
		t.Fatalf("runWarmInstallWith returned err: %v", err)
	}
	if len(ran) != 1 || strings.Join(ran[0], " ") != "go mod download" {
		t.Fatalf("runCmd argv=%v, want [go mod download]", ran)
	}
	want := "→ go mod download (go.sum detected)\n"
	if out.String() != want {
		t.Fatalf("out=%q, want %q", out.String(), want)
	}
}

// TestRunWarmInstallWith_NoMatchesPrintsHint confirms the "nothing to
// warm" branch still surfaces the explanatory line. Empty input is the
// common case for a repo with no lockfiles.
func TestRunWarmInstallWith_NoMatchesPrintsHint(t *testing.T) {
	var out bytes.Buffer
	err := runWarmInstallWith(nil,
		func(string) bool { t.Fatal("probeBin should not be called"); return false },
		func([]string) error { t.Fatal("runCmd should not be called"); return nil },
		&out,
	)
	if err != nil {
		t.Fatalf("runWarmInstallWith returned err: %v", err)
	}
	if !strings.Contains(out.String(), "no lockfile detected") {
		t.Fatalf("out=%q missing no-lockfile hint", out.String())
	}
}

// TestRunWarmInstallWith_PropagatesRunCmdError ensures a failed
// installer (e.g. lockfile drift) bubbles up wrapped with the argv,
// matching today's `<cmd>: <inner err>` error surface that callers
// already log against.
func TestRunWarmInstallWith_PropagatesRunCmdError(t *testing.T) {
	match := detectMatch{
		entry: detectEntry{name: "php", cmd: []string{"composer", "install"}},
		hit:   "composer.lock",
	}
	boom := errors.New("boom")
	err := runWarmInstallWith(
		[]detectMatch{match},
		func(string) bool { return true },
		func([]string) error { return boom },
		&bytes.Buffer{},
	)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped boom error, got %v", err)
	}
	if !strings.Contains(err.Error(), "composer install") {
		t.Fatalf("err=%q missing argv prefix", err.Error())
	}
}
