package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
)

func TestApplyRepoFeatures_Noop(t *testing.T) {
	// nil cfg → no-op, no consent recorded.
	_, consent, err := applyRepoFeatures(context.Background(), "x", nil, nil, nil, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("nil cfg should be no-op: %v", err)
	}
	if len(consent) != 0 {
		t.Fatalf("consent = %v", consent)
	}

	// Empty Features → also no-op.
	cfg := &ahjocontainer.Config{}
	_, consent, err = applyRepoFeatures(context.Background(), "x", cfg, nil, nil, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("empty features should be no-op: %v", err)
	}
	if len(consent) != 0 {
		t.Fatalf("consent = %v", consent)
	}
}

func TestApplyRepoFeatures_DeclinedTrustAborts(t *testing.T) {
	// User declines the trust prompt → returns an error citing the
	// glob, before any network call. The non-curated source ensures
	// we hit the prompt path rather than auto-trust.
	cfg := &ahjocontainer.Config{
		Features: map[string]any{
			"ghcr.io/acme/foo:1": map[string]any{},
		},
	}
	in := strings.NewReader("n\n")
	out := &bytes.Buffer{}
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, nil, nil, in, out)
	if err == nil {
		t.Fatal("expected error on declined trust")
	}
	if !strings.Contains(err.Error(), "ghcr.io/acme/*") {
		t.Fatalf("error should cite the glob: %v", err)
	}
	if !strings.Contains(out.String(), "Trust Features matching") {
		t.Fatalf("expected trust prompt in output; got: %q", out.String())
	}
}

func TestApplyRepoFeatures_CuratedAutoTrustNoPrompt(t *testing.T) {
	// All Features under the curated namespace — no prompt should
	// appear. We don't have real network here, so the function will
	// fail at fetch time; what matters is the prompt text isn't shown
	// and no consent is recorded for ghcr.io/devcontainers/features/*.
	cfg := &ahjocontainer.Config{
		Features: map[string]any{
			"ghcr.io/devcontainers/features/node:1": map[string]any{},
		},
	}
	in := strings.NewReader("")
	out := &bytes.Buffer{}
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, nil, nil, in, out)
	// Expected: error during fetch (no real registry); but the prompt
	// must NOT have been shown.
	if err == nil {
		t.Fatal("expected fetch error against unreachable registry")
	}
	if strings.Contains(out.String(), "Trust Features matching") {
		t.Fatalf("curated source must not prompt; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "auto-trusted") {
		t.Fatalf("expected auto-trusted line; output:\n%s", out.String())
	}
}

