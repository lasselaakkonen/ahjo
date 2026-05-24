// Package ports allocates loopback TCP ports for SSH and exposed container ports.
package ports

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const Version = 1

const (
	PurposeSSH       = "ssh"
	ExposePrefix     = "expose-" // followed by container port, e.g. "expose-3000"
	AutoExposePrefix = "auto-"   // followed by container port, e.g. "auto-3000"
	defaultMin       = 10000
	defaultMax       = 10999
)

type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type Allocation struct {
	Slug    string `json:"slug"`
	Port    int    `json:"port"`
	Purpose string `json:"purpose"`
}

// PortPair is a container-port → host-port mapping. It backs both the
// `ahjo ls` expose column and the `top` details pane, and is reused for
// forwards (where Container is the in-container listen port and Host the
// loopback port it proxies to). Plain ints, no styling — display formatting
// lives at the call site.
type PortPair struct {
	Container int
	Host      int
}

// ExposedPairs extracts the (container, host) mappings from a slug's
// allocations, keeping only expose/auto-expose purposes (ssh and unknowns are
// skipped) and sorting by container port. Both the ls table and the TUI
// details pane derive their expose rendering from this so the parsing lives in
// exactly one place.
func ExposedPairs(allocs []Allocation) []PortPair {
	var out []PortPair
	for _, a := range allocs {
		var prefix string
		switch {
		case strings.HasPrefix(a.Purpose, AutoExposePrefix):
			prefix = AutoExposePrefix
		case strings.HasPrefix(a.Purpose, ExposePrefix):
			prefix = ExposePrefix
		default:
			continue
		}
		var cport int
		if _, err := fmt.Sscanf(strings.TrimPrefix(a.Purpose, prefix), "%d", &cport); err != nil {
			continue
		}
		out = append(out, PortPair{Container: cport, Host: a.Port})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out
}

type Ports struct {
	Version     int          `json:"version"`
	Range       Range        `json:"range"`
	Allocations []Allocation `json:"allocations"`
}

func defaultPorts() *Ports {
	return &Ports{Version: Version, Range: Range{Min: defaultMin, Max: defaultMax}}
}

func Load() (*Ports, error) {
	b, err := os.ReadFile(paths.PortsPath())
	if errors.Is(err, os.ErrNotExist) {
		return defaultPorts(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ports: %w", err)
	}
	var p Ports
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse ports: %w", err)
	}
	if p.Version == 0 {
		p.Version = Version
	}
	if p.Version != Version {
		return nil, fmt.Errorf("ports version %d unsupported (binary expects %d)", p.Version, Version)
	}
	if p.Range.Min == 0 && p.Range.Max == 0 {
		p.Range = Range{Min: defaultMin, Max: defaultMax}
	}
	return &p, nil
}

func (p *Ports) Save() error {
	if err := paths.EnsureSkeleton(); err != nil {
		return err
	}
	p.Version = Version
	tmp, err := os.CreateTemp(paths.AhjoDir(), "ports-*.json.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		tmp.Close()
		return fmt.Errorf("encode ports: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), paths.PortsPath())
}

// Find returns the existing allocation for (slug, purpose), or nil.
func (p *Ports) Find(slug, purpose string) *Allocation {
	for i := range p.Allocations {
		if p.Allocations[i].Slug == slug && p.Allocations[i].Purpose == purpose {
			return &p.Allocations[i]
		}
	}
	return nil
}

// Allocate returns the existing allocation for (slug, purpose) or assigns the
// next free port in the configured range.
func (p *Ports) Allocate(slug, purpose string) (int, error) {
	if a := p.Find(slug, purpose); a != nil {
		return a.Port, nil
	}
	used := make(map[int]struct{}, len(p.Allocations))
	for _, a := range p.Allocations {
		used[a.Port] = struct{}{}
	}
	for port := p.Range.Min; port <= p.Range.Max; port++ {
		if _, taken := used[port]; taken {
			continue
		}
		p.Allocations = append(p.Allocations, Allocation{Slug: slug, Port: port, Purpose: purpose})
		return port, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d", p.Range.Min, p.Range.Max)
}

// FreeSlug removes every allocation for slug.
func (p *Ports) FreeSlug(slug string) {
	out := p.Allocations[:0]
	for _, a := range p.Allocations {
		if a.Slug == slug {
			continue
		}
		out = append(out, a)
	}
	p.Allocations = out
}

// FreePurpose removes the single allocation matching (slug, purpose). Used by
// auto-expose to release a port when its container-side listener disappears.
func (p *Ports) FreePurpose(slug, purpose string) {
	out := p.Allocations[:0]
	for _, a := range p.Allocations {
		if a.Slug == slug && a.Purpose == purpose {
			continue
		}
		out = append(out, a)
	}
	p.Allocations = out
}

// AllocationsForSlug returns a copy of every allocation belonging to slug.
// Used by `ahjo ls` to render exposed-ports per worktree.
func (p *Ports) AllocationsForSlug(slug string) []Allocation {
	var out []Allocation
	for _, a := range p.Allocations {
		if a.Slug == slug {
			out = append(out, a)
		}
	}
	return out
}
