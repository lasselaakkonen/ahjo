package initflow

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// candidateSubnets is the ordered list of /24s ahjo tries for incusbr0.
// First entry is the historical default so the common (non-nested) case is
// unchanged. Remaining entries cover the nested ahjo-in-ahjo case where the
// outer Lima VM already owns 10.20.30.0/24, plus a couple of fallbacks for
// the rare case where the host runs other Incus or libvirt bridges that
// also stake out these private /24s.
var candidateSubnets = []string{
	"10.20.30.0/24",
	"10.30.40.0/24",
	"10.40.50.0/24",
	"10.50.60.0/24",
	"10.60.70.0/24",
}

// PickGatewayCIDR returns the first candidate /24 that is not already
// on-link on this host, formatted as "10.x.y.1/24" (the gateway address +
// prefix length, which is what `incus admin init`'s preseed expects under
// networks[].config[ipv4.address]).
//
// "Already on-link" is detected by parsing `ip -4 -o route show` and looking
// for any route whose destination network overlaps the candidate. That
// catches both routes the kernel auto-installs for assigned interface
// addresses (proto kernel, scope link) and any operator-added link-scoped
// routes — both signal "this subnet is already directly attached somewhere",
// which is exactly the precondition that makes nested incusbr0 hijack the
// gateway IP.
//
// reason is a short human-readable explanation suitable for the init log
// (e.g. "10.20.30.0/24 already on-link via eth0"). When the first candidate
// is free, reason is "no on-link collisions".
func PickGatewayCIDR() (cidr, reason string, err error) {
	routes, err := onLinkRoutes()
	if err != nil {
		return "", "", err
	}
	for _, cand := range candidateSubnets {
		_, candNet, perr := net.ParseCIDR(cand)
		if perr != nil {
			return "", "", fmt.Errorf("parse candidate %q: %w", cand, perr)
		}
		if collide, via := firstCollision(candNet, routes); collide {
			if cand == candidateSubnets[0] {
				// Note the first collision so the operator sees why we
				// strayed from the historical default.
				reason = fmt.Sprintf("%s already on-link via %s", cand, via)
			}
			continue
		}
		if reason == "" {
			reason = "no on-link collisions"
		}
		return gatewayCIDR(candNet), reason, nil
	}
	return "", "", fmt.Errorf("all candidate /24s collide with on-link routes; "+
		"delete one of these or change ahjo's candidate list: %v", candidateSubnets)
}

// onLinkRoute is one parsed line from `ip -4 -o route show`. We keep only
// the bits firstCollision needs: the destination network and the device the
// route is bound to (for the log message).
type onLinkRoute struct {
	dest *net.IPNet
	dev  string
}

// onLinkRoutes shells out to `ip -4 -o route show` and parses each line.
// We deliberately don't add a netlink dep — the parse is a half-screen of
// strings code, and shipping a syscall-level dep just for the init step
// would be heavier than the problem. Routes whose destination doesn't parse
// as a CIDR (e.g. "default", "unreachable …") are skipped — they can't
// collide with a /24 candidate.
func onLinkRoutes() ([]onLinkRoute, error) {
	out, err := exec.Command("ip", "-4", "-o", "route", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("ip -4 -o route show: %w", err)
	}
	var routes []onLinkRoute
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		_, dest, perr := net.ParseCIDR(fields[0])
		if perr != nil {
			// "default", "unreachable", IP literals, etc. — not a /24 we care about.
			continue
		}
		dev := ""
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "dev" {
				dev = fields[i+1]
				break
			}
		}
		routes = append(routes, onLinkRoute{dest: dest, dev: dev})
	}
	return routes, nil
}

// firstCollision reports whether candNet overlaps any of the on-link routes,
// returning the device of the colliding route for the log message. Overlap
// is checked both ways: a /16 route covers any /24 inside it, and a /24
// route is itself the collision when candNet matches.
func firstCollision(candNet *net.IPNet, routes []onLinkRoute) (bool, string) {
	for _, r := range routes {
		if r.dest.Contains(candNet.IP) || candNet.Contains(r.dest.IP) {
			return true, r.dev
		}
	}
	return false, ""
}

// gatewayCIDR turns a *net.IPNet for a /24 (e.g. 10.30.40.0/24) into the
// gateway-with-prefix form Incus's preseed wants for ipv4.address
// (e.g. "10.30.40.1/24"). Assumes a /24 — callers pull from candidateSubnets.
func gatewayCIDR(n *net.IPNet) string {
	ip := n.IP.To4()
	if ip == nil {
		return n.String()
	}
	ones, _ := n.Mask.Size()
	return fmt.Sprintf("%d.%d.%d.1/%d", ip[0], ip[1], ip[2], ones)
}
