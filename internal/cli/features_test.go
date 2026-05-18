package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/ahjocontainer"
)

func TestApplyRepoFeatures_Noop(t *testing.T) {
	// nil cfg → no-op, no consent recorded.
	consent, err := applyRepoFeatures(context.Background(), "x", nil, nil, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("nil cfg should be no-op: %v", err)
	}
	if len(consent) != 0 {
		t.Fatalf("consent = %v", consent)
	}

	// Empty Features → also no-op.
	cfg := &ahjocontainer.Config{}
	consent, err = applyRepoFeatures(context.Background(), "x", cfg, nil, strings.NewReader(""), &bytes.Buffer{})
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
	_, err := applyRepoFeatures(context.Background(), "x", cfg, nil, in, out)
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
	_, err := applyRepoFeatures(context.Background(), "x", cfg, nil, in, out)
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
	_, err := applyRepoFeatures(context.Background(), "x", cfg, nil, in, out)
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
	_, err := applyRepoFeatures(context.Background(), "x", cfg, nil, in, out)
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
	_, err := applyRepoFeatures(context.Background(), "x", cfg, prior, in, out)
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
