package initflow

import (
	"net"
	"strings"
	"testing"
)

func mustRoute(t *testing.T, cidr, dev string) onLinkRoute {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", cidr, err)
	}
	return onLinkRoute{dest: n, dev: dev}
}

func TestFirstCollision(t *testing.T) {
	_, cand, err := net.ParseCIDR("10.20.30.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	tests := []struct {
		name    string
		routes  []onLinkRoute
		wantHit bool
		wantDev string
	}{
		{
			name:    "exact /24 match",
			routes:  []onLinkRoute{mustRoute(t, "10.20.30.0/24", "eth0")},
			wantHit: true,
			wantDev: "eth0",
		},
		{
			name:    "/16 superset covers candidate",
			routes:  []onLinkRoute{mustRoute(t, "10.20.0.0/16", "br-outer")},
			wantHit: true,
			wantDev: "br-outer",
		},
		{
			name:    "/25 subset inside candidate",
			routes:  []onLinkRoute{mustRoute(t, "10.20.30.0/25", "vethX")},
			wantHit: true,
			wantDev: "vethX",
		},
		{
			name:    "disjoint /24",
			routes:  []onLinkRoute{mustRoute(t, "10.30.40.0/24", "eth0")},
			wantHit: false,
		},
		{
			name:    "empty routes",
			routes:  nil,
			wantHit: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hit, dev := firstCollision(cand, tt.routes)
			if hit != tt.wantHit {
				t.Fatalf("firstCollision hit = %v, want %v", hit, tt.wantHit)
			}
			if hit && dev != tt.wantDev {
				t.Fatalf("firstCollision dev = %q, want %q", dev, tt.wantDev)
			}
		})
	}
}

func TestPickFromRoutes(t *testing.T) {
	tests := []struct {
		name       string
		routes     []onLinkRoute
		wantCIDR   string
		wantReason string
		wantErr    bool
	}{
		{
			name:       "no collisions returns first candidate",
			routes:     nil,
			wantCIDR:   "10.20.30.1/24",
			wantReason: "no on-link collisions",
		},
		{
			name: "nested-ahjo case: default /24 taken by outer eth0",
			routes: []onLinkRoute{
				mustRoute(t, "10.20.30.0/24", "eth0"),
			},
			wantCIDR:   "10.30.40.1/24",
			wantReason: "10.20.30.0/24 already on-link via eth0",
		},
		{
			name: "first three candidates all on-link, picks fourth",
			routes: []onLinkRoute{
				mustRoute(t, "10.20.30.0/24", "eth0"),
				mustRoute(t, "10.30.40.0/24", "br0"),
				mustRoute(t, "10.40.50.0/24", "virbr0"),
			},
			wantCIDR:   "10.50.60.1/24",
			wantReason: "10.20.30.0/24 already on-link via eth0",
		},
		{
			name: "every candidate collides via explicit /24 routes",
			routes: []onLinkRoute{
				mustRoute(t, "10.20.30.0/24", "a"),
				mustRoute(t, "10.30.40.0/24", "b"),
				mustRoute(t, "10.40.50.0/24", "c"),
				mustRoute(t, "10.50.60.0/24", "d"),
				mustRoute(t, "10.60.70.0/24", "e"),
			},
			wantErr: true,
		},
		{
			name: "10.0.0.0/8 superset catches all candidates",
			routes: []onLinkRoute{
				mustRoute(t, "10.0.0.0/8", "tun0"),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cidr, reason, err := pickFromRoutes(tt.routes)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("pickFromRoutes: want error, got cidr=%q reason=%q", cidr, reason)
				}
				if !strings.Contains(err.Error(), "10.20.30.0/24") {
					t.Fatalf("error %q should mention the candidate list", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("pickFromRoutes: unexpected error: %v", err)
			}
			if cidr != tt.wantCIDR {
				t.Fatalf("cidr = %q, want %q", cidr, tt.wantCIDR)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestGatewayCIDR(t *testing.T) {
	_, n, err := net.ParseCIDR("10.30.40.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	if got, want := gatewayCIDR(n), "10.30.40.1/24"; got != want {
		t.Fatalf("gatewayCIDR = %q, want %q", got, want)
	}
}
