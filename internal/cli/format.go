package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/ports"
)

// formatExposed renders the worktree's expose-/auto- allocations as a
// comma-separated list of `<container>->127.0.0.1:<host>` entries, sorted by
// container port. Returns "-" when there are no exposes.
func formatExposed(allocs []ports.Allocation) string {
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

// formatForwards renders a container's ahjo-forward-* proxy devices as a
// comma-separated list of `:<container><-:<host>` entries, sorted by container
// port. The `<-` arrow (inbound) distinguishes these from formatExposed's `->`
// (outbound). Returns "-" when there are no forwards. Sourced from live Incus
// device config because forwards aren't tracked in ports.json.
func formatForwards(devices []incus.ProxyDevice) string {
	type row struct{ cport, hport int }
	var rows []row
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
		rows = append(rows, row{cport: cport, hport: hport})
	}
	if len(rows) == 0 {
		return "-"
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cport < rows[j].cport })
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf(":%d<-:%d", r.cport, r.hport))
	}
	return strings.Join(parts, ",")
}
