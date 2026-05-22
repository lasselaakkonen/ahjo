package cli

import (
	"reflect"
	"sort"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

func TestParseForwardPort(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"8000", 8000, false},
		{"1", 1, false},
		{"65535", 65535, false},
		{"0", 0, true},
		{"65536", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		got, err := parseForwardPort(tt.in)
		if (err != nil) != tt.wantErr {
			t.Fatalf("parseForwardPort(%q): err=%v wantErr=%v", tt.in, err, tt.wantErr)
		}
		if !tt.wantErr && got != tt.want {
			t.Fatalf("parseForwardPort(%q)=%d want %d", tt.in, got, tt.want)
		}
	}
}

func TestForwardDeviceName(t *testing.T) {
	if got := forwardDeviceName(3000); got != "ahjo-forward-3000" {
		t.Fatalf("forwardDeviceName(3000)=%q want ahjo-forward-3000", got)
	}
}

func TestFormatForwards(t *testing.T) {
	tests := []struct {
		name    string
		devices []incus.ProxyDevice
		want    string
	}{
		{"none", nil, "-"},
		{
			"ignores ssh/expose devices",
			[]incus.ProxyDevice{
				{Name: "ahjo-ssh", Listen: "tcp:127.0.0.1:10000", Connect: "tcp:127.0.0.1:22"},
				{Name: "ahjo-expose-3000", Listen: "tcp:127.0.0.1:10001", Connect: "tcp:127.0.0.1:3000"},
			},
			"-",
		},
		{
			"single forward, default same-port",
			[]incus.ProxyDevice{
				{Name: "ahjo-forward-8000", Listen: "tcp:127.0.0.1:8000", Connect: "tcp:10.20.30.1:8000"},
			},
			":8000<-:8000",
		},
		{
			"sorted by container port, remapped host port",
			[]incus.ProxyDevice{
				{Name: "ahjo-forward-9000", Listen: "tcp:127.0.0.1:9000", Connect: "tcp:10.0.0.1:5000"},
				{Name: "ahjo-forward-3000", Listen: "tcp:127.0.0.1:3000", Connect: "tcp:10.0.0.1:8000"},
			},
			":3000<-:8000,:9000<-:5000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatForwards(tt.devices); got != tt.want {
				t.Fatalf("formatForwards=%q want %q", got, tt.want)
			}
		})
	}
}

func TestForwardDevicePorts(t *testing.T) {
	devices := []incus.ProxyDevice{
		{Name: "ahjo-forward-8000", Listen: "tcp:127.0.0.1:8000", Connect: "tcp:10.0.0.1:8000"},
		{Name: "ahjo-forward-3000", Listen: "tcp:127.0.0.1:3000", Connect: "tcp:10.0.0.1:3000"},
		{Name: "ahjo-expose-5000", Listen: "tcp:127.0.0.1:10001", Connect: "tcp:127.0.0.1:5000"},
		{Name: "ahjo-ssh", Listen: "tcp:127.0.0.1:10000", Connect: "tcp:127.0.0.1:22"},
	}
	got := forwardDevicePorts(devices)
	want := map[int]bool{8000: true, 3000: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forwardDevicePorts=%v want %v", got, want)
	}
}

func TestWantedAutoExposePorts(t *testing.T) {
	tests := []struct {
		name      string
		listening []int
		forwards  map[int]bool
		minPort   int
		want      []int
	}{
		{
			"skips ssh, paste, below-min",
			[]int{22, incus.PasteDaemonContainerPort, 100, 3000},
			nil, 3000,
			[]int{3000},
		},
		{
			"excludes a forwarded-in port",
			[]int{3000, 8000},
			map[int]bool{8000: true},
			3000,
			[]int{3000},
		},
		{
			"all listeners forwarded -> nothing to auto-expose",
			[]int{8000},
			map[int]bool{8000: true},
			3000,
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wantedAutoExposePorts(tt.listening, tt.forwards, tt.minPort)
			sort.Ints(got)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("wantedAutoExposePorts=%v want %v", got, tt.want)
			}
		})
	}
}
