package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func TestPartitionForwardEnv(t *testing.T) {
	global := []string{"GH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
	approved := []string{"NPM_TOKEN"}
	requested := []string{
		"GH_TOKEN",       // global hit
		"NPM_TOKEN",      // approved hit
		"AWS_SECRET",     // pending
		"\x1b[31mEVIL",   // invalid (escape sequence)
		"A=B",            // invalid
		"",               // invalid
		"9LIVES",         // invalid (leading digit)
		"AWS_SECRET",     // duplicate pending
		"NPM_TOKEN",      // duplicate allowed
		"\x1b[31mEVIL",   // duplicate invalid
		"_PRIVATE_THING", // pending
	}
	allowed, pending, invalid := partitionForwardEnv(global, approved, requested)
	if want := []string{"GH_TOKEN", "NPM_TOKEN"}; !reflect.DeepEqual(allowed, want) {
		t.Errorf("allowed = %q, want %q", allowed, want)
	}
	if want := []string{"AWS_SECRET", "_PRIVATE_THING"}; !reflect.DeepEqual(pending, want) {
		t.Errorf("pending = %q, want %q", pending, want)
	}
	if want := []string{"\x1b[31mEVIL", "A=B", "", "9LIVES"}; !reflect.DeepEqual(invalid, want) {
		t.Errorf("invalid = %q, want %q", invalid, want)
	}
}

func TestPartitionForwardEnv_NoApprovedListGrantsNothing(t *testing.T) {
	allowed, pending, _ := partitionForwardEnv(nil, nil, []string{"FOO"})
	if len(allowed) != 0 || !reflect.DeepEqual(pending, []string{"FOO"}) {
		t.Errorf("allowed=%q pending=%q", allowed, pending)
	}
}

func TestForwardEnvKeys(t *testing.T) {
	got := forwardEnvKeys([]string{"GH_TOKEN", "", "X"}, []string{"X", "NPM_TOKEN", "GH_TOKEN"})
	if want := []string{"GH_TOKEN", "X", "NPM_TOKEN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("forwardEnvKeys = %q, want %q", got, want)
	}
}

func TestResolveForwardEnv(t *testing.T) {
	lookup := func(k string) (string, bool) {
		switch k {
		case "A":
			return "from-lookup", true
		case "B":
			return "b-lookup", true
		case "EMPTY":
			return "", true
		}
		return "", false
	}
	repoEnv := map[string]string{"A": "from-repo"}
	got := resolveForwardEnv([]string{"A", "B", "MISSING", "EMPTY", ""}, repoEnv, lookup)
	want := map[string]string{"A": "from-repo", "B": "b-lookup", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveForwardEnv = %v, want %v", got, want)
	}

	if got := resolveForwardEnv(nil, nil, nil); got == nil || len(got) != 0 {
		t.Errorf("empty resolve = %#v, want empty non-nil map", got)
	}
}

// failReader fails the test if the prompt reads stdin when it must not.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Fatal("non-interactive prompt read stdin")
	return 0, nil
}

func TestPromptForwardEnvConsent(t *testing.T) {
	isSet := func(k string) bool { return k == "NPM_TOKEN" }
	pending := []string{"NPM_TOKEN", "FOO", "BAR"}

	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"yes/no/YES", "y\nn\nYES\n", []string{"NPM_TOKEN", "BAR"}},
		{"all no", "n\nno\nwhatever\n", nil},
		{"EOF declines rest", "yes\n", []string{"NPM_TOKEN"}},
		{"empty input", "", nil},
		{"y without newline at EOF", "n\nn\ny", []string{"BAR"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := promptForwardEnvConsent("acme/api", pending, true, isSet, strings.NewReader(tc.input), &out)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("approved = %q, want %q", got, tc.want)
			}
			s := out.String()
			if !strings.Contains(s, "  NPM_TOKEN (set), FOO (not set), BAR (not set)\n") {
				t.Errorf("missing name list in output:\n%s", s)
			}
			if !strings.Contains(s, "Anything running in the container can read forwarded values.") {
				t.Errorf("missing warning line:\n%s", s)
			}
			if !strings.Contains(s, "Forward FOO to this repo's containers? [y/N] ") {
				t.Errorf("missing per-name question:\n%s", s)
			}
			if len(tc.want) < len(pending) && !strings.Contains(s, "approve with: ahjo repo approve-env acme/api") {
				t.Errorf("declined names but no approve hint:\n%s", s)
			}
		})
	}
}

func TestPromptForwardEnvConsent_NonInteractive(t *testing.T) {
	var out bytes.Buffer
	got := promptForwardEnvConsent("acme/api", []string{"FOO", "BAR"}, false, nil, failReader{t}, &out)
	if len(got) != 0 {
		t.Errorf("non-interactive approved %q", got)
	}
	if want := forwardEnvHint("acme/api", []string{"FOO", "BAR"}); out.String() != want {
		t.Errorf("output = %q, want hint %q", out.String(), want)
	}
}

func TestPromptForwardEnvConsent_NothingPending(t *testing.T) {
	var out bytes.Buffer
	if got := promptForwardEnvConsent("a", nil, true, nil, failReader{t}, &out); got != nil || out.Len() != 0 {
		t.Errorf("got %q, output %q", got, out.String())
	}
}

