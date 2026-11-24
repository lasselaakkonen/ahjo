package initflow

import (
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
)

// DefaultGatewaysIPv4 returns the IPv4 gateways of every default route on this
// host, sorted and deduplicated, from `ip -4 -o route show default`. A default
// route with no gateway (bound straight to a device) contributes nothing, so
// an empty result is not an error.
//
// Inside a Lima VM every default gateway is the Mac (the vzNAT gateway, and
// host.lima.internal on the user-mode network), which is why the egress
// firewall hard-blocks them there. On a native Linux host the default gateway
// is the user's router, which is theirs to opt in.
func DefaultGatewaysIPv4() ([]netip.Addr, error) {
	out, err := exec.Command("ip", "-4", "-o", "route", "show", "default").Output()
	if err != nil {
		return nil, fmt.Errorf("ip -4 -o route show default: %w", err)
	}
	return parseDefaultGateways(string(out)), nil
}

// parseDefaultGateways is the pure parse behind DefaultGatewaysIPv4, split out
// so tests can feed sample `ip` output. It reads every "via <addr>" on a line
// that starts with "default", which also covers multipath routes (one
// "nexthop via ..." per path on the same -o line). Gateways that are not IPv4
// ("via inet6 fe80::1") and lines of other route types ("unreachable default")
// are skipped.
func parseDefaultGateways(out string) []netip.Addr {
	var gws []netip.Addr
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] != "via" {
				continue
			}
			tok := fields[i+1]
			if (tok == "inet" || tok == "inet6") && i+2 < len(fields) {
				tok = fields[i+2]
			}
			a, err := netip.ParseAddr(tok)
			if err != nil {
				continue
			}
			if a = a.Unmap(); a.Is4() {
				gws = append(gws, a)
			}
		}
	}
	slices.SortFunc(gws, netip.Addr.Compare)
	return slices.Compact(gws)
}
