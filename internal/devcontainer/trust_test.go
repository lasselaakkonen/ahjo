package devcontainer

import (
	"reflect"
	"sort"
	"testing"
)

func TestSourceToGlob(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/devcontainers/features/node:1":       "ghcr.io/devcontainers/features/*",
		"ghcr.io/devcontainers/features/common-utils": "ghcr.io/devcontainers/features/*",
		"ghcr.io/foo/bar/baz:2.1":                     "ghcr.io/foo/bar/*",
		"ghcr.io/foo/single@sha256:abc":               "ghcr.io/foo/*",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := SourceToGlob(in); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestMatchesGlob(t *testing.T) {
	type kase struct {
		glob, source string
		want         bool
	}
	cases := []kase{
		{"ghcr.io/devcontainers/features/*", "ghcr.io/devcontainers/features/node:1", true},
		{"ghcr.io/devcontainers/features/*", "ghcr.io/devcontainers/features/node", true},
		{"ghcr.io/devcontainers/features/*", "ghcr.io/devcontainers/features/sub/node", false}, // * doesn't cross /
		{"ghcr.io/foo/*", "ghcr.io/bar/baz:1", false},
		{"ghcr.io/foo/*", "ghcr.io/foo/baz:1", true},
	}
	for _, c := range cases {
		if got := MatchesGlob(c.glob, c.source); got != c.want {
			t.Errorf("Match(%q, %q) = %v; want %v", c.glob, c.source, got, c.want)
		}
	}
}

func TestIsCuratedTrusted(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/devcontainers/features/node:1":       true,
		"ghcr.io/devcontainers/features/common-utils": true,
		"ghcr.io/devcontainers/features/sub/x":        false, // * doesn't cross /
		"ghcr.io/foo/node:1":                          false,
		"docker.io/devcontainers/features/node:1":     false,
	}
	for src, want := range cases {
		if got := IsCuratedTrusted(src); got != want {
			t.Errorf("IsCuratedTrusted(%q) = %v; want %v", src, got, want)
		}
	}
}

func TestPartitionFeatureSources(t *testing.T) {
	sources := []string{
		"ghcr.io/devcontainers/features/node:1",
		"ghcr.io/devcontainers/features/common-utils",
		"ghcr.io/acme/foo:1",
		"ghcr.io/acme/bar:1",
		"ghcr.io/widgets/baz:1",
	}
	consented := []string{
		"ghcr.io/acme/*",
	}
	auto, known, prompt := PartitionFeatureSources(sources, consented)
	sort.Strings(auto)
	sort.Strings(known)
	sort.Strings(prompt)

	wantAuto := []string{"ghcr.io/devcontainers/features/*"}
	wantKnown := []string{"ghcr.io/acme/*"}
	wantPrompt := []string{"ghcr.io/widgets/*"}

	if !reflect.DeepEqual(auto, wantAuto) {
		t.Errorf("auto = %v; want %v", auto, wantAuto)
	}
	if !reflect.DeepEqual(known, wantKnown) {
		t.Errorf("known = %v; want %v", known, wantKnown)
	}
	if !reflect.DeepEqual(prompt, wantPrompt) {
		t.Errorf("prompt = %v; want %v", prompt, wantPrompt)
	}
}

func TestPartitionFeatureSources_Dedupe(t *testing.T) {
	// Two refs from the same publisher should produce one bucket entry.
	sources := []string{
		"ghcr.io/acme/foo:1",
		"ghcr.io/acme/bar:2",
	}
	auto, known, prompt := PartitionFeatureSources(sources, nil)
	if len(auto) != 0 || len(known) != 0 {
		t.Fatalf("auto/known unexpectedly populated: %v / %v", auto, known)
	}
	if len(prompt) != 1 || prompt[0] != "ghcr.io/acme/*" {
		t.Fatalf("prompt = %v; want one ghcr.io/acme/*", prompt)
	}
}
