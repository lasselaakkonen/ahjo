package initflow

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// fixedBlockedPrefixes are the IPv4 ranges containers on incusbr0 must not
// reach. Together they cover the Mac (host.lima.internal is 192.168.5.2), the
// Lima VM and bridge gateway (10.x), the LAN, VPN and Tailscale (CGNAT) ranges,
// cloud metadata (169.254.169.254), and address space that is never a
// legitimate internet destination. Everything else is public internet, which
// stays reachable so package managers and git keep working.
var fixedBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/3"),
}

// minOptInBits is the widest opt-in prefix ParseOptIn accepts. Anything wider
// than a /8 would re-open most of the blocked set in one line, which is almost
// certainly a typo rather than intent.
const minOptInBits = 8

// BlockedPrefixes returns the fixed blocked ranges plus a /32 for every IPv4
// host address. The host addresses matter on a native Linux host with a public
// IP: that address is outside every private range, yet dialing it reaches the
// host's own listeners. IPv6 addresses are ignored because the bridge has no
// IPv6 and the ACL default rejects anything not explicitly allowed.
//
// The result is sorted and deduplicated so the rendered ACL, and the hash the
// caller stores of it, is stable across runs.
func BlockedPrefixes(hostAddrs []netip.Addr) []netip.Prefix {
	out := slices.Clone(fixedBlockedPrefixes)
	for _, a := range hostAddrs {
		a = a.Unmap()
		if !a.Is4() {
			continue
		}
		out = append(out, netip.PrefixFrom(a, 32))
	}
	slices.SortFunc(out, comparePrefix)
	return slices.Compact(out)
}

// AllowedEgress returns the IPv4 prefixes containers may reach: everything in
// 0.0.0.0/0 that is in no blocked prefix, plus everything in an opt-in prefix.
// Opt-ins win over blocks, which is the whole point of them (a corporate
// 10.1.0.0/16 inside the blocked 10/8).
//
// We need an explicit allow-list because Incus evaluates drop and reject rules
// before allow rules regardless of order, so "reject 10/8 but allow 10.1/16"
// cannot be written as rules. Instead the ACL default rejects, and this list
// enumerates what is allowed.
//
// The computation walks the address space from 0.0.0.0/0, splitting a prefix
// in halves while classify can't decide it, and emits each prefix at the
// widest level where it is fully allowed. Walking low half first keeps the
// output sorted and non-overlapping. classify can call a prefix undecided even
// when it turns out fully allowed (blocked 10/8 with opt-in 10/8 makes 10/7
// look mixed), so a parent whose two halves both come back fully allowed is
// emitted as itself; that keeps the output minimal, with no two sibling
// prefixes that could merge. Inputs may overlap or repeat; non-IPv4 inputs
// are dropped.
func AllowedEgress(blocked, optIn []netip.Prefix) []netip.Prefix {
	b := normalizeIPv4(blocked)
	o := normalizeIPv4(optIn)
	var walk func(p netip.Prefix) (allowed []netip.Prefix, full bool)
	walk = func(p netip.Prefix) ([]netip.Prefix, bool) {
		switch classify(p, b, o) {
		case prefixAllowed:
			return []netip.Prefix{p}, true
		case prefixBlocked:
			return nil, false
		}
		lo, hi := splitPrefix(p)
		loOut, loFull := walk(lo)
		hiOut, hiFull := walk(hi)
		if loFull && hiFull {
			return []netip.Prefix{p}, true
		}
		return append(loOut, hiOut...), false
	}
	out, _ := walk(netip.MustParsePrefix("0.0.0.0/0"))
	return out
}

type prefixState int

const (
	prefixAllowed prefixState = iota
	prefixBlocked
	prefixMixed
)

