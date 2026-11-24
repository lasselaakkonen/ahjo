package cli

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

func TestComputeEgressDesiredInvalidOptIn(t *testing.T) {
	for _, bad := range []string{"not-a-cidr", "10.0.0.0", "fd00::/8", "0.0.0.0/0"} {
		if _, err := computeEgressDesired(nil, []string{bad}); err == nil {
			t.Fatalf("computeEgressDesired(%q): want error, got nil", bad)
		}
	}
}

func TestComputeEgressDesiredHashStable(t *testing.T) {
	a := []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("10.20.30.1")}
	b := []netip.Addr{netip.MustParseAddr("10.20.30.1"), netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("203.0.113.7")}
	d1, err := computeEgressDesired(a, []string{"10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := computeEgressDesired(b, []string{"10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if d1.Hash != d2.Hash {
		t.Fatalf("hash depends on host address order/duplicates: %s vs %s", d1.Hash, d2.Hash)
	}
	if d1.Hash != rulesHash(d1.RulesYAML) {
		t.Fatal("Hash is not the hash of RulesYAML")
	}
	if len(d1.Hash) != 64 {
		t.Fatalf("hash %q is not sha256 hex", d1.Hash)
	}

	d3, err := computeEgressDesired(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d3.Hash == d1.Hash {
		t.Fatal("dropping the opt-in did not change the hash")
	}
	d4, err := computeEgressDesired([]netip.Addr{netip.MustParseAddr("198.51.100.9")}, []string{"10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if d4.Hash == d1.Hash {
		t.Fatal("a different public host address did not change the hash")
	}
}

func TestComputeEgressDesiredContent(t *testing.T) {
	d, err := computeEgressDesired([]netip.Addr{netip.MustParseAddr("203.0.113.7")}, []string{"10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	inAllowed := func(s string) bool {
		a := netip.MustParseAddr(s)
		for _, p := range d.Allowed {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	for _, s := range []string{"192.168.5.2", "169.254.169.254", "10.20.30.1", "203.0.113.7", "127.0.0.1"} {
		if inAllowed(s) {
			t.Errorf("%s is allowed, want blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "140.82.112.3", "10.1.2.3"} {
		if !inAllowed(s) {
			t.Errorf("%s is blocked, want allowed", s)
		}
	}
	if len(d.OptIn) != 1 || d.OptIn[0].String() != "10.1.0.0/16" {
		t.Fatalf("OptIn = %v", d.OptIn)
	}
}

func TestACLEditYAMLCarriesHash(t *testing.T) {
	d, err := computeEgressDesired(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
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
