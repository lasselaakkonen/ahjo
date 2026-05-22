package incus

import (
	"errors"
	"reflect"
	"testing"
)

func TestReverseProxyArgs(t *testing.T) {
	got := reverseProxyArgs("c1", "ahjo-forward-3000", 3000, "10.20.30.1", 8000)
	want := []string{
		"config", "device", "add", "c1", "ahjo-forward-3000", "proxy",
		"listen=tcp:127.0.0.1:3000",
		"connect=tcp:10.20.30.1:8000",
		"bind=container",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reverseProxyArgs:\n got %q\nwant %q", got, want)
	}
}

// TestReverseProxyArgs_PasteDaemonShape guards the consolidation: the
// refactored EnsurePasteDaemonProxy must still produce the exact argv it built
// by hand before — listen/connect on 18340 with bind=container.
func TestReverseProxyArgs_PasteDaemonShape(t *testing.T) {
	got := reverseProxyArgs("c1", pasteDaemonProxyDevice, PasteDaemonContainerPort, "192.168.5.2", PasteDaemonContainerPort)
	want := []string{
		"config", "device", "add", "c1", "ahjo-paste-daemon", "proxy",
		"listen=tcp:127.0.0.1:18340",
		"connect=tcp:192.168.5.2:18340",
		"bind=container",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paste-daemon argv drifted:\n got %q\nwant %q", got, want)
	}
}

func TestReverseConnectIP(t *testing.T) {
	tests := []struct {
		name    string
		resolve func() (string, error)
		want    string
	}{
		{"lima resolves -> gateway ip", func() (string, error) { return "192.168.5.2", nil }, "192.168.5.2"},
		{"native linux -> loopback fallback", func() (string, error) { return "", errors.New("no such host") }, "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reverseConnectIP(tt.resolve)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("reverseConnectIP=%q want %q", got, tt.want)
			}
		})
	}
}