// classify decides whether every address in p is allowed, every address is
// blocked, or it needs splitting. A /32 can never be mixed: overlap with a
// prefix is the same as containment at that size, so the recursion stops.
func classify(p netip.Prefix, blocked, optIn []netip.Prefix) prefixState {
	optInOverlaps := false
	for _, q := range optIn {
		if containsPrefix(q, p) {
			return prefixAllowed
		}
		if q.Overlaps(p) {
			optInOverlaps = true
		}
	}
	blockedOverlaps := false
	for _, q := range blocked {
		if containsPrefix(q, p) && !optInOverlaps {
			return prefixBlocked
		}
		if q.Overlaps(p) {
			blockedOverlaps = true
		}
	}
	if !blockedOverlaps {
		return prefixAllowed
	}
	return prefixMixed
}

// containsPrefix reports whether outer covers every address of inner.
func containsPrefix(outer, inner netip.Prefix) bool {
	return outer.Bits() <= inner.Bits() && outer.Contains(inner.Addr())
}

// splitPrefix returns the two halves of an IPv4 prefix one bit longer. The
// caller never passes a /32 because classify never reports one as mixed.
func splitPrefix(p netip.Prefix) (lo, hi netip.Prefix) {
	bits := p.Bits() + 1
	base := p.Addr().As4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	n |= 1 << (32 - bits)
	hiAddr := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	return netip.PrefixFrom(p.Addr(), bits), netip.PrefixFrom(hiAddr, bits)
}

// normalizeIPv4 masks each prefix to its network address and keeps IPv4 only,
// so containment checks compare like with like.
func normalizeIPv4(ps []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ps))
	for _, p := range ps {
		if !p.IsValid() {
			continue
		}
		a := p.Addr()
		bits := p.Bits()
		if a.Is4In6() {
			if bits < 96 {
				continue
			}
			a = a.Unmap()
			bits -= 96
		}
		if !a.Is4() {
			continue
		}
		out = append(out, netip.PrefixFrom(a, bits).Masked())
	}
	return out
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

// ParseOptIn parses the user's opt-in CIDRs (config `network.allow_cidrs`).
// Config validation and the init/update step share it so a value that loads
// is a value the step can apply. Each entry must be an IPv4 prefix in CIDR
// form; it is masked to its network address so "10.1.2.3/16" means
// 10.1.0.0/16 rather than being rejected for a detail the user didn't intend.
func ParseOptIn(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		s = strings.TrimSpace(s)
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("network.allow_cidrs: %q is not a CIDR prefix (want e.g. 10.1.0.0/16): %w", s, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("network.allow_cidrs: %q is not IPv4; incusbr0 has no IPv6, so only IPv4 prefixes apply", s)
		}
		if p.Bits() < minOptInBits {
			return nil, fmt.Errorf("network.allow_cidrs: %q is wider than /%d; an opt-in that wide re-opens most private ranges and defeats the egress firewall", s, minOptInBits)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// EgressACLYAML renders the body fed to `incus network acl edit <name>` on
// stdin: one egress allow rule per prefix, no ingress rules (the network's
// ingress default handles that), and a description marking it as ahjo's.
//
// There is no name key, since the name comes from the command line, and no
// config key. The caller owns config (it stores the rules hash in a user.*
// key). Note that `acl edit` replaces the whole object, so a caller that keeps
// config keys must append its own `config:` block to this body; otherwise
// those keys are cleared.
func EgressACLYAML(allowed []netip.Prefix) string {
	var b strings.Builder
	b.WriteString("description: \"Managed by ahjo (incusbr0 egress allow-list); ahjo update rewrites it\"\n")
	if len(allowed) == 0 {
		b.WriteString("egress: []\n")
	} else {
		b.WriteString("egress:\n")
		for _, p := range allowed {
			fmt.Fprintf(&b, "- action: allow\n  destination: %s\n  state: enabled\n", p)
		}
	}
	b.WriteString("ingress: []\n")
	return b.String()
}

// HostIPv4Addrs lists the IPv4 addresses on this host's interfaces, for
// BlockedPrefixes. It is a snapshot: addresses gained after init/update are
// not blocked until the next update.
func HostIPv4Addrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list interface addresses: %w", err)
	}
	var out []netip.Addr
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipn.IP.To4()
		if ip4 == nil {
			continue
		}
		if addr, ok := netip.AddrFromSlice(ip4); ok {
			out = append(out, addr)
		}
	}
	return out, nil
}
