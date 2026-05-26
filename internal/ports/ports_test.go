package ports

import (
	"reflect"
	"testing"
)

// newPorts builds an in-memory store with a small range so allocation and
// exhaustion are easy to drive without touching disk.
func newPorts(min, max int, allocs ...Allocation) *Ports {
	return &Ports{Version: Version, Range: Range{Min: min, Max: max}, Allocations: allocs}
}

func TestAllocate_AssignsLowestFreePort(t *testing.T) {
	p := newPorts(10000, 10002)
	got, err := p.Allocate("repoA", PurposeSSH)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != 10000 {
		t.Fatalf("first allocation = %d, want 10000 (range min)", got)
	}
	if len(p.Allocations) != 1 {
		t.Fatalf("expected 1 allocation recorded, got %d", len(p.Allocations))
	}
}

func TestAllocate_IsIdempotentPerSlugPurpose(t *testing.T) {
	p := newPorts(10000, 10010)
	first, _ := p.Allocate("repoA", PurposeSSH)
	again, err := p.Allocate("repoA", PurposeSSH)
	if err != nil {
		t.Fatalf("Allocate(again): %v", err)
	}
	if again != first {
		t.Fatalf("re-Allocate = %d, want stable %d", again, first)
	}
	if len(p.Allocations) != 1 {
		t.Fatalf("idempotent allocate must not add a row; got %d", len(p.Allocations))
	}
}

func TestAllocate_SkipsUsedPorts(t *testing.T) {
	// 10000 and 10001 already taken; next free is 10002.
	p := newPorts(10000, 10005,
		Allocation{Slug: "x", Port: 10000, Purpose: PurposeSSH},
		Allocation{Slug: "y", Port: 10001, Purpose: PurposeSSH},
	)
	got, err := p.Allocate("z", PurposeSSH)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != 10002 {
		t.Fatalf("Allocate = %d, want 10002 (lowest free)", got)
	}
}

func TestAllocate_ExhaustionErrors(t *testing.T) {
	p := newPorts(10000, 10000, Allocation{Slug: "x", Port: 10000, Purpose: PurposeSSH})
	if _, err := p.Allocate("y", PurposeSSH); err == nil {
		t.Fatal("expected error when the range is exhausted")
	}
}

func TestFind(t *testing.T) {
	p := newPorts(10000, 10010, Allocation{Slug: "x", Port: 10000, Purpose: PurposeSSH})
	if a := p.Find("x", PurposeSSH); a == nil || a.Port != 10000 {
		t.Fatalf("Find(x,ssh) = %v, want port 10000", a)
	}
	if a := p.Find("x", "expose-3000"); a != nil {
		t.Fatalf("Find(x,expose-3000) = %v, want nil", a)
	}
}

func TestFreeSlug(t *testing.T) {
	p := newPorts(10000, 10010,
		Allocation{Slug: "x", Port: 10000, Purpose: PurposeSSH},
		Allocation{Slug: "x", Port: 10001, Purpose: "expose-3000"},
		Allocation{Slug: "y", Port: 10002, Purpose: PurposeSSH},
	)
	p.FreeSlug("x")
	if len(p.Allocations) != 1 || p.Allocations[0].Slug != "y" {
		t.Fatalf("FreeSlug(x) left %v, want only y's allocation", p.Allocations)
	}
}

func TestFreePurpose(t *testing.T) {
	p := newPorts(10000, 10010,
		Allocation{Slug: "x", Port: 10000, Purpose: PurposeSSH},
		Allocation{Slug: "x", Port: 10001, Purpose: "auto-3000"},
	)
	p.FreePurpose("x", "auto-3000")
	want := []Allocation{{Slug: "x", Port: 10000, Purpose: PurposeSSH}}
	if !reflect.DeepEqual(p.Allocations, want) {
		t.Fatalf("FreePurpose left %v, want %v", p.Allocations, want)
	}
}

func TestAllocationsForSlug(t *testing.T) {
	p := newPorts(10000, 10010,
		Allocation{Slug: "x", Port: 10000, Purpose: PurposeSSH},
		Allocation{Slug: "y", Port: 10001, Purpose: PurposeSSH},
		Allocation{Slug: "x", Port: 10002, Purpose: "expose-8080"},
	)
	got := p.AllocationsForSlug("x")
	if len(got) != 2 {
		t.Fatalf("AllocationsForSlug(x) = %d rows, want 2", len(got))
	}
	for _, a := range got {
		if a.Slug != "x" {
			t.Fatalf("AllocationsForSlug(x) returned a %q row: %+v", a.Slug, a)
		}
	}
}

// TestExposedPairs pins the parsing the ls table and TUI both depend on: keep
// only expose-/auto- purposes, parse the trailing container port, skip ssh and
// malformed rows, and sort by container port.
func TestExposedPairs(t *testing.T) {
	allocs := []Allocation{
		{Slug: "x", Port: 10000, Purpose: PurposeSSH},       // skipped (ssh)
		{Slug: "x", Port: 10005, Purpose: "expose-8080"},    // → {8080, 10005}
		{Slug: "x", Port: 10003, Purpose: "auto-3000"},      // → {3000, 10003}
		{Slug: "x", Port: 10009, Purpose: "expose-notanum"}, // skipped (unparseable)
		{Slug: "x", Port: 10010, Purpose: "weird-1"},        // skipped (unknown prefix)
	}
	got := ExposedPairs(allocs)
	want := []PortPair{
		{Container: 3000, Host: 10003}, // sorted: 3000 before 8080
		{Container: 8080, Host: 10005},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExposedPairs = %v, want %v", got, want)
	}
}

func TestExposedPairs_Empty(t *testing.T) {
	if got := ExposedPairs(nil); got != nil {
		t.Fatalf("ExposedPairs(nil) = %v, want nil", got)
	}
}