func TestForwardEnvHint(t *testing.T) {
	got := forwardEnvHint("acme/api", []string{"A", "B"})
	want := "note: ahjocontainer.json requests env vars not approved for this repo: A, B — not forwarded.\n" +
		"      approve with: ahjo repo approve-env acme/api A B\n"
	if got != want {
		t.Errorf("hint =\n%q\nwant\n%q", got, want)
	}
	if forwardEnvHint("x", nil) != "" {
		t.Error("hint for no names should be empty")
	}
	if !strings.Contains(forwardEnvHint("", []string{"A"}), "approve-env <repo-alias> A") {
		t.Error("empty alias should print a placeholder")
	}
}

func TestForwardEnvSkipReport_QuotesInvalid(t *testing.T) {
	var out bytes.Buffer
	forwardEnvSkip{RepoAlias: "r", Pending: []string{"FOO"}, Invalid: []string{"\x1b[2J"}}.report(&out)
	s := out.String()
	if strings.Contains(s, "\x1b") {
		t.Errorf("raw escape byte reached output: %q", s)
	}
	if !strings.Contains(s, `warn: ignoring invalid forward_env name "\x1b[2J"`) {
		t.Errorf("invalid name not %%q-reported: %q", s)
	}
	if !strings.Contains(s, "approve-env r FOO") {
		t.Errorf("missing hint: %q", s)
	}
}

func TestRequestedForwardEnv(t *testing.T) {
	a := &ahjocontainer.Config{}
	a.Customizations.Ahjo.ForwardEnv = []string{"A", "B"}
	b := &ahjocontainer.Config{}
	b.Customizations.Ahjo.ForwardEnv = []string{"B", "C"}
	got := requestedForwardEnv(a, nil, b)
	if want := []string{"A", "B", "B", "C"}; !reflect.DeepEqual(got, want) {
		t.Errorf("requestedForwardEnv = %q, want %q", got, want)
	}
	if got := requestedForwardEnv(nil); got != nil {
		t.Errorf("nil config: %q", got)
	}
}

func TestRepoForContainerIn(t *testing.T) {
	reg := &registry.Registry{
		Repos: []registry.Repo{
			{Name: "acme-api", Aliases: []string{"acme/api"}, BaseContainerName: "ahjo-acme-api"},
		},
		Branches: []registry.Branch{
			{Repo: "acme-api", IncusName: "ahjo-acme-api-feat-x"},
			{Repo: "gone", IncusName: "ahjo-gone-main"},
		},
	}
	if r := repoForContainerIn(reg, "ahjo-acme-api"); r == nil || r.Name != "acme-api" {
		t.Errorf("base container: %v", r)
	}
	if r := repoForContainerIn(reg, "ahjo-acme-api-feat-x"); r == nil || r.Name != "acme-api" {
		t.Errorf("branch container: %v", r)
	}
	if r := repoForContainerIn(reg, "ahjo-gone-main"); r != nil {
		t.Errorf("orphan branch row should yield nil, got %v", r)
	}
	if r := repoForContainerIn(reg, "nope"); r != nil {
		t.Errorf("unknown container: %v", r)
	}
}

// TestBranchEnv_GatesRequestedNames exercises the attach-time wiring, not
// just the helpers: a name the in-container config requests is forwarded
// only when the registry row approves it.
func TestBranchEnv_GatesRequestedNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // registry, config and env files live under HOME
	t.Setenv("OK_VAR", "ok-value")
	t.Setenv("EVIL_VAR", "secret")
	reg := &registry.Registry{Repos: []registry.Repo{{
		Name:               "acme-api",
		Aliases:            []string{"acme/api"},
		BaseContainerName:  "ahjo-acme-api",
		ApprovedForwardEnv: []string{"OK_VAR"},
	}}}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	dc := &ahjocontainer.Config{}
	dc.Customizations.Ahjo.ForwardEnv = []string{"OK_VAR", "EVIL_VAR", "bad name"}

	env, skip, err := branchEnv("ahjo-acme-api", dc)
	if err != nil {
		t.Fatal(err)
	}
	if env["OK_VAR"] != "ok-value" {
		t.Errorf("approved name not forwarded: %q", env["OK_VAR"])
	}
	if _, ok := env["EVIL_VAR"]; ok {
		t.Error("unapproved name was forwarded")
	}
	if !reflect.DeepEqual(skip.Pending, []string{"EVIL_VAR"}) || !reflect.DeepEqual(skip.Invalid, []string{"bad name"}) {
		t.Errorf("skip = %+v", skip)
	}
	if skip.RepoAlias != "acme/api" {
		t.Errorf("RepoAlias = %q", skip.RepoAlias)
	}

	// No registry row for the container: nothing repo-requested is forwarded.
	env, skip, err = branchEnv("ahjo-unknown", dc)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["OK_VAR"]; ok {
		t.Error("name forwarded without a registry row")
	}
	if !reflect.DeepEqual(skip.Pending, []string{"OK_VAR", "EVIL_VAR"}) {
		t.Errorf("pending without row = %q", skip.Pending)
	}
}
