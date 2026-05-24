package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/ports"
)

// formatExposed renders the worktree's expose-/auto- allocations as a
// comma-separated list of `:<container>->127.0.0.1:<host>` entries, sorted by
// container port. Returns "-" when there are no exposes. Used by `ahjo ls`;
// the `top` details pane formats ports.ExposedPairs itself.
func formatExposed(allocs []ports.Allocation) string {
	pairs := ports.ExposedPairs(allocs)
	if len(pairs) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf(":%d->127.0.0.1:%d", p.Container, p.Host))
	}
	return strings.Join(parts, ",")
}

// formatForwards renders a container's ahjo-forward-* proxy devices as a
// comma-separated list of `:<container><-:<host>` entries, sorted by container
// port. The `<-` arrow (inbound) distinguishes these from formatExposed's `->`
// (outbound). Returns "-" when there are no forwards. Used by `ahjo ls`.
func formatForwards(devices []incus.ProxyDevice) string {
	pairs := forwardPairs(devices)
	if len(pairs) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf(":%d<-:%d", p.Container, p.Host))
	}
	return strings.Join(parts, ",")
}

// forwardPairs extracts the (container-listen, host-connect) mappings from a
// container's ahjo-forward-* proxy devices, sorted by container port. Sourced
// from live Incus device config because forwards aren't tracked in ports.json.
// Both formatForwards (ls) and the in-container ahjo-state snapshot build on it.
func forwardPairs(devices []incus.ProxyDevice) []ports.PortPair {
	var out []ports.PortPair
	for _, d := range devices {
		if !strings.HasPrefix(d.Name, ahjoForwardDevicePrefix) {
			continue
		}
		cport, ok := portFromAddr(d.Listen)
		if !ok {
			continue
		}
		hport, ok := portFromAddr(d.Connect)
		if !ok {
			continue
		}
		out = append(out, ports.PortPair{Container: cport, Host: hport})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out
}