func TestApplyRepoFeatures_BuiltinAutoTrustNoPrompt(t *testing.T) {
	// `ahjo/<name>` resolves to a binary-embedded Feature; trust posture
	// is auto under BuiltinTrustedGlob, dispatch is in-process (no OCI
	// fetch). We don't have a real container here, so Apply will fail at
	// `incus exec`; what matters is that:
	//   - the auto-trusted line names ahjo/*,
	//   - no prompt is shown,
	//   - the error path is past Resolve (i.e. the built-in materialized
	//     successfully) — anything else would mean dispatch is wrong.
	cfg := &ahjocontainer.Config{
		Features: map[string]any{
			"ahjo/docker": map[string]any{},
		},
	}
	in := strings.NewReader("")
	out := &bytes.Buffer{}
	approve := func([]string) error { return nil }
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, nil, approve, in, out)
	if err == nil {
		t.Fatal("expected Apply to fail without a real container")
	}
	if strings.Contains(out.String(), "Trust Features matching") {
		t.Fatalf("ahjo/* must not prompt; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "auto-trusted") || !strings.Contains(out.String(), "ahjo/*") {
		t.Fatalf("expected ahjo/* in auto-trusted line; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ahjo/docker") {
		t.Fatalf("expected feature line for ahjo/docker; output:\n%s", out.String())
	}
}

func TestApplyRepoFeatures_UnknownBuiltin(t *testing.T) {
	cfg := &ahjocontainer.Config{
		Features: map[string]any{
			"ahjo/dockerd": map[string]any{},
		},
	}
	in := strings.NewReader("")
	out := &bytes.Buffer{}
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, nil, nil, in, out)
	if err == nil {
		t.Fatal("expected unknown built-in to error")
	}
	if !strings.Contains(err.Error(), "unknown built-in feature") {
		t.Fatalf("error should mention unknown built-in; got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Fatalf("error should list known built-ins; got: %v", err)
	}
}

func TestApplyRepoFeatures_PriorConsentSkipsPrompt(t *testing.T) {
	cfg := &ahjocontainer.Config{
		Features: map[string]any{
			"ghcr.io/acme/foo:1": map[string]any{},
		},
	}
	prior := map[string]bool{"ghcr.io/acme/*": true}
	in := strings.NewReader("")
	out := &bytes.Buffer{}
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, prior, nil, in, out)
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if strings.Contains(out.String(), "Trust Features matching") {
		t.Fatalf("prior consent must skip prompt; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "previously trusted") {
		t.Fatalf("expected `previously trusted` line; output:\n%s", out.String())
	}
}

func TestApplyRepoFeatures_NestingConfirmedBeforeApply(t *testing.T) {
	// ahjo/docker requests security.nesting. The callback must run after
	// Resolve (so it sees the resolved ref) and before any Feature is
	// applied; its error must abort with no apply output written.
	cfg := &ahjocontainer.Config{
		Features: map[string]any{
			"ahjo/docker": map[string]any{},
			"ahjo/prek":   map[string]any{},
		},
	}
	out := &bytes.Buffer{}
	var gotRefs []string
	calls := 0
	deny := errors.New("nesting denied by test")
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, nil,
		func(refs []string) error {
			calls++
			gotRefs = refs
			return deny
		},
		strings.NewReader(""), out)
	if !errors.Is(err, deny) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if calls != 1 {
		t.Fatalf("callback called %d times, want 1", calls)
	}
	if want := []string{"ahjo/docker"}; !equalStrings(gotRefs, want) {
		t.Fatalf("requestedBy = %v, want %v", gotRefs, want)
	}
	if strings.Contains(out.String(), "(apply)") {
		t.Fatalf("no Feature may be applied before nesting is confirmed; output:\n%s", out.String())
	}
}

func TestApplyRepoFeatures_NestingNilCallbackDenies(t *testing.T) {
	cfg := &ahjocontainer.Config{
		Features: map[string]any{"ahjo/docker": map[string]any{}},
	}
	out := &bytes.Buffer{}
	_, _, err := applyRepoFeatures(context.Background(), "x", cfg, nil, nil, strings.NewReader(""), out)
	if err == nil {
		t.Fatal("nil confirmNesting must deny, not silently proceed")
	}
	if !strings.Contains(err.Error(), "security.nesting") || !strings.Contains(err.Error(), "ahjo/docker") {
		t.Fatalf("error should name security.nesting and the Feature; got: %v", err)
	}
	if strings.Contains(out.String(), "(apply)") {
		t.Fatalf("no Feature may be applied when nesting is denied; output:\n%s", out.String())
	}
}

func TestApplyRepoFeatures_NoNestingFeatureSkipsCallback(t *testing.T) {
	// ahjo/prek does not request nesting: the callback must not be called
	// and the pipeline proceeds to Apply (which fails without a real
	// container — that failure is expected and not what's under test).
	cfg := &ahjocontainer.Config{
		Features: map[string]any{"ahjo/prek": map[string]any{}},
	}
	out := &bytes.Buffer{}
	called := false
	_, _, _ = applyRepoFeatures(context.Background(), "x", cfg, nil,
		func([]string) error { called = true; return nil },
		strings.NewReader(""), out)
	if called {
		t.Fatal("confirmNesting must not be called when no Feature requests nesting")
	}
	if !strings.Contains(out.String(), "feature ahjo/prek (apply)") {
		t.Fatalf("expected the pipeline to reach Apply; output:\n%s", out.String())
	}
}

func TestFeaturesRequestingNesting_SortedBuiltinsOnly(t *testing.T) {
	mk := func(reg, repo string, nesting bool) devcontainer.FetchedFeature {
		ff := devcontainer.FetchedFeature{
			Ref:      devcontainer.FeatureRef{Registry: reg, Repository: repo},
			Metadata: &devcontainer.Metadata{},
		}
		ff.Metadata.Customizations.Ahjo.Nesting = nesting
		return ff
	}
	ffs := []devcontainer.FetchedFeature{
		mk("ahjo", "zeta", true),
		mk("ghcr.io", "acme/evil", true),
		mk("ahjo", "prek", false),
		mk("ahjo", "docker", true),
	}
	got := featuresRequestingNesting(ffs)
	if want := []string{"ahjo/docker", "ahjo/zeta"}; !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFeaturesRequestNesting_Empty(t *testing.T) {
	// No features → no nesting request.
	if featuresRequestNesting(nil) {
		t.Fatal("nil slice should return false")
	}
	if featuresRequestNesting([]devcontainer.FetchedFeature{}) {
		t.Fatal("empty slice should return false")
	}
}

func TestFeaturesRequestNesting_AhjoBuiltinNestingTrue(t *testing.T) {
	// An ahjo/* built-in with Nesting:true → requests nesting.
	ffs := []devcontainer.FetchedFeature{
		{
			Ref:      devcontainer.FeatureRef{Registry: "ahjo", Repository: "docker"},
			Metadata: &devcontainer.Metadata{},
		},
	}
	ffs[0].Metadata.Customizations.Ahjo.Nesting = true

	if !featuresRequestNesting(ffs) {
		t.Fatal("ahjo/* built-in with Nesting=true should return true")
	}
}

func TestFeaturesRequestNesting_AhjoBuiltinNestingFalse(t *testing.T) {
	// An ahjo/* built-in without Nesting:true → does not request nesting.
	ffs := []devcontainer.FetchedFeature{
		{
			Ref:      devcontainer.FeatureRef{Registry: "ahjo", Repository: "prek"},
			Metadata: &devcontainer.Metadata{},
		},
	}
	if featuresRequestNesting(ffs) {
		t.Fatal("ahjo/* built-in with Nesting=false should return false")
	}
}

func TestFeaturesRequestNesting_OCIFeatureIgnored(t *testing.T) {
	// An OCI Feature that sets Nesting:true must be silently ignored —
	// only ahjo/* built-ins may request Incus config changes.
	ffs := []devcontainer.FetchedFeature{
		{
			Ref: devcontainer.FeatureRef{
				Registry:   "ghcr.io",
				Repository: "devcontainers/features/docker-in-docker",
			},
			Metadata: &devcontainer.Metadata{},
		},
	}
	ffs[0].Metadata.Customizations.Ahjo.Nesting = true

	if featuresRequestNesting(ffs) {
		t.Fatal("OCI Feature with Nesting=true must be ignored (only ahjo/* built-ins honored)")
	}
}

func TestFeaturesRequestNesting_NilMetadata(t *testing.T) {
	// Nil Metadata (shouldn't happen in practice but must not panic).
	ffs := []devcontainer.FetchedFeature{
		{
			Ref:      devcontainer.FeatureRef{Registry: "ahjo", Repository: "docker"},
			Metadata: nil,
		},
	}
	if featuresRequestNesting(ffs) {
		t.Fatal("nil Metadata should not panic and should return false")
	}
}

func TestSecurityConfigFlags_NestingAbsent(t *testing.T) {
	// security.nesting must NOT be in the default flags — it is now
	// gated behind the ahjo/docker Feature and customizations.ahjo.nested_incus.
	for _, kv := range securityConfigFlags() {
		if kv[0] == "security.nesting" {
			t.Fatalf("securityConfigFlags must not include security.nesting; found %v", kv)
		}
	}
}

func TestSecurityConfigFlags_RequiredKeysPresent(t *testing.T) {
	required := []string{
		"security.syscalls.intercept.setxattr",
		"linux.sysctl.net.ipv4.ip_unprivileged_port_start",
		"security.guestapi",
	}
	flags := securityConfigFlags()
	byKey := make(map[string]string, len(flags))
	for _, kv := range flags {
		byKey[kv[0]] = kv[1]
	}
	for _, k := range required {
		if _, ok := byKey[k]; !ok {
			t.Errorf("securityConfigFlags missing required key %q", k)
		}
	}
}
