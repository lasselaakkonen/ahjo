package registry

import (
	"strings"
	"testing"
)

func TestAliasToSlugCaps(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := AliasToSlug(long)
	if len(got) != maxSlugLen {
		t.Fatalf("AliasToSlug len = %d, want %d", len(got), maxSlugLen)
	}
}

func TestWithSlugSuffix(t *testing.T) {
	tests := []struct {
		name string
		base string
		n    int
		want string
	}{
		{"short base", "foo", 2, "foo-2"},
		{"at cap", strings.Repeat("a", maxSlugLen), 2, strings.Repeat("a", maxSlugLen-2) + "-2"},
		{"trailing dash trimmed", strings.Repeat("a", maxSlugLen-3) + "---", 9, strings.Repeat("a", maxSlugLen-3) + "-9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withSlugSuffix(tt.base, tt.n)
			if got != tt.want {
				t.Fatalf("withSlugSuffix(%q,%d) = %q, want %q", tt.base, tt.n, got, tt.want)
			}
			if len(got) > maxSlugLen {
				t.Fatalf("withSlugSuffix overflowed: len=%d", len(got))
			}
		})
	}
}

// Regression: when MakeSlug's base lands at the cap and is taken, the loop
// must produce a distinct slug (the prior implementation truncated the
// suffix off, returning the colliding base).
func TestMakeSlugCollisionAtCap(t *testing.T) {
	repoSlug := strings.Repeat("a", 30)
	branch := strings.Repeat("b", 30)
	r := &Registry{}
	first := r.MakeSlug(repoSlug, branch)
	if len(first) != maxSlugLen {
		t.Fatalf("first slug len = %d, want %d", len(first), maxSlugLen)
	}
	r.Branches = append(r.Branches, Branch{Slug: first})
	second := r.MakeSlug(repoSlug, branch)
	if second == first {
		t.Fatalf("collision not resolved: both slugs = %q", first)
	}
	if len(second) > maxSlugLen {
		t.Fatalf("second slug overflowed: len=%d", len(second))
	}
	if !strings.HasSuffix(second, "-2") {
		t.Fatalf("expected -2 suffix, got %q", second)
	}
}
