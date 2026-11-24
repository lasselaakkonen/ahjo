package initflow

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", c, err)
		}
		out = append(out, p)
	}
	return out
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func addrU32(a netip.Addr) uint64 {
	b := a.As4()
	return uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
}

func u32Addr(n uint64) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}

// span is a half-open [lo, hi) range of IPv4 addresses as integers. The test
// uses interval arithmetic as an independent oracle for the prefix walk.
type span struct{ lo, hi uint64 }

func prefixSpan(p netip.Prefix) span {
	p = p.Masked()
	lo := addrU32(p.Addr())
	return span{lo, lo + 1<<(32-p.Bits())}
}

func mergeSpans(ps []netip.Prefix) []span {
	var ss []span
	for _, p := range ps {
		ss = append(ss, prefixSpan(p))
	}
	slices.SortFunc(ss, func(a, b span) int { return int(a.lo) - int(b.lo) })
	var out []span
	for _, s := range ss {
		if n := len(out); n > 0 && s.lo <= out[n-1].hi {
			out[n-1].hi = max(out[n-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	return out
}

// subtractSpans returns the parts of the merged spans a not in the merged
// spans b.
func subtractSpans(a, b []span) []span {
	var out []span
	for _, s := range a {
		cur := s.lo
		for _, o := range b {
			if o.hi <= cur || o.lo >= s.hi {
				continue
			}
			if o.lo > cur {
				out = append(out, span{cur, o.lo})
			}
			cur = max(cur, o.hi)
		}
		if cur < s.hi {
			out = append(out, span{cur, s.hi})
		}
	}
	return out
}

// effectiveBlockedCount is |(blocked \ optIn) ∪ hard|, computed by intervals.
func effectiveBlockedCount(blocked, optIn, hard []netip.Prefix) uint64 {
	soft := subtractSpans(mergeSpans(blocked), mergeSpans(optIn))
	hs := mergeSpans(hard)
	var total uint64
	for _, h := range hs {
		total += h.hi - h.lo
	}
	for _, r := range subtractSpans(soft, hs) {
		total += r.hi - r.lo
	}
	return total
}

func addrs(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		out = append(out, a)
	}
	return out
}

func TestAllowedEgressMembership(t *testing.T) {
	hard := HardBlockedPrefixes(addrs(t, "203.0.113.7"), netip.MustParsePrefix("10.20.30.0/24"))
	allowed := AllowedEgress(BlockedPrefixes(), nil, hard)

	var outside []string
	for _, p := range fixedBlockedPrefixes {
		s := prefixSpan(p)
		outside = append(outside, u32Addr(s.lo).String(), u32Addr(s.hi-1).String())
	}
	outside = append(outside,
		"192.168.5.2",     // host.lima.internal, the Mac
		"169.254.169.254", // cloud metadata
		"10.20.30.1",      // default incusbr0 gateway
		"203.0.113.7",     // host interface address passed in
	)
	inside := []string{
		"1.1.1.1", "140.82.112.3", "8.8.8.8",
		"172.15.255.255", "172.32.0.0",
		"192.167.255.255", "192.169.0.0",
		"100.63.255.255", "100.128.0.0",
		"203.0.113.6", "203.0.113.8",
	}

	tests := []struct {
		addr string
		want bool
	}{}
	for _, a := range outside {
		tests = append(tests, struct {
			addr string
			want bool
		}{a, false})
	}
	for _, a := range inside {
		tests = append(tests, struct {
			addr string
			want bool
		}{a, true})
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := inAny(allowed, netip.MustParseAddr(tt.addr)); got != tt.want {
				t.Fatalf("allowed(%s) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestAllowedEgressOptIn(t *testing.T) {
	tests := []struct {
		name    string
		optIn   []string
		hard    []string
		allowed []string
		blocked []string
	}{
		{
			name:    "opt-in /16 inside 10/8",
			optIn:   []string{"10.1.0.0/16"},
			hard:    []string{"203.0.113.7/32", "10.20.30.0/24"},
			allowed: []string{"10.1.2.3", "10.1.0.0", "10.1.255.255"},
			blocked: []string{"10.0.0.1", "10.2.0.1", "10.0.255.255", "10.20.30.1"},
		},
		{
			name:    "opt-in covering a public host address keeps the host blocked",
			optIn:   []string{"203.0.113.0/24"},
			hard:    []string{"203.0.113.7/32"},
			allowed: []string{"203.0.113.6", "203.0.113.8", "203.0.113.0", "203.0.113.255"},
			blocked: []string{"203.0.113.7", "192.168.5.2"},
		},
		{
			name:    "opt-in /32 for one LAN host",
			optIn:   []string{"192.168.1.10/32"},
			hard:    []string{"203.0.113.7/32"},
			allowed: []string{"192.168.1.10"},
			blocked: []string{"192.168.1.9", "192.168.1.11", "192.168.5.2"},
		},
		{
			// The corporate-VPN case: 10/8 must not open the incusbr0
			// gateway (every host listener) or a sibling container.
			name:    "opt-in 10/8 keeps the bridge subnet blocked",
			optIn:   []string{"10.0.0.0/8"},
			hard:    []string{"10.20.30.0/24"},
			allowed: []string{"10.1.2.3", "10.20.31.1", "10.0.0.1", "10.20.29.255", "10.255.255.255"},
			blocked: []string{"10.20.30.1", "10.20.30.57", "10.20.30.0", "10.20.30.255"},
		},
		{
			// Inside a Lima VM: the Mac and the VM's own address stay
			// blocked under a whole-192.168 opt-in.
			name:    "opt-in 192.168/16 keeps the Mac and the VM blocked",
			optIn:   []string{"192.168.0.0/16"},
			hard:    []string{"192.168.5.2/32", "192.168.5.15/32"},
			allowed: []string{"192.168.5.3", "192.168.1.10", "192.168.5.1", "192.168.5.14", "192.168.5.16"},
			blocked: []string{"192.168.5.2", "192.168.5.15"},
		},
		{
			name:    "hard block equal to the opt-in",
			optIn:   []string{"10.20.30.0/24"},
			hard:    []string{"10.20.30.0/24"},
			blocked: []string{"10.20.30.0", "10.20.30.1", "10.20.30.255"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			optIn, err := ParseOptIn(tt.optIn)
			if err != nil {
				t.Fatalf("ParseOptIn: %v", err)
			}
			allowed := AllowedEgress(BlockedPrefixes(), optIn, mustPrefixes(t, tt.hard...))
			for _, a := range tt.allowed {
				if !inAny(allowed, netip.MustParseAddr(a)) {
					t.Errorf("%s should be allowed", a)
				}
			}
			for _, a := range tt.blocked {
				if inAny(allowed, netip.MustParseAddr(a)) {
					t.Errorf("%s should be blocked", a)
				}
			}
		})
	}
}

// TestAllowedEgressProperties checks, for a range of inputs including
// overlapping and duplicate ones, that the output is sorted, disjoint,
// minimal, covers exactly 2^32 minus the effectively blocked count, and agrees
// with a direct membership check on random addresses.
func TestAllowedEgressProperties(t *testing.T) {
	type tc struct {
		name    string
		blocked []netip.Prefix
		optIn   []netip.Prefix
		hard    []netip.Prefix
	}
	tests := []tc{
		{
			name:    "default with host address",
			blocked: BlockedPrefixes(),
			hard:    HardBlockedPrefixes(addrs(t, "203.0.113.7")),
		},
		{
			name:    "default with opt-in",
			blocked: BlockedPrefixes(),
			optIn:   mustPrefixes(t, "10.1.0.0/16", "192.168.1.10/32"),
		},
		{
			name:    "Lima VM with wide opt-ins",
			blocked: BlockedPrefixes(),
			optIn:   mustPrefixes(t, "10.0.0.0/8", "192.168.0.0/16"),
			hard: HardBlockedPrefixes(addrs(t, "192.168.5.15", "10.20.30.1", "127.0.0.1", "192.168.5.2"),
				netip.MustParsePrefix("10.20.30.1/24")),
		},
		{
			name:    "hard block equal to an opt-in",
			blocked: mustPrefixes(t, "10.0.0.0/8"),
			optIn:   mustPrefixes(t, "10.20.30.0/24"),
			hard:    mustPrefixes(t, "10.20.30.0/24"),
		},
		{
			name:    "hard block wider than the opt-in and outside the blocked set",
			blocked: mustPrefixes(t, "10.0.0.0/8"),
			optIn:   mustPrefixes(t, "10.1.2.0/24"),
			hard:    mustPrefixes(t, "10.1.0.0/16", "8.8.8.8/32", "8.8.8.8/32"),
		},
		{
			name: "hard blocks only",
			hard: mustPrefixes(t, "0.0.0.0/32", "255.255.255.255/32", "128.0.0.0/1"),
		},
		{
			name:    "overlapping and duplicate blocks",
			blocked: mustPrefixes(t, "10.0.0.0/8", "10.0.0.0/9", "10.0.0.0/8", "192.168.0.0/16", "192.168.5.2/32", "11.0.0.0/8"),
		},
		{
			name:    "opt-in equal to a block",
			blocked: mustPrefixes(t, "10.0.0.0/8"),
			optIn:   mustPrefixes(t, "10.0.0.0/8"),
		},
		{
			name:    "overlapping opt-ins",
			blocked: mustPrefixes(t, "10.0.0.0/8"),
			optIn:   mustPrefixes(t, "10.1.0.0/16", "10.1.2.0/24", "10.0.0.0/9"),
		},
		{
			name:    "opt-in outside blocked set",
			blocked: BlockedPrefixes(),
			optIn:   mustPrefixes(t, "8.8.8.0/24"),
		},
		{
			name:    "unmasked inputs",
			blocked: mustPrefixes(t, "10.9.9.9/8"),
			optIn:   mustPrefixes(t, "10.1.2.3/16"),
		},
		{
			name: "nothing blocked",
		},
		{
			name:    "everything blocked",
			blocked: mustPrefixes(t, "0.0.0.0/0"),
		},
		{
			name:    "IPv6 inputs ignored",
			blocked: mustPrefixes(t, "10.0.0.0/8", "fd00::/8"),
			optIn:   mustPrefixes(t, "::/0"),
		},
	}
	rng := rand.New(rand.NewPCG(1, 2))
	// Randomized inputs: prefixes of mixed length clustered in a few /8s so
	// blocks, opt-ins and hard blocks actually overlap.
	randPrefixes := func(n int, minBits, maxBits int) []netip.Prefix {
		bases := []uint32{10 << 24, 192<<24 | 168<<16, 203<<24 | 113<<8, rng.Uint32()}
		out := make([]netip.Prefix, 0, n)
		for range n {
			base := bases[rng.IntN(len(bases))]
			a := u32Addr(uint64(base ^ rng.Uint32()>>uint(8+rng.IntN(24))))
			bits := minBits + rng.IntN(maxBits-minBits+1)
			out = append(out, netip.PrefixFrom(a, bits))
		}
		return out
	}
	for i := range 40 {
		tests = append(tests, tc{
			name:    fmt.Sprintf("random %d", i),
			blocked: slices.Concat(BlockedPrefixes(), randPrefixes(rng.IntN(4), 8, 32)),
			optIn:   randPrefixes(rng.IntN(5), 8, 32),
			hard:    randPrefixes(rng.IntN(6), 12, 32),
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := AllowedEgress(tt.blocked, tt.optIn, tt.hard)
			v4Blocked := normalizeIPv4(tt.blocked)
			v4OptIn := normalizeIPv4(tt.optIn)
			v4Hard := normalizeIPv4(tt.hard)

			var total uint64
			for i, p := range allowed {
				if !p.Addr().Is4() {
					t.Fatalf("non-IPv4 prefix %s in output", p)
				}
				if p != p.Masked() {
					t.Fatalf("prefix %s not masked", p)
				}
				total += 1 << (32 - p.Bits())
				if i == 0 {
					continue
				}
				prev := allowed[i-1]
				if prefixSpan(prev).hi > prefixSpan(p).lo {
					t.Fatalf("prefixes %s and %s overlap or are out of order", prev, p)
				}
				if prev.Bits() == p.Bits() && p.Bits() > 0 {
					pp, _ := prev.Addr().Prefix(prev.Bits() - 1)
					pq, _ := p.Addr().Prefix(p.Bits() - 1)
					if pp == pq {
						t.Fatalf("sibling prefixes %s and %s should have merged", prev, p)
					}
				}
			}
			want := uint64(1)<<32 - effectiveBlockedCount(v4Blocked, v4OptIn, v4Hard)
			if total != want {
				t.Fatalf("allowed address count = %d, want %d", total, want)
			}

			// Random samples plus every input prefix's edges.
			var samples []netip.Addr
			for range 2000 {
				samples = append(samples, u32Addr(uint64(rng.Uint32())))
			}
			for _, p := range slices.Concat(v4Blocked, v4OptIn, v4Hard) {
				s := prefixSpan(p)
				samples = append(samples, u32Addr(s.lo), u32Addr(s.hi-1))
				if s.lo > 0 {
					samples = append(samples, u32Addr(s.lo-1))
				}
				if s.hi < 1<<32 {
					samples = append(samples, u32Addr(s.hi))
				}
			}
			for _, a := range samples {
				want := (!inAny(v4Blocked, a) || inAny(v4OptIn, a)) && !inAny(v4Hard, a)
				if got := inAny(allowed, a); got != want {
					t.Fatalf("allowed(%s) = %v, want %v", a, got, want)
				}
			}
		})
	}
}

func TestAllowedEgressDeterministic(t *testing.T) {
	a := BlockedPrefixes()
	b := slices.Clone(a)
	slices.Reverse(b)
	optA := mustPrefixes(t, "10.1.0.0/16", "192.168.1.0/24")
	optB := slices.Clone(optA)
	slices.Reverse(optB)
	hardA := mustPrefixes(t, "203.0.113.7/32", "10.20.30.0/24", "10.1.2.3/32")
	hardB := slices.Clone(hardA)
	slices.Reverse(hardB)
	if !slices.Equal(AllowedEgress(a, optA, hardA), AllowedEgress(b, optB, hardB)) {
		t.Fatal("AllowedEgress depends on input order")
	}
}

func TestBlockedPrefixes(t *testing.T) {
	got := BlockedPrefixes()
	if !slices.IsSortedFunc(got, comparePrefix) || len(got) != len(fixedBlockedPrefixes) {
		t.Fatalf("BlockedPrefixes = %v, want the fixed set sorted", got)
	}
	for _, p := range fixedBlockedPrefixes {
		if !slices.Contains(got, p) {
			t.Fatalf("BlockedPrefixes lacks %s", p)
		}
	}
	got[0] = netip.MustParsePrefix("1.2.3.4/32")
	if slices.Contains(BlockedPrefixes(), got[0]) {
		t.Fatal("BlockedPrefixes returns a shared slice")
	}
}

func TestHardBlockedPrefixes(t *testing.T) {
	tests := []struct {
		name    string
		addrs   []string
		subnets []string
		want    []string
	}{
		{name: "nothing"},
		{
			name:  "IPv4 address added as /32",
			addrs: []string{"203.0.113.7"},
			want:  []string{"203.0.113.7/32"},
		},
		{
			name:  "4-in-6 unmapped, IPv6 ignored, duplicates collapsed",
			addrs: []string{"::ffff:198.51.100.9", "2001:db8::1", "fe80::1", "198.51.100.9"},
			want:  []string{"198.51.100.9/32"},
		},
		{
			name:    "Lima VM: VM, Mac, gateway and bridge subnet, sorted and masked",
			addrs:   []string{"192.168.5.15", "10.20.30.1", "192.168.5.2", "127.0.0.1"},
			subnets: []string{"10.20.30.1/24"},
			want:    []string{"10.20.30.0/24", "10.20.30.1/32", "127.0.0.1/32", "192.168.5.2/32", "192.168.5.15/32"},
		},
		{
			name:    "IPv6 subnet ignored",
			subnets: []string{"fd42::/64", "10.20.30.0/24"},
			want:    []string{"10.20.30.0/24"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HardBlockedPrefixes(addrs(t, tt.addrs...), mustPrefixes(t, tt.subnets...)...)
			want := mustPrefixes(t, tt.want...)
			if !slices.Equal(got, want) {
				t.Fatalf("HardBlockedPrefixes = %v, want %v", got, want)
			}
		})
	}
}

func TestOptInHardBlocked(t *testing.T) {
	tests := []struct {
		name  string
		optIn []string
		hard  []string
		want  []string
	}{
		{name: "no opt-in", hard: []string{"10.20.30.0/24"}},
		{name: "no overlap", optIn: []string{"10.1.0.0/16"}, hard: []string{"10.20.30.0/24", "203.0.113.7/32"}},
		{
			name:  "opt-in covers hard blocks, nested hard blocks collapse",
			optIn: []string{"10.0.0.0/8", "192.168.0.0/16"},
			hard:  []string{"10.20.30.1/32", "10.20.30.0/24", "192.168.5.15/32", "192.168.5.2/32", "203.0.113.7/32"},
			want:  []string{"10.20.30.0/24", "192.168.5.2/32", "192.168.5.15/32"},
		},
		{
			name:  "opt-in inside a hard block reports the opt-in",
			optIn: []string{"10.20.30.0/28"},
			hard:  []string{"10.20.30.0/24"},
			want:  []string{"10.20.30.0/28"},
		},
		{
			name:  "hard block equal to opt-in, reported once",
			optIn: []string{"10.20.30.0/24", "10.0.0.0/8"},
			hard:  []string{"10.20.30.0/24"},
			want:  []string{"10.20.30.0/24"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OptInHardBlocked(mustPrefixes(t, tt.optIn...), mustPrefixes(t, tt.hard...))
			want := mustPrefixes(t, tt.want...)
			if !slices.Equal(got, want) {
				t.Fatalf("OptInHardBlocked = %v, want %v", got, want)
			}
		})
	}
}

func TestParseOptIn(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr string
	}{
		{name: "empty", in: nil, want: nil},
		{name: "valid list", in: []string{"10.1.0.0/16", " 192.168.1.10/32 "}, want: []string{"10.1.0.0/16", "192.168.1.10/32"}},
		{name: "masked to network", in: []string{"10.1.2.3/16"}, want: []string{"10.1.0.0/16"}},
		{name: "/8 is the widest allowed", in: []string{"10.0.0.0/8"}, want: []string{"10.0.0.0/8"}},
		{name: "garbage", in: []string{"not-a-cidr"}, wantErr: "not a CIDR"},
		{name: "bare address", in: []string{"10.1.2.3"}, wantErr: "not a CIDR"},
		{name: "bad length", in: []string{"10.0.0.0/33"}, wantErr: "not a CIDR"},
		{name: "IPv6", in: []string{"fd00::/8"}, wantErr: "not IPv4"},
		{name: "4-in-6", in: []string{"::ffff:10.0.0.0/104"}, wantErr: "not IPv4"},
		{name: "default route", in: []string{"0.0.0.0/0"}, wantErr: "defeats the egress firewall"},
		{name: "wider than /8", in: []string{"10.0.0.0/7"}, wantErr: "defeats the egress firewall"},
		{name: "one bad entry fails all", in: []string{"10.1.0.0/16", "nope"}, wantErr: "not a CIDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOptIn(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseOptIn(%q) = %v, want error containing %q", tt.in, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseOptIn(%q) error = %q, want it to contain %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOptIn(%q): %v", tt.in, err)
			}
			want := mustPrefixes(t, tt.want...)
			if !slices.Equal(got, want) {
				t.Fatalf("ParseOptIn(%q) = %v, want %v", tt.in, got, want)
			}
		})
	}
}

// aclDoc is the subset of the Incus ACL document the tests inspect.
type aclDoc struct {
	description string
	egress      []map[string]string
	ingressSeen bool
	topKeys     []string
}

// parseACLYAML is a strict reader for the exact YAML shape EgressACLYAML
// emits. go.mod has no YAML library and the brief rules out adding one, so
// this checks structure line by line instead: any line it does not expect
// fails the test, which is stricter than a general parser would be.
func parseACLYAML(t *testing.T, doc string) aclDoc {
	t.Helper()
	if !strings.HasSuffix(doc, "\n") {
		t.Fatalf("document does not end in a newline:\n%s", doc)
	}
	var d aclDoc
	var section string
	for i, line := range strings.Split(strings.TrimSuffix(doc, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			if section != "egress" {
				t.Fatalf("line %d: list item outside egress: %q", i+1, line)
			}
			d.egress = append(d.egress, map[string]string{})
			line = "  " + line[2:]
			fallthrough
		case strings.HasPrefix(line, "  "):
			if section != "egress" || len(d.egress) == 0 {
				t.Fatalf("line %d: unexpected indented line: %q", i+1, line)
			}
			k, v, ok := strings.Cut(strings.TrimPrefix(line, "  "), ": ")
			if !ok || strings.ContainsAny(k, " :") || v == "" {
				t.Fatalf("line %d: malformed rule field: %q", i+1, line)
			}
			rule := d.egress[len(d.egress)-1]
			if _, dup := rule[k]; dup {
				t.Fatalf("line %d: duplicate key %q in rule", i+1, k)
			}
			rule[k] = v
		default:
			k, v, _ := strings.Cut(line, ":")
			v = strings.TrimSpace(v)
			d.topKeys = append(d.topKeys, k)
			section = k
			switch k {
			case "description":
				if !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) || strings.Count(v, `"`) != 2 || strings.Contains(v, `\`) {
					t.Fatalf("line %d: description must be a simple double-quoted string: %q", i+1, v)
				}
				d.description = strings.Trim(v, `"`)
			case "egress":
				if v != "" && v != "[]" {
					t.Fatalf("line %d: unexpected egress value %q", i+1, v)
				}
			case "ingress":
				if v != "[]" {
					t.Fatalf("line %d: ingress = %q, want []", i+1, v)
				}
				d.ingressSeen = true
			default:
				t.Fatalf("line %d: unexpected top-level key %q", i+1, k)
			}
		}
	}
	return d
}

func TestEgressACLYAML(t *testing.T) {
	tests := []struct {
		name    string
		allowed []netip.Prefix
	}{
		{name: "empty allow-list", allowed: nil},
		{name: "two prefixes", allowed: mustPrefixes(t, "1.0.0.0/8", "8.8.8.0/24")},
		{name: "real default set", allowed: AllowedEgress(BlockedPrefixes(), nil, HardBlockedPrefixes(addrs(t, "203.0.113.7")))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := EgressACLYAML(tt.allowed)
			d := parseACLYAML(t, out)
			if !strings.Contains(d.description, "ahjo") {
				t.Fatalf("description %q does not mention ahjo", d.description)
			}
			if !d.ingressSeen {
				t.Fatal("missing ingress: []")
			}
			if !slices.Equal(d.topKeys, []string{"description", "egress", "ingress"}) {
				t.Fatalf("top-level keys = %v, want description, egress, ingress", d.topKeys)
			}
			if len(d.egress) != len(tt.allowed) {
				t.Fatalf("got %d egress rules, want %d", len(d.egress), len(tt.allowed))
			}
			for i, rule := range d.egress {
				want := map[string]string{
					"action":      "allow",
					"destination": tt.allowed[i].String(),
					"state":       "enabled",
				}
				if len(rule) != len(want) {
					t.Fatalf("rule %d = %v, want %v", i, rule, want)
				}
				for k, v := range want {
					if rule[k] != v {
						t.Fatalf("rule %d %s = %q, want %q", i, k, rule[k], v)
					}
				}
			}
		})
	}
}

func TestHostIPv4Addrs(t *testing.T) {
	addrs, err := HostIPv4Addrs()
	if err != nil {
		t.Fatalf("HostIPv4Addrs: %v", err)
	}
	for _, a := range addrs {
		if !a.Is4() {
			t.Fatalf("HostIPv4Addrs returned non-IPv4 %s", a)
		}
	}
}
