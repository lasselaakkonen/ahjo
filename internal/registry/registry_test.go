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

// Regression: truncating an over-long slug at the cap must not leave a
// trailing "-", which Incus rejects ("Name must not end with - character").
func TestAliasToSlugNoTrailingDashAtCap(t *testing.T) {
	// Place a dash-producing char so the cut at maxSlugLen lands on a dash.
	alias := strings.Repeat("a", maxSlugLen-1) + "/" + strings.Repeat("b", 10)
	got := AliasToSlug(alias)
	if strings.HasSuffix(got, "-") {
		t.Fatalf("AliasToSlug = %q, must not end with -", got)
	}
	if len(got) > maxSlugLen {
		t.Fatalf("AliasToSlug len = %d, want <= %d", len(got), maxSlugLen)
	}
}

func TestMakeSlugNoTrailingDashAtCap(t *testing.T) {
	repoSlug := "repo"
	// base = "repo-" (5) + branch; put the dash at index maxSlugLen-1 so the
	// cut keeps it as the final char.
	branch := strings.Repeat("a", maxSlugLen-6) + "-" + strings.Repeat("b", 10)
	r := &Registry{}
	got := r.MakeSlug(repoSlug, branch)
	if strings.HasSuffix(got, "-") {
		t.Fatalf("MakeSlug = %q, must not end with -", got)
	}
	if len(got) > maxSlugLen {
		t.Fatalf("MakeSlug len = %d, want <= %d", len(got), maxSlugLen)
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

func TestAllocateRepoSlugExternalTaken(t *testing.T) {
	r := &Registry{}
	taken := map[string]bool{"acme-foo": true}
	got := r.AllocateRepoSlug("acme/foo", func(s string) bool { return taken[s] })
	if got != "acme-foo-2" {
		t.Fatalf("first call: got %q, want %q", got, "acme-foo-2")
	}
	taken["acme-foo-2"] = true
	got = r.AllocateRepoSlug("acme/foo", func(s string) bool { return taken[s] })
	if got != "acme-foo-3" {
		t.Fatalf("second call: got %q, want %q", got, "acme-foo-3")
	}
}

func TestAllocateRepoSlugNilPredicate(t *testing.T) {
	r := &Registry{Repos: []Repo{{Name: "acme-foo"}}}
	got := r.AllocateRepoSlug("acme/foo", nil)
	if got != "acme-foo-2" {
		t.Fatalf("got %q, want %q", got, "acme-foo-2")
	}
}

func TestBranchHostKeysSlug(t *testing.T) {
	tests := []struct {
		name string
		br   Branch
		want string
	}{
		{
			"default branch shares base container dir",
			Branch{Slug: "acme-main", IncusName: "ahjo-acme", IsDefault: true},
			"acme",
		},
		{
			"non-default branch uses its own dir",
			Branch{Slug: "acme-feature", IncusName: "ahjo-acme-feature"},
			"acme-feature",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.br.HostKeysSlug(); got != tt.want {
				t.Fatalf("HostKeysSlug() = %q, want %q", got, tt.want)
			}
		})
	}
}
