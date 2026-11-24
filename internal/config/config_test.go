package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/lasselaakkonen/ahjo/internal/paths"
)

// forward_ssh_agent is a *bool so an absent key stays nil (auto), distinct
// from an explicit false. Load() must not force a default for it; this guards
// that nil-survival by unmarshalling over a defaults() base directly.
func TestForwardSSHAgentParse(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	cases := []struct {
		name string
		toml string
		want *bool // nil = expect nil
	}{
		{"absent stays nil", "version = 1\n", nil},
		{"explicit true", "version = 1\nforward_ssh_agent = true\n", boolPtr(true)},
		{"explicit false", "version = 1\nforward_ssh_agent = false\n", boolPtr(false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaults()
			if err := toml.Unmarshal([]byte(c.toml), cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := cfg.ForwardSSHAgent
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("ForwardSSHAgent = %v, want nil", *got)
			case c.want != nil && got == nil:
				t.Fatalf("ForwardSSHAgent = nil, want %v", *c.want)
			case c.want != nil && got != nil && *got != *c.want:
				t.Fatalf("ForwardSSHAgent = %v, want %v", *got, *c.want)
			}
		})
	}
}

// loadTOML writes body as ~/.ahjo/config.toml under a temp HOME and calls Load.
func loadTOML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.ConfigPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load()
}

func TestNetworkAllowCIDRs(t *testing.T) {
	t.Run("valid list loads", func(t *testing.T) {
		cfg, err := loadTOML(t, "version = 1\n[network]\nallow_cidrs = [\"10.1.0.0/16\", \"192.168.5.7/32\", \"10.0.0.0/8\"]\n")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := []string{"10.1.0.0/16", "192.168.5.7/32", "10.0.0.0/8"}
		if !reflect.DeepEqual(cfg.Network.AllowCIDRs, want) {
			t.Fatalf("AllowCIDRs = %v, want %v", cfg.Network.AllowCIDRs, want)
		}
	})
	t.Run("absent section is empty", func(t *testing.T) {
		cfg, err := loadTOML(t, "version = 1\n")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Network.AllowCIDRs) != 0 {
			t.Fatalf("AllowCIDRs = %v, want empty", cfg.Network.AllowCIDRs)
		}
	})
	for _, c := range []struct{ name, cidr string }{
		{"invalid CIDR", "not-a-cidr"},
		{"missing prefix length", "10.1.0.0"},
		{"IPv6", "fd00::/8"},
		{"IPv4-mapped IPv6", "::ffff:10.0.0.0/104"},
		{"match-all", "0.0.0.0/0"},
		{"too wide", "10.0.0.0/7"},
	} {
		t.Run("rejects "+c.name, func(t *testing.T) {
			_, err := loadTOML(t, "version = 1\n[network]\nallow_cidrs = [\""+c.cidr+"\"]\n")
			if err == nil {
				t.Fatalf("Load accepted %q", c.cidr)
			}
			if !strings.Contains(err.Error(), "network.allow_cidrs") || !strings.Contains(err.Error(), c.cidr) {
				t.Fatalf("error %q should name network.allow_cidrs and %q", err, c.cidr)
			}
		})
	}
}

// An empty [network] section must not change what Save writes for existing
// users who never set it.
func TestSaveOmitsEmptyAllowCIDRs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := defaults().Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(paths.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "allow_cidrs") {
		t.Fatalf("saved config mentions allow_cidrs:\n%s", b)
	}
}
