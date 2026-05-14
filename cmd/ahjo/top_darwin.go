//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/lasselaakkonen/ahjo/internal/ide"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

// runMacTop runs the `ahjo top` TUI on the Mac. The UI layer is shared with
// the in-VM build; the platform difference is entirely in Deps. The Mac
// Deps reaches the source of truth (registry, container probes, ports,
// mirror state) by JSON-RPC'ing the hidden `ahjo top-state` /
// `ahjo branch-status` / `ahjo top-toggle-expose` subcommands via
// `limactl shell ahjo`. IDE detection and launching run Mac-side directly
// so Cursor.app / Visual Studio Code.app / etc. show up in the picker.
func runMacTop() error {
	if err := preflightLima(); err != nil {
		return err
	}
	deps := top.Deps{
		ResolveContainerName: macResolveContainerName,
		FormatExposed:        macFormatExposed,
		HostStatus:           macHostStatusForTop,
		ToggleExpose:         macToggleExpose,
		IDEs:                 macIDEs,
		LoadSnapshot:         macLoadSnapshot,
		LoadBranchStatus:     macLoadBranchStatus,
	}
	_, err := tea.NewProgram(top.New(deps)).Run()
	return err
}

// macResolveContainerName mirrors the in-VM resolver: the IncusName is
// authoritative in the registry, and the TUI uses this for display only
// (the actual incus exec happens in-VM).
func macResolveContainerName(br *registry.Branch) (string, error) {
	if br.IncusName != "" {
		return br.IncusName, nil
	}
	alias := br.Slug
	if len(br.Aliases) > 0 {
		alias = br.Aliases[0]
	}
	return "", fmt.Errorf("registry row for %q (slug %q) has no incus_name; recreate with `ahjo rm %s && ahjo create`", alias, br.Slug, alias)
}

// macFormatExposed renders a branch's expose-/auto- allocations as the
// same `:cport->127.0.0.1:hport` comma-list the in-VM TUI prints. Lives
// here as a copy rather than reaching into internal/cli so the Mac shim
// doesn't pull in the cobra+incus subtree.
func macFormatExposed(allocs []ports.Allocation) string {
	type row struct{ cport, hport int }
	var rows []row
	for _, a := range allocs {
		var prefix string
		switch {
		case strings.HasPrefix(a.Purpose, ports.AutoExposePrefix):
			prefix = ports.AutoExposePrefix
		case strings.HasPrefix(a.Purpose, ports.ExposePrefix):
			prefix = ports.ExposePrefix
		default:
			continue
		}
		var cport int
		if _, err := fmt.Sscanf(strings.TrimPrefix(a.Purpose, prefix), "%d", &cport); err != nil {
			continue
		}
		rows = append(rows, row{cport: cport, hport: a.Port})
	}
	if len(rows) == 0 {
		return "-"
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cport < rows[j].cport })
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf(":%d->127.0.0.1:%d", r.cport, r.hport))
	}
	return strings.Join(parts, ",")
}

// macHostStatusForTop probes Lima for the ahjo VM state. Best-effort —
// surfaced as a single line in the right pane when nothing is selected.
func macHostStatusForTop() top.HostStatus {
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}}\t{{.Status}}\t{{.SSHLocalPort}}").Output()
	if err != nil {
		return top.HostStatus{Title: "lima", Lines: []string{"limactl unavailable: " + err.Error()}}
	}
	var lines []string
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		fields := strings.Split(raw, "\t")
		switch len(fields) {
		case 3:
			lines = append(lines, fields[0]+"  "+fields[1]+"  ssh:"+fields[2])
		case 2:
			lines = append(lines, fields[0]+"  "+fields[1])
		default:
			lines = append(lines, raw)
		}
	}
	if len(lines) == 0 {
		lines = []string{"no Lima VMs"}
	}
	return top.HostStatus{Title: "lima", Lines: lines}
}

// macIDEs probes the Mac's /Applications + ~/Applications for installed
// IDE bundles and wraps each one in a launcher that runs Mac-side via
// `open` (vscode-remote://ssh-remote+... or Zed's ssh:// URL). The launch
// happens directly in this process — no detour through the paste daemon.
func macIDEs() []top.IDE {
	slugs := ide.DetectInstalled()
	out := make([]top.IDE, 0, len(slugs))
	for _, slug := range slugs {
		slug := slug
		out = append(out, top.IDE{
			Name: ide.DisplayName(slug),
			Open: func(host, path string) error {
				return ide.LaunchOnHost(slug, host, path)
			},
		})
	}
	return out
}

// macLoadSnapshot fetches a Snapshot by shelling `ahjo top-state` into the
// VM and JSON-decoding the result. Warm `limactl shell` via the SSH
// ControlMaster is ~50ms; the TUI ticks every 2s so the latency is
// invisible during normal use.
func macLoadSnapshot() (top.Snapshot, error) {
	var snap top.Snapshot
	out, stderr, err := runLima("ahjo", "top-state")
	if err != nil {
		return snap, fmt.Errorf("top-state: %w (%s)", err, strings.TrimSpace(stderr))
	}
	if err := json.Unmarshal(out, &snap); err != nil {
		return snap, fmt.Errorf("decode top-state: %w", err)
	}
	return snap, nil
}

// macLoadBranchStatus fetches a BranchStatus by shelling
// `ahjo branch-status <slug>` into the VM. The git+gh subprocesses inside
// the container can take a few hundred ms; the TUI rate-limits per-branch
// fetches via branchStatusStaleness so holding arrow keys can't fan out.
func macLoadBranchStatus(slug string) (top.BranchStatus, error) {
	var bs top.BranchStatus
	out, stderr, err := runLima("ahjo", "branch-status", slug)
	if err != nil {
		return bs, fmt.Errorf("branch-status %s: %w (%s)", slug, err, strings.TrimSpace(stderr))
	}
	if err := json.Unmarshal(out, &bs); err != nil {
		return bs, fmt.Errorf("decode branch-status: %w", err)
	}
	return bs, nil
}

// macToggleExpose shells `ahjo top-toggle-expose <slug>` into the VM and
// returns its stdout as the flash-line status. Toggle semantics live in
// the VM (it owns the incus state); this is a thin RPC.
func macToggleExpose(br *registry.Branch) (string, error) {
	out, stderr, err := runLima("ahjo", "top-toggle-expose", br.Slug)
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// runLima runs `limactl shell <vmName> <argv...>` and returns its stdout,
// stderr, and the exec error. Lives here rather than in lima.Cmd-callers'
// inline because three places want the same {stdout, stderr, err} shape.
func runLima(argv ...string) ([]byte, string, error) {
	full := append([]string{"shell", vmName}, argv...)
	cmd := lima.Cmd(full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}
