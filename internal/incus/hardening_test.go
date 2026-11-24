package incus

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestACLList(t *testing.T) {
	tests := []struct {
		in       string
		contains bool
		with     string
	}{
		{"", false, "ahjo-egress"},
		{"ahjo-egress", true, "ahjo-egress"},
		{"corp", false, "corp,ahjo-egress"},
		{"corp,ahjo-egress", true, "corp,ahjo-egress"},
		{"ahjo-egress,corp", true, "ahjo-egress,corp"},
		{" corp , ahjo-egress ", true, "corp,ahjo-egress"},
		{"corp,,other", false, "corp,other,ahjo-egress"},
		{"ahjo-egress-2", false, "ahjo-egress-2,ahjo-egress"},
	}
	for _, tt := range tests {
		if got := ACLListContains(tt.in, EgressACL); got != tt.contains {
			t.Errorf("ACLListContains(%q) = %v, want %v", tt.in, got, tt.contains)
		}
		if got := ACLListWith(tt.in, EgressACL); got != tt.with {
			t.Errorf("ACLListWith(%q) = %q, want %q", tt.in, got, tt.with)
		}
	}
}

func hardenedForTest() NetworkHardeningObserved {
	o := NetworkHardeningObserved{
		NetworkExists: true, NetworkACLs: "corp," + EgressACL, Network: map[string]string{},
		NICExists: true, NICNetwork: BridgeNetwork, NIC: map[string]string{},
		ACLExists: true, ACLHash: "h",
	}
	for _, kv := range HardenedNetworkDefaults() {
		o.Network[kv.Key] = kv.Value
	}
	for _, kv := range HardenedNICKeys() {
		o.NIC[kv.Key] = kv.Value
	}
	return o
}

func TestNetworkHardeningStatus(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(o *NetworkHardeningObserved)
		egress, port bool
		reason       []string
	}{
		{"hardened", func(*NetworkHardeningObserved) {}, true, true, nil},
		{"fresh install", func(o *NetworkHardeningObserved) {
			*o = NetworkHardeningObserved{NetworkExists: true, NICExists: true, NICNetwork: BridgeNetwork}
		}, false, false, []string{"ACL ahjo-egress not found", "lacks security.port_isolation=true"}},
		{"not attached", func(o *NetworkHardeningObserved) { o.NetworkACLs = "corp" }, false, true,
			[]string{"does not have ahjo-egress"}},
		{"egress default allow", func(o *NetworkHardeningObserved) { o.Network[NetworkDefaultEgressKey] = "allow" }, false, true,
			[]string{`is "allow", want reject`}},
		{"no network", func(o *NetworkHardeningObserved) { o.NetworkExists = false }, false, true,
			[]string{"network incusbr0 not found"}},
		{"no eth0", func(o *NetworkHardeningObserved) { o.NICExists = false }, true, false,
			[]string{"has no eth0 device"}},
		{"eth0 elsewhere", func(o *NetworkHardeningObserved) { o.NICNetwork = "other" }, true, false,
			[]string{`network "other"`}},
		// Doctor only checks the two load-bearing parts.
		{"mac filtering off still ok", func(o *NetworkHardeningObserved) { o.NIC[NICMACFilteringKey] = "" }, true, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := hardenedForTest()
			tt.mutate(&o)
			s := o.Status()
			if s.EgressFiltered != tt.egress || s.PortIsolated != tt.port {
				t.Fatalf("status = %+v, want egress=%v port=%v", s, tt.egress, tt.port)
			}
			if s.OK() != (tt.egress && tt.port) {
				t.Fatalf("OK() = %v", s.OK())
			}
			if len(tt.reason) == 0 && s.Reason != "" {
				t.Fatalf("reason = %q, want empty", s.Reason)
			}
			for _, r := range tt.reason {
				if !strings.Contains(s.Reason, r) {
					t.Fatalf("reason %q lacks %q", s.Reason, r)
				}
			}
		})
	}
}

