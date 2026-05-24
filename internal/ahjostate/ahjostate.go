// Package ahjostate renders a container's current ahjo bridge state (mirror /
// expose / forward) into the two snapshots ahjo pushes inside the container,
// both under /home/ubuntu/.ahjo:
//
//   - ahjo-state.md  (RenderMarkdown) — prose Claude reads on demand (AHJO.md
//     points at it rather than embedding it, so it's never stale).
//   - ahjo-state.json (RenderJSON)    — the machine-readable twin the Claude
//     Code statusline parses with jq. Both are rendered from the same State so
//     they cannot drift.
//
// Rendering is pure (only the ports.PortPair value type, no incus/registry
// deps) so it stays unit-testable; the caller in internal/cli gathers the live
// state and hands it here. The state the container itself cannot discover —
// expose proxies live in the host's incus config, forward host ports aren't
// visible inside, mirror's host path is host-only — so the host is the source
// of truth and rewrites these files on every change. See
// internal/cli/ahjostate.go for the gather + push side.
package ahjostate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lasselaakkonen/ahjo/internal/ports"
)

// State is the snapshot RenderMarkdown and RenderJSON turn into the two pushed
// files. Zero value renders as "everything off", which is the correct state for
// a freshly created container. Expose/Forward reuse ports.PortPair (the same
// type ls/top use) so the snapshot can't drift from those columns.
type State struct {
	Slug  string    // container slug, for the header
	Alias string    // branch alias, used in the enable hints (falls back to "<alias>")
	At    time.Time // generation time, rendered as a UTC minute stamp

	MirrorOn         bool
	MirrorRepo       string // in-container path that is mirrored (always /repo today)
	MirrorHostTarget string // host dir it mirrors to; empty when unknown

	Expose  []ports.PortPair // container port -> host 127.0.0.1:port
	Forward []ports.PortPair // host port (Host) -> container 127.0.0.1:port (Container)
}

// RenderMarkdown produces the ahjo-state.md markdown for s — the prose snapshot
// Claude reads on demand (AHJO.md points at it rather than embedding it).
func RenderMarkdown(s State) string {
	var b strings.Builder
	b.WriteString("# ahjo-state\n")
	fmt.Fprintf(&b, "_updated %s · slug %s_\n\n", s.At.UTC().Format("2006-01-02T15:04Z"), orDash(s.Slug))

	renderMirror(&b, s)
	renderExpose(&b, s)
	renderForward(&b, s)
	return b.String()
}

// stateJSON is the wire shape of ahjo-state.json. The three bridges are grouped
// into their own objects so the statusline's jq reads naturally — .mirror.on,
// (.expose | length), .forward[].host — rather than scraping the markdown.
type stateJSON struct {
	Slug      string           `json:"slug"`
	Alias     string           `json:"alias"`
	UpdatedAt string           `json:"updated_at"`
	Mirror    mirrorJSON       `json:"mirror"`
	Expose    []ports.PortPair `json:"expose"`  // container :port -> host 127.0.0.1:port
	Forward   []ports.PortPair `json:"forward"` // host 127.0.0.1:port -> container :port
}

type mirrorJSON struct {
	On         bool   `json:"on"`
	Repo       string `json:"repo"`        // in-container path mirrored out (always /repo today)
	HostTarget string `json:"host_target"` // host dir it mirrors to; "" when unknown
}

// RenderJSON produces the ahjo-state.json bytes for s — the machine-readable
// twin of RenderMarkdown, consumed by the statusline. Empty bridges serialize
// as [] (not null) so jq access is uniform. Trailing newline for clean files.
func RenderJSON(s State) ([]byte, error) {
	repo := s.MirrorRepo
	if repo == "" {
		repo = "/repo"
	}
	w := stateJSON{
		Slug:      s.Slug,
		Alias:     s.Alias,
		UpdatedAt: s.At.UTC().Format(time.RFC3339),
		Mirror:    mirrorJSON{On: s.MirrorOn, Repo: repo, HostTarget: s.MirrorHostTarget},
		Expose:    nonNil(s.Expose),
		Forward:   nonNil(s.Forward),
	}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func nonNil(p []ports.PortPair) []ports.PortPair {
	if p == nil {
		return []ports.PortPair{}
	}
	return p
}

func renderMirror(b *strings.Builder, s State) {
	if !s.MirrorOn {
		fmt.Fprintf(b, "- **mirror**: OFF — enable from host: `ahjo mirror %s --target <dir>`\n", s.alias())
		return
	}
	repo := s.MirrorRepo
	if repo == "" {
		repo = "/repo"
	}
	if s.MirrorHostTarget != "" {
		fmt.Fprintf(b, "- **mirror**: ON — `%s` → host `%s`\n", repo, s.MirrorHostTarget)
	} else {
		fmt.Fprintf(b, "- **mirror**: ON — `%s` → host\n", repo)
	}
	b.WriteString("  (create/modify only; deletions not replicated)\n")
}

func renderExpose(b *strings.Builder, s State) {
	if len(s.Expose) == 0 {
		fmt.Fprintf(b, "- **expose**: OFF — enable from host: `ahjo expose %s <container-port>`\n", s.alias())
		return
	}
	b.WriteString("- **expose**: ON\n")
	b.WriteString("  | container | host |\n")
	b.WriteString("  |---|---|\n")
	for _, p := range s.Expose {
		fmt.Fprintf(b, "  | :%d | 127.0.0.1:%d |\n", p.Container, p.Host)
	}
}

func renderForward(b *strings.Builder, s State) {
	if len(s.Forward) == 0 {
		fmt.Fprintf(b, "- **forward**: OFF — enable from host: `ahjo forward %s <host-port>`\n", s.alias())
		return
	}
	b.WriteString("- **forward**: ON\n")
	b.WriteString("  | host | container |\n")
	b.WriteString("  |---|---|\n")
	for _, p := range s.Forward {
		fmt.Fprintf(b, "  | 127.0.0.1:%d | :%d |\n", p.Host, p.Container)
	}
}

func (s State) alias() string {
	if s.Alias != "" {
		return s.Alias
	}
	return "<alias>"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
