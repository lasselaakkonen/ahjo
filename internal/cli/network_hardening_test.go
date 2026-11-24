package cli

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

const testBridgeIPv4 = "10.20.30.1/24"

func egressIn(hostAddrs []netip.Addr, allowCIDRs []string) egressInputs {
	return egressInputs{HostAddrs: hostAddrs, BridgeIPv4: testBridgeIPv4, AllowCIDRs: allowCIDRs}
}

func mustDesired(t *testing.T, in egressInputs) egressDesired {
	t.Helper()
	d, err := computeEgressDesired(in)
	if err != nil {
		t.Fatalf("computeEgressDesired(%+v): %v", in, err)
	}
	return d
}

func (d egressDesired) allows(s string) bool {
	a := netip.MustParseAddr(s)
	for _, p := range d.Allowed {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func TestComputeEgressDesiredInvalidOptIn(t *testing.T) {
	for _, bad := range []string{"not-a-cidr", "10.0.0.0", "fd00::/8", "0.0.0.0/0"} {
		if _, err := computeEgressDesired(egressIn(nil, []string{bad})); err == nil {
			t.Fatalf("computeEgressDesired(%q): want error, got nil", bad)
		}
	}
}

func TestComputeEgressDesiredBridgeAddress(t *testing.T) {
	for _, bad := range []string{"", "none", " none ", "auto", "garbage", "10.20.30.1", "fd42::1/64"} {
		_, err := computeEgressDesired(egressInputs{BridgeIPv4: bad})
		if err == nil {
			t.Fatalf("bridge ipv4.address %q: want error, got nil", bad)
		}
		if !strings.Contains(err.Error(), "incusbr0") || !strings.Contains(err.Error(), "ipv4.address") {
			t.Fatalf("bridge ipv4.address %q: error %q does not name the key", bad, err)
		}
	}
	d := mustDesired(t, egressInputs{BridgeIPv4: "10.99.7.1/24"})
	if d.BridgeSubnet != netip.MustParsePrefix("10.99.7.0/24") {
		t.Fatalf("BridgeSubnet = %s, want masked 10.99.7.0/24", d.BridgeSubnet)
	}
}

func TestComputeEgressDesiredHardBlocks(t *testing.T) {
	d := mustDesired(t, egressInputs{
		HostAddrs:  []netip.Addr{netip.MustParseAddr("192.168.5.15"), netip.MustParseAddr("10.20.30.1")},
		LimaHost:   netip.MustParseAddr("192.168.5.2"),
		BridgeIPv4: testBridgeIPv4,
		AllowCIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"},
	})
	for _, p := range []string{"10.20.30.0/24", "10.20.30.1/32", "192.168.5.2/32", "192.168.5.15/32"} {
		if !slices.Contains(d.Hard, netip.MustParsePrefix(p)) {
			t.Errorf("Hard = %v, lacks %s", d.Hard, p)
		}
	}
	for _, s := range []string{"10.20.30.1", "10.20.30.57", "192.168.5.2", "192.168.5.15"} {
		if d.allows(s) {
			t.Errorf("%s is allowed under a covering opt-in, want hard-blocked", s)
		}
	}
	for _, s := range []string{"10.1.2.3", "10.20.31.1", "192.168.5.3", "192.168.1.10"} {
		if !d.allows(s) {
			t.Errorf("%s is blocked, want allowed by the opt-in", s)
		}
	}
	wantOverlap := []netip.Prefix{
		netip.MustParsePrefix("10.20.30.0/24"),
		netip.MustParsePrefix("192.168.5.2/32"),
		netip.MustParsePrefix("192.168.5.15/32"),
	}
	if !slices.Equal(d.OptInHardBlocked, wantOverlap) {
		t.Fatalf("OptInHardBlocked = %v, want %v", d.OptInHardBlocked, wantOverlap)
	}
	line := d.optInHardBlockedLine()
	for _, want := range []string{"10.20.30.0/24, 192.168.5.2/32, 192.168.5.15/32", "stays blocked", "ahjo forward"} {
		if !strings.Contains(line, want) {
			t.Errorf("overlap line lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Errorf("overlap line is not one line: %q", line)
	}
	sum := d.hardBlockSummary()
	for _, want := range []string{"incusbr0 subnet 10.20.30.0/24", "192.168.5.15", "host.lima.internal 192.168.5.2"} {
		if !strings.Contains(sum, want) {
			t.Errorf("hard-block summary lacks %q: %s", want, sum)
		}
	}

	// Native Linux, opt-in clear of every hard block: no overlap line, no
	// Lima entry.
	n := mustDesired(t, egressIn([]netip.Addr{netip.MustParseAddr("203.0.113.7")}, []string{"10.1.0.0/16"}))
	if line := n.optInHardBlockedLine(); line != "" {
		t.Errorf("unexpected overlap line: %s", line)
	}
	if strings.Contains(n.hardBlockSummary(), "lima") {
		t.Errorf("summary mentions Lima on a native host: %s", n.hardBlockSummary())
	}
}

// TestComputeEgressDesiredLimaGateways checks that the VM's default gateways,
// which are the Mac, stay blocked under a broad opt-in, and that a native
// Linux host (no gateways passed) leaves its router to the opt-in.
func TestComputeEgressDesiredLimaGateways(t *testing.T) {
	wide := []string{"192.168.0.0/16"}
	d := mustDesired(t, egressInputs{
		HostAddrs:    []netip.Addr{netip.MustParseAddr("192.168.64.5")},
		LimaHost:     netip.MustParseAddr("192.168.5.2"),
		LimaGateways: []netip.Addr{netip.MustParseAddr("192.168.5.2"), netip.MustParseAddr("192.168.64.1")},
		BridgeIPv4:   testBridgeIPv4,
		AllowCIDRs:   wide,
	})
	for _, s := range []string{"192.168.5.2", "192.168.64.1", "192.168.64.5"} {
		if d.allows(s) {
			t.Errorf("%s is allowed under opt-in %v, want hard-blocked", s, wide)
		}
	}
	for _, s := range []string{"192.168.64.2", "192.168.1.10"} {
		if !d.allows(s) {
			t.Errorf("%s is blocked, want allowed by the opt-in", s)
		}
	}
	if !slices.Contains(d.OptInHardBlocked, netip.MustParsePrefix("192.168.64.1/32")) {
		t.Errorf("OptInHardBlocked = %v, lacks the gateway", d.OptInHardBlocked)
	}
	sum := d.hardBlockSummary()
	if !strings.Contains(sum, "Mac gateways 192.168.64.1") || !strings.Contains(sum, "host.lima.internal 192.168.5.2") {
		t.Errorf("summary does not name the Mac gateway and host.lima.internal: %s", sum)
	}
	if n := strings.Count(sum, "192.168.5.2"); n != 1 {
		t.Errorf("summary names 192.168.5.2 %d times, want once: %s", n, sum)
	}

	// Only host.lima.internal as gateway: no separate gateway entry.
	only := mustDesired(t, egressInputs{
		LimaHost:     netip.MustParseAddr("192.168.5.2"),
		LimaGateways: []netip.Addr{netip.MustParseAddr("192.168.5.2")},
		BridgeIPv4:   testBridgeIPv4,
	})
	if strings.Contains(only.hardBlockSummary(), "gateways") {
		t.Errorf("summary lists a gateway already named as host.lima.internal: %s", only.hardBlockSummary())
	}

	// Native Linux: no gateways passed, so the router inside the opt-in is
	// reachable and the summary says nothing about gateways.
	n := mustDesired(t, egressIn([]netip.Addr{netip.MustParseAddr("192.168.1.20")}, wide))
	for _, s := range []string{"192.168.1.1", "192.168.64.1"} {
		if !n.allows(s) {
			t.Errorf("native Linux: %s is blocked, want allowed by the opt-in", s)
		}
	}
	if n.allows("192.168.1.20") {
		t.Error("native Linux: host address 192.168.1.20 is allowed, want hard-blocked")
	}
	if strings.Contains(n.hardBlockSummary(), "gateways") {
		t.Errorf("native Linux summary mentions gateways: %s", n.hardBlockSummary())
	}
}

func TestComputeEgressDesiredHashStable(t *testing.T) {
	a := []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("10.20.30.1")}
	b := []netip.Addr{netip.MustParseAddr("10.20.30.1"), netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("203.0.113.7")}
	d1 := mustDesired(t, egressIn(a, []string{"10.1.0.0/16"}))
	d2 := mustDesired(t, egressIn(b, []string{"10.1.0.0/16"}))
	if d1.Hash != d2.Hash {
		t.Fatalf("hash depends on host address order/duplicates: %s vs %s", d1.Hash, d2.Hash)
	}
	if d1.Hash != rulesHash(d1.RulesYAML) {
		t.Fatal("Hash is not the hash of RulesYAML")
	}
	if len(d1.Hash) != 64 {
		t.Fatalf("hash %q is not sha256 hex", d1.Hash)
	}

	d3 := mustDesired(t, egressIn(a, nil))
	if d3.Hash == d1.Hash {
		t.Fatal("dropping the opt-in did not change the hash")
	}
	d4 := mustDesired(t, egressIn([]netip.Addr{netip.MustParseAddr("198.51.100.9")}, []string{"10.1.0.0/16"}))
	if d4.Hash == d1.Hash {
		t.Fatal("a different public host address did not change the hash")
	}
}

// TestComputeEgressDesiredHashTracksHardBlocks checks that moving a hard block
// the opt-ins expose re-renders the rules, so `ahjo update` re-applies them.
func TestComputeEgressDesiredHashTracksHardBlocks(t *testing.T) {
	wide := []string{"10.0.0.0/8", "192.168.0.0/16"}
	base := egressInputs{BridgeIPv4: testBridgeIPv4, LimaHost: netip.MustParseAddr("192.168.5.2"), AllowCIDRs: wide}
	h := mustDesired(t, base).Hash

	bridge := base
	bridge.BridgeIPv4 = "10.20.31.1/24"
	if mustDesired(t, bridge).Hash == h {
		t.Error("a different bridge subnet did not change the hash")
	}
	mac := base
	mac.LimaHost = netip.MustParseAddr("192.168.5.3")
	if mustDesired(t, mac).Hash == h {
		t.Error("a different host.lima.internal did not change the hash")
	}
	noLima := base
	noLima.LimaHost = netip.Addr{}
	if mustDesired(t, noLima).Hash == h {
		t.Error("dropping host.lima.internal did not change the hash")
	}
	gw := base
	gw.LimaGateways = []netip.Addr{netip.MustParseAddr("192.168.64.1")}
	if mustDesired(t, gw).Hash == h {
		t.Error("a Lima default gateway inside an opt-in did not change the hash")
	}
	host := base
	host.HostAddrs = []netip.Addr{netip.MustParseAddr("10.1.2.3")}
	if mustDesired(t, host).Hash == h {
		t.Error("a new host address inside an opt-in did not change the hash")
	}
}

func TestComputeEgressDesiredContent(t *testing.T) {
	d := mustDesired(t, egressIn([]netip.Addr{netip.MustParseAddr("203.0.113.7")}, []string{"10.1.0.0/16"}))
	for _, s := range []string{"192.168.5.2", "169.254.169.254", "10.20.30.1", "203.0.113.7", "127.0.0.1"} {
		if d.allows(s) {
			t.Errorf("%s is allowed, want blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "140.82.112.3", "10.1.2.3"} {
		if !d.allows(s) {
			t.Errorf("%s is blocked, want allowed", s)
		}
	}
	if len(d.OptIn) != 1 || d.OptIn[0].String() != "10.1.0.0/16" {
		t.Fatalf("OptIn = %v", d.OptIn)
	}
}

func TestACLEditYAMLCarriesHash(t *testing.T) {
	d := mustDesired(t, egressIn(nil, nil))
	y := d.aclEditYAML()
	if !strings.HasPrefix(y, d.RulesYAML) {
		t.Fatal("edit body does not start with the hashed rules")
	}
	want := "config:\n  " + incus.EgressACLHashKey + ": \"" + d.Hash + "\"\n"
	if !strings.HasSuffix(y, want) {
		t.Fatalf("edit body does not end with the hash config block:\n%s", y[len(d.RulesYAML):])
	}
	if strings.Count(y, "config:") != 1 {
		t.Fatal("expected exactly one config block")
	}
}

// hardenedObserved is the observed state once the step has fully applied.
func hardenedObserved(hash string) incus.NetworkHardeningObserved {
	o := incus.NetworkHardeningObserved{
		NetworkExists: true,
		NetworkACLs:   incus.EgressACL,
		Network:       map[string]string{},
		NICExists:     true,
		NICNetwork:    incus.BridgeNetwork,
		NIC:           map[string]string{},
		ACLExists:     true,
		ACLHash:       hash,
	}
	for _, kv := range incus.HardenedNetworkDefaults() {
		o.Network[kv.Key] = kv.Value
	}
	for _, kv := range incus.HardenedNICKeys() {
		o.NIC[kv.Key] = kv.Value
	}
	return o
}

func TestNetworkHardeningSkip(t *testing.T) {
	const want = "abc"
	tests := []struct {
		name     string
		mutate   func(o *incus.NetworkHardeningObserved)
		wantSkip bool
		wantErr  string
	}{
		{"fully applied", func(*incus.NetworkHardeningObserved) {}, true, ""},
		{"user ACLs kept alongside ours", func(o *incus.NetworkHardeningObserved) {
			o.NetworkACLs = "corp-acl, " + incus.EgressACL
		}, true, ""},
		{"stale hash", func(o *incus.NetworkHardeningObserved) { o.ACLHash = "old" }, false, ""},
		{"no hash yet", func(o *incus.NetworkHardeningObserved) { o.ACLHash = "" }, false, ""},
		{"ACL missing", func(o *incus.NetworkHardeningObserved) { o.ACLExists = false; o.ACLHash = "" }, false, ""},
		{"not attached", func(o *incus.NetworkHardeningObserved) { o.NetworkACLs = "" }, false, ""},
		{"only other ACLs attached", func(o *incus.NetworkHardeningObserved) { o.NetworkACLs = "corp-acl" }, false, ""},
		{"name prefix is not ours", func(o *incus.NetworkHardeningObserved) { o.NetworkACLs = incus.EgressACL + "-2" }, false, ""},
		{"egress default allow", func(o *incus.NetworkHardeningObserved) {
			o.Network[incus.NetworkDefaultEgressKey] = "allow"
		}, false, ""},
		{"egress default unset", func(o *incus.NetworkHardeningObserved) {
			o.Network[incus.NetworkDefaultEgressKey] = ""
		}, false, ""},
		{"ingress default unset", func(o *incus.NetworkHardeningObserved) {
			o.Network[incus.NetworkDefaultIngressKey] = ""
		}, false, ""},
		{"port isolation off", func(o *incus.NetworkHardeningObserved) {
			o.NIC[incus.NICPortIsolationKey] = "false"
		}, false, ""},
		{"mac filtering unset", func(o *incus.NetworkHardeningObserved) {
			o.NIC[incus.NICMACFilteringKey] = ""
		}, false, ""},
		{"ipv4 filtering unset", func(o *incus.NetworkHardeningObserved) {
			o.NIC[incus.NICIPv4FilteringKey] = ""
		}, false, ""},
		{"network missing", func(o *incus.NetworkHardeningObserved) { o.NetworkExists = false }, false, "network incusbr0 not found"},
		{"eth0 missing", func(o *incus.NetworkHardeningObserved) { o.NICExists = false }, false, "has no eth0 device"},
		{"eth0 on another network", func(o *incus.NetworkHardeningObserved) { o.NICNetwork = "lxdbr0" }, false, `network="lxdbr0"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := hardenedObserved(want)
			tt.mutate(&o)
			skip, reason, err := networkHardeningSkip(o, want)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if skip {
					t.Fatal("skip must be false alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if skip != tt.wantSkip {
				t.Fatalf("skip = %v, want %v (drift %v)", skip, tt.wantSkip, hardeningDrift(o, want))
			}
			if skip && reason == "" {
				t.Fatal("skip without a reason")
			}
		})
	}
}

func TestNetworkHardeningShow(t *testing.T) {
	s := networkHardeningShow([]string{"10.1.0.0/16", "172.20.5.0/24"}, nil)
	for _, want := range []string{
		"incus network acl create ahjo-egress",
		"incus network acl edit ahjo-egress",
		"incus network set incusbr0 security.acls=<existing ACLs>,ahjo-egress security.acls.default.egress.action=reject security.acls.default.ingress.action=allow",
		"incus profile device set default eth0 security.port_isolation=true security.mac_filtering=true security.ipv4_filtering=true",
		"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.168.0.0/16", "224.0.0.0/3",
		"opt-in (network.allow_cidrs): 10.1.0.0/16, 172.20.5.0/24",
		"always blocked, even inside an opt-in: the incusbr0 subnet",
		"host.lima.internal and every default gateway",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Show lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "action: allow") || strings.Count(s, "\n") > 12 {
		t.Errorf("Show should not list the allow rules:\n%s", s)
	}
	if s := networkHardeningShow(nil, nil); !strings.Contains(s, "opt-in (network.allow_cidrs): none") {
		t.Errorf("Show without opt-ins:\n%s", s)
	}
	if s := networkHardeningShow(nil, errors.New("bad cidr")); !strings.Contains(s, "bad cidr") {
		t.Errorf("Show does not surface the config error:\n%s", s)
	}
}

func stepIndex(titles []string, prefix string) int {
	for i, t := range titles {
		if strings.HasPrefix(t, prefix) {
			return i
		}
	}
	return -1
}

func TestNetworkHardeningStepWiring(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hardening := networkHardeningStep().Title

	var initTitles []string
	for _, s := range vmInitSteps(context.Background(), true) {
		initTitles = append(initTitles, s.Title)
	}
	join := stepIndex(initTitles, "Join incus-admin group")
	h := stepIndex(initTitles, hardening)
	build := stepIndex(initTitles, "Build ahjo-base image")
	if join < 0 || h < 0 || build < 0 || h != join+1 || build != h+1 {
		t.Fatalf("init order: join=%d hardening=%d build=%d in %q", join, h, build, initTitles)
	}

	var updTitles []string
	for _, s := range vmUpdateSteps(context.Background()) {
		updTitles = append(updTitles, s.Title)
	}
	inotify := stepIndex(updTitles, inotifySysctlStep().Title)
	h = stepIndex(updTitles, hardening)
	if inotify < 0 || h != inotify+1 {
		t.Fatalf("update order: inotify=%d hardening=%d in %q", inotify, h, updTitles)
	}
}

func TestNetworkHardeningWording(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := networkHardeningStep()
	for _, want := range []string{"right away", "re-attach their network interface", "native Linux", "did not create", "ahjo forward"} {
		if !strings.Contains(st.Note, want) {
			t.Errorf("Note lacks %q: %s", want, st.Note)
		}
	}
	if strings.Contains(st.Post, "next restart") || !strings.Contains(st.Post, "right away") || !strings.Contains(st.Post, "drops open connections") {
		t.Errorf("Post wording: %s", st.Post)
	}
}
