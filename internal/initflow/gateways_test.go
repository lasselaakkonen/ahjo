package initflow

import (
	"net/netip"
	"slices"
	"testing"
)

func TestParseDefaultGateways(t *testing.T) {
	addrs := func(ss ...string) []netip.Addr {
		var out []netip.Addr
		for _, s := range ss {
			out = append(out, netip.MustParseAddr(s))
		}
		return out
	}
	tests := []struct {
		name string
		in   string
		want []netip.Addr
	}{
		{
			name: "lima vzNAT plus user-mode network",
			in: "default via 192.168.64.1 dev lima0 proto dhcp src 192.168.64.5 metric 100 \n" +
				"default via 192.168.5.2 dev eth0 proto dhcp src 192.168.5.15 metric 200 \n",
			want: addrs("192.168.5.2", "192.168.64.1"),
		},
		{
			name: "default without via has no gateway",
			in:   "default dev wg0 scope link \ndefault via 10.0.2.2 dev eth0 \n",
			want: addrs("10.0.2.2"),
		},
		{
			name: "only device default",
			in:   "default dev tun0 proto static scope link metric 50\n",
			want: nil,
		},
		{
			name: "duplicates across routes collapse",
			in: "default via 192.168.64.1 dev eth1 metric 100\n" +
				"default via 192.168.64.1 dev eth2 metric 200\n",
			want: addrs("192.168.64.1"),
		},
		{
			name: "multipath nexthops",
			in:   "default proto static metric 100 nexthop via 192.168.1.1 dev eth0 weight 1 nexthop via 192.168.2.1 dev eth1 weight 1\n",
			want: addrs("192.168.1.1", "192.168.2.1"),
		},
		{
			name: "IPv6 gateways and junk ignored",
			in: "default via inet6 fe80::1 dev eth0 proto ra\n" +
				"default via fe80::1 dev eth0\n" +
				"unreachable default metric 4278198272\n" +
				"10.20.30.0/24 via 10.0.0.1 dev incusbr0\n" +
				"garbage\n" +
				"default via not-an-ip dev eth0\n" +
				"default via\n" +
				"\n",
			want: nil,
		},
		{
			name: "IPv4 nexthop spelled with inet",
			in:   "default via inet 192.168.64.1 dev eth1\n",
			want: addrs("192.168.64.1"),
		},
		{
			name: "empty output",
			in:   "",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDefaultGateways(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("parseDefaultGateways = %v, want %v", got, tt.want)
			}
		})
	}
}
