package cli

import (
	"bytes"
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
