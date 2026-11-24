package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func TestApplyApproveEnv(t *testing.T) {
	global := []string{"GH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
	cases := []struct {
		name         string
		current      []string
		keys         []string
		revoke       bool
		wantNext     []string
		wantInGlobal []string
	}{
		{
			name:     "add to empty, sorted",
			keys:     []string{"NPM_TOKEN", "AWS_KEY"},
			wantNext: []string{"AWS_KEY", "NPM_TOKEN"},
		},
		{
			name:     "add dedupes against current and itself",
			current:  []string{"NPM_TOKEN"},
			keys:     []string{"NPM_TOKEN", "FOO", "FOO"},
			wantNext: []string{"FOO", "NPM_TOKEN"},
		},
		{
			name:         "global names reported, not stored",
			current:      []string{"FOO"},
			keys:         []string{"GH_TOKEN", "BAR", "GH_TOKEN"},
			wantNext:     []string{"BAR", "FOO"},
			wantInGlobal: []string{"GH_TOKEN"},
		},
		{
			name:     "revoke removes, missing is a no-op",
			current:  []string{"BAR", "FOO", "NPM_TOKEN"},
			keys:     []string{"FOO", "NOT_THERE"},
			revoke:   true,
			wantNext: []string{"BAR", "NPM_TOKEN"},
		},
		{
			name:         "revoke global name reports it and drops a stray stored copy",
			current:      []string{"GH_TOKEN", "FOO"},
			keys:         []string{"GH_TOKEN"},
			revoke:       true,
			wantNext:     []string{"FOO"},
			wantInGlobal: []string{"GH_TOKEN"},
		},
		{
			name:     "revoke everything leaves empty list",
			current:  []string{"FOO"},
			keys:     []string{"FOO"},
			revoke:   true,
			wantNext: []string{},
		},
		{
			name:     "unsorted duplicate current is normalized",
			current:  []string{"Z", "A", "Z", ""},
			keys:     nil,
			wantNext: []string{"A", "Z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, inGlobal := applyApproveEnv(tc.current, tc.keys, global, tc.revoke)
			if tc.wantNext == nil {
				tc.wantNext = []string{}
			}
			if !reflect.DeepEqual(next, tc.wantNext) {
				t.Errorf("next = %q, want %q", next, tc.wantNext)
			}
			if !reflect.DeepEqual(inGlobal, tc.wantInGlobal) {
				t.Errorf("inGlobal = %q, want %q", inGlobal, tc.wantInGlobal)
			}
		})
	}
}

func TestApplyApproveEnv_DoesNotMutateCurrent(t *testing.T) {
	current := []string{"B", "A"}
	_, _ = applyApproveEnv(current, []string{"C"}, nil, false)
	if !reflect.DeepEqual(current, []string{"B", "A"}) {
		t.Errorf("current mutated: %q", current)
	}
}

func TestRunRepoApproveEnv_RejectsInvalidNames(t *testing.T) {
	// Validation runs before any registry / config access, so this needs no
	// fixture: an invalid name must fail fast.
	for _, k := range []string{"A=B", "9X", "\x1b[31mX", ""} {
		if err := runRepoApproveEnv(nil, "any", []string{"OK", k}, false); err == nil {
			t.Errorf("name %q accepted", k)
		}
	}
	if err := runRepoApproveEnv(nil, "any", nil, true); err == nil {
		t.Error("--revoke without keys accepted")
	}
}

func TestRunRepoApproveEnv_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // registry, config and lockfile live under ~/.ahjo
	reg := &registry.Registry{Repos: []registry.Repo{{Name: "acme-api", Aliases: []string{"acme/api", "api"}}}}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	approved := func() []string {
		t.Helper()
		r, err := registry.Load()
		if err != nil {
			t.Fatal(err)
		}
		return r.FindRepo("acme-api").ApprovedForwardEnv
	}

	var out bytes.Buffer
	if err := runRepoApproveEnv(&out, "api", []string{"NPM_TOKEN", "GH_TOKEN", "AWS_KEY"}, false); err != nil {
		t.Fatal(err)
	}
	if got := approved(); !reflect.DeepEqual(got, []string{"AWS_KEY", "NPM_TOKEN"}) {
		t.Errorf("after approve: %q", got)
	}
	for _, want := range []string{
		"GH_TOKEN already forwarded globally",
		"→ approved for api: NPM_TOKEN, AWS_KEY (takes effect on the next ahjo shell / ahjo claude)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runRepoApproveEnv(&out, "acme/api", []string{"NPM_TOKEN"}, true); err != nil {
		t.Fatal(err)
	}
	if got := approved(); !reflect.DeepEqual(got, []string{"AWS_KEY"}) {
		t.Errorf("after revoke: %q", got)
	}
	if !strings.Contains(out.String(), "→ revoked for acme/api: NPM_TOKEN") {
		t.Errorf("revoke output: %s", out.String())
	}

	out.Reset()
	if err := runRepoApproveEnv(&out, "api", nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "approved for api: AWS_KEY\n") {
		t.Errorf("list output: %q", out.String())
	}

	if err := runRepoApproveEnv(&out, "nope", []string{"X"}, false); err == nil ||
		!strings.Contains(err.Error(), `no repo with alias "nope"`) {
		t.Errorf("unknown alias err = %v", err)
	}
}
