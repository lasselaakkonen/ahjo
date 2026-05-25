package config

import (
	"testing"

	"github.com/BurntSushi/toml"
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
