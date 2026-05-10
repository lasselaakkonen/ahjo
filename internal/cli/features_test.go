package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
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
	cfg := &devcontainer.Config{}
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
	cfg := &devcontainer.Config{
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
	cfg := &devcontainer.Config{
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

func TestApplyRepoFeatures_PriorConsentSkipsPrompt(t *testing.T) {
	cfg := &devcontainer.Config{
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

