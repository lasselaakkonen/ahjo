package devcontainer

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeFetcher returns a FetchFunc whose results are predetermined by
// metadata, indexed by canonical ref. Useful for testing Resolve in
// isolation from network + tar.
func fakeFetcher(metadata map[string]*Metadata) FetchFunc {
	return func(_ context.Context, ref FeatureRef, opts map[string]any) (FetchedFeature, error) {
		key := ref.String()
		m, ok := metadata[key]
		if !ok {
			// Try matching by stripped form so test fixtures with bare
			// `ghcr.io/x/y` (no tag) work alongside `ghcr.io/x/y:1`.
			m, ok = metadata[stripRefVersion(key)]
		}
		if !ok {
			return FetchedFeature{}, fmt.Errorf("no metadata for %s", key)
		}
		strOpts, err := NormalizeOptions(opts)
		if err != nil {
			return FetchedFeature{}, err
		}
		return FetchedFeature{
			Ref:      ref,
			Feature:  Feature{ID: key, Dir: "/fake/" + key, Options: strOpts},
			Metadata: m,
		}, nil
	}
}

func TestResolve_HardDepsTransitive(t *testing.T) {
	meta := map[string]*Metadata{
		"ghcr.io/x/a:1": {ID: "a", DependsOn: map[string]map[string]any{
			"ghcr.io/x/b:1": {},
		}},
		"ghcr.io/x/b:1": {ID: "b", DependsOn: map[string]map[string]any{
			"ghcr.io/x/c:1": {},
		}},
		"ghcr.io/x/c:1": {ID: "c"},
	}
	features := map[string]any{
		"ghcr.io/x/a:1": map[string]any{},
	}
	got, err := Resolve(context.Background(), features, fakeFetcher(meta))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []string{"ghcr.io/x/c:1", "ghcr.io/x/b:1", "ghcr.io/x/a:1"}
	if got, w := refsString(got), strings.Join(want, " → "); got != w {
		t.Fatalf("order = %s\nwant = %s", got, w)
	}
}

func TestResolve_InstallsAfterSoftConditional(t *testing.T) {
	// node depends on nothing but says "install me after common-utils".
	// When common-utils is in the install set, the soft edge applies.
	meta := map[string]*Metadata{
		"ghcr.io/x/node:1":         {ID: "node", InstallsAfter: []string{"ghcr.io/x/common-utils"}},
		"ghcr.io/x/common-utils:1": {ID: "common-utils"},
	}
	t.Run("present", func(t *testing.T) {
		features := map[string]any{
			"ghcr.io/x/node:1":         map[string]any{},
			"ghcr.io/x/common-utils:1": map[string]any{},
		}
		got, err := Resolve(context.Background(), features, fakeFetcher(meta))
		if err != nil {
			t.Fatal(err)
		}
		want := "ghcr.io/x/common-utils:1 → ghcr.io/x/node:1"
		if r := refsString(got); r != want {
			t.Fatalf("order = %s; want %s", r, want)
		}
	})
	t.Run("absent", func(t *testing.T) {
		// Without common-utils declared, installsAfter is a no-op.
		features := map[string]any{
			"ghcr.io/x/node:1": map[string]any{},
		}
		got, err := Resolve(context.Background(), features, fakeFetcher(meta))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Ref.String() != "ghcr.io/x/node:1" {
			t.Fatalf("order = %v", refsString(got))
		}
	})
}

func TestResolve_DetectsCycle(t *testing.T) {
	meta := map[string]*Metadata{
		"ghcr.io/x/a:1": {ID: "a", DependsOn: map[string]map[string]any{"ghcr.io/x/b:1": {}}},
		"ghcr.io/x/b:1": {ID: "b", DependsOn: map[string]map[string]any{"ghcr.io/x/a:1": {}}},
	}
	features := map[string]any{
		"ghcr.io/x/a:1": map[string]any{},
	}
	_, err := Resolve(context.Background(), features, fakeFetcher(meta))
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error should mention cycle: %v", err)
	}
}

func TestResolve_OptionsPlumbedToFetch(t *testing.T) {
	captured := map[string]map[string]any{}
	fetch := func(_ context.Context, ref FeatureRef, opts map[string]any) (FetchedFeature, error) {
		captured[ref.String()] = opts
		return FetchedFeature{
			Ref:     ref,
			Feature: Feature{ID: ref.String(), Dir: "/fake"},
			Metadata: &Metadata{
				ID: ref.String(),
			},
		}, nil
	}
	features := map[string]any{
		"ghcr.io/x/node:1": map[string]any{"version": "20", "tls": true},
	}
	if _, err := Resolve(context.Background(), features, fetch); err != nil {
		t.Fatal(err)
	}
	got := captured["ghcr.io/x/node:1"]
	if got["version"] != "20" || got["tls"] != true {
		t.Fatalf("options = %v", got)
	}
}

func TestNormalizeOptions(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want map[string]string
	}{
		{"object", map[string]any{"version": "20", "tls": true},
			map[string]string{"VERSION": "20", "TLS": "true"}},
		{"string-shorthand", "20",
			map[string]string{"VERSION": "20"}},
		{"bool-shorthand", true,
			map[string]string{"VERSION": "true"}},
		{"int-shorthand", float64(20),
			map[string]string{"VERSION": "20"}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeOptions(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("%s: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestStripRefVersion(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/x/y:1":          "ghcr.io/x/y",
		"ghcr.io/x/y@sha256:abc": "ghcr.io/x/y",
		"ghcr.io/x/y":            "ghcr.io/x/y",
		"localhost:5000/x/y:dev": "localhost:5000/x/y",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := stripRefVersion(in); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func refsString(ff []FetchedFeature) string {
	parts := make([]string, len(ff))
	for i, f := range ff {
		parts[i] = f.Ref.String()
	}
	return strings.Join(parts, " → ")
}