func TestObserveNetworkHardening(t *testing.T) {
	fe := withFakeExec(t,
		fakeRun{stdout: `[{"name":"lxdbr0"},{"name":"incusbr0"}]`}, // network list
		fakeRun{stdout: "corp,ahjo-egress\n"},                      // security.acls
		fakeRun{stdout: "reject\n"},                                // default egress
		fakeRun{stdout: "allow\n"},                                 // default ingress
		fakeRun{stdout: "eth0\nroot\n"},                            // profile device list
		fakeRun{stdout: "incusbr0\n"},                              // eth0 network
		fakeRun{stdout: "true\n"},                                  // port isolation
		fakeRun{stdout: "true\n"},                                  // mac filtering
		fakeRun{stdout: "\n"},                                      // ipv4 filtering unset
		fakeRun{stdout: `[{"name":"ahjo-egress"}]`},                // acl list
		fakeRun{stdout: "deadbeef\n"},                              // hash
	)
	o, err := ObserveNetworkHardening()
	if err != nil {
		t.Fatalf("ObserveNetworkHardening: %v", err)
	}
	want := NetworkHardeningObserved{
		NetworkExists: true, NetworkACLs: "corp,ahjo-egress",
		Network:   map[string]string{NetworkDefaultEgressKey: "reject", NetworkDefaultIngressKey: "allow"},
		NICExists: true, NICNetwork: "incusbr0",
		NIC:       map[string]string{NICPortIsolationKey: "true", NICMACFilteringKey: "true", NICIPv4FilteringKey: ""},
		ACLExists: true, ACLHash: "deadbeef",
	}
	if !reflect.DeepEqual(o, want) {
		t.Fatalf("observed:\n got %+v\nwant %+v", o, want)
	}
	wantCalls := [][]string{
		{"incus", "network", "list", "--format=json"},
		{"incus", "network", "get", "incusbr0", "security.acls"},
		{"incus", "network", "get", "incusbr0", "security.acls.default.egress.action"},
		{"incus", "network", "get", "incusbr0", "security.acls.default.ingress.action"},
		{"incus", "profile", "device", "list", "default"},
		{"incus", "profile", "device", "get", "default", "eth0", "network"},
		{"incus", "profile", "device", "get", "default", "eth0", "security.port_isolation"},
		{"incus", "profile", "device", "get", "default", "eth0", "security.mac_filtering"},
		{"incus", "profile", "device", "get", "default", "eth0", "security.ipv4_filtering"},
		{"incus", "network", "acl", "list", "--format=json"},
		{"incus", "network", "acl", "get", "ahjo-egress", "user.ahjo.rules-hash"},
	}
	if !reflect.DeepEqual(fe.Calls, wantCalls) {
		t.Fatalf("calls:\n got %q\nwant %q", fe.Calls, wantCalls)
	}
}

func TestObserveNetworkHardeningMissingObjects(t *testing.T) {
	fe := withFakeExec(t,
		fakeRun{stdout: `[{"name":"lxdbr0"}]`}, // network list: no incusbr0
		fakeRun{stdout: "root\n"},              // profile device list: no eth0
		fakeRun{stdout: `[]`},                  // acl list
	)
	o, err := ObserveNetworkHardening()
	if err != nil {
		t.Fatalf("ObserveNetworkHardening: %v", err)
	}
	if o.NetworkExists || o.NICExists || o.ACLExists {
		t.Fatalf("observed %+v, want nothing to exist", o)
	}
	if len(fe.Calls) != 3 {
		t.Fatalf("getters ran for missing objects: %q", fe.Calls)
	}
}

func TestObserveNetworkHardeningIncusError(t *testing.T) {
	withFakeExec(t, fakeRun{stderr: "Error: Get \"http://unix.socket/1.0\": dial unix: connect: permission denied", exit: 1})
	if _, err := ObserveNetworkHardening(); err == nil {
		t.Fatal("expected incus failure to surface as an error")
	}
	withFakeExec(t, fakeRun{stderr: "Error: boom", exit: 1})
	if _, err := CheckNetworkHardening(); err == nil {
		t.Fatal("CheckNetworkHardening must surface incus failures")
	}
}

func TestProfileDeviceExists(t *testing.T) {
	withFakeExec(t, fakeRun{stdout: "eth0\nroot\n"})
	if ok, err := ProfileDeviceExists("default", "eth0"); err != nil || !ok {
		t.Fatalf("got (%v, %v), want (true, nil)", ok, err)
	}
	withFakeExec(t, fakeRun{stdout: "eth01\nroot\n"})
	if ok, err := ProfileDeviceExists("default", "eth0"); err != nil || ok {
		t.Fatalf("got (%v, %v), want (false, nil)", ok, err)
	}
	withFakeExec(t, fakeRun{stderr: "Error: Profile not found", exit: 1})
	if _, err := ProfileDeviceExists("nope", "eth0"); err == nil {
		t.Fatal("missing profile must be an error")
	}
}

func TestLimaHostIPv4(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		err     error
		want    string
		wantErr string
	}{
		{name: "IPv4", ip: "192.168.5.2", want: "192.168.5.2"},
		{name: "4-in-6 unmapped", ip: "::ffff:192.168.5.2", want: "192.168.5.2"},
		{name: "IPv6 rejected", ip: "fe80::1", wantErr: "want an IPv4"},
		{name: "garbage rejected", ip: "not-an-ip", wantErr: "resolved to"},
		{name: "lookup error passed through", err: errors.New("no such host"), wantErr: "no such host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := limaHostIPv4(func() (string, error) { return tt.ip, tt.err })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
