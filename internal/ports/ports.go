// Package ports allocates loopback TCP ports for SSH and exposed container ports.
package ports

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

const Version = 1

const (
	PurposeSSH    = "ssh"
	ExposePrefix  = "expose-" // followed by container port, e.g. "expose-3000"
	defaultMin    = 10000
	defaultMax    = 10999
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
