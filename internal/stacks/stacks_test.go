package stacks

import (
	"sort"
	"strings"
	"testing"
)

// wantStacks is the canonical list of bundled stacks. A new stack file
// must land here too — keeps "I added a stack file but forgot to
// document/wire it" failures out of the field.
var wantStacks = []string{"go", "node", "php", "python", "ruby", "rust"}

func TestList_MatchesEmbeddedSet(t *testing.T) {
	got := List()
	want := append([]string(nil), wantStacks...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}

func TestLoad_AllStacksParseAndCarryFeatures(t *testing.T) {
	for _, name := range wantStacks {
		t.Run(name, func(t *testing.T) {
			cfg, found, err := Load(name)
			if err != nil {
				t.Fatalf("Load(%q): %v", name, err)
			}
			if !found {
				t.Fatalf("Load(%q): bundled stack reported not found", name)
			}
			if cfg == nil {
				t.Fatalf("Load(%q): nil config with found=true", name)
				return
			}
			if len(cfg.Features) == 0 {
				t.Errorf("stack %q declares no features; every stack should install at least one toolchain Feature", name)
			}
		})
	}
}

func TestLoad_UnknownStack(t *testing.T) {
	cfg, found, err := Load("clojure")
	if err != nil {
		t.Fatalf("Load(unknown): err = %v, want nil", err)
	}
	if found {
		t.Errorf("Load(unknown): found = true, want false")
	}
	if cfg != nil {
		t.Errorf("Load(unknown): cfg non-nil")
	}
}

func TestLoad_EmptyName(t *testing.T) {
	cfg, found, err := Load("")
	if err != nil || found || cfg != nil {
		t.Errorf("Load(\"\"): (%v, %v, %v), want (nil, false, nil)", cfg, found, err)
	}
}

func TestLoad_FeatureSourcesAreCurated(t *testing.T) {
	// All bundled stacks pull from ghcr.io/devcontainers/features/* — the
	// curated namespace applyRepoFeatures auto-trusts. A stack pulling
	// from anywhere else would silently start prompting users on what
	// they think is an ahjo-shipped preset, which would be a surprise.
	for _, name := range wantStacks {
		t.Run(name, func(t *testing.T) {
			cfg, _, err := Load(name)
			if err != nil {
				t.Fatalf("Load(%q): %v", name, err)
			}
			for src := range cfg.Features {
				if !strings.HasPrefix(src, "ghcr.io/devcontainers/features/") {
					t.Errorf("stack %q pulls Feature %q from outside the curated namespace", name, src)
				}
			}
		})
	}
}
