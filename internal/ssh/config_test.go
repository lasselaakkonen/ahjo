package ssh

import (
	"os"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

// isolateSharedDir points SharedDir() at a tempdir. SharedDir follows
// $HOME on darwin and standalone Linux, but inside the Lima VM it resolves to
// the virtiofs-mounted Mac home read from /proc/mounts, which no env var can
// redirect — skip there rather than overwrite the user's real ssh-config.
func isolateSharedDir(t *testing.T) {
	t.Helper()
	if mac, ok := paths.MacHostHome(); ok {
		t.Skipf("SharedDir resolves to the Mac host home %s; refusing to write the real ssh-config", mac)
	}
	t.Setenv("HOME", t.TempDir())
}

// hostBlocks splits a rendered ssh-config into "Host <name>" → body lines.
func hostBlocks(t *testing.T, cfg string) map[string][]string {
	t.Helper()
	blocks := map[string][]string{}
	var cur string
	for _, line := range strings.Split(cfg, "\n") {
		if name, ok := strings.CutPrefix(line, "Host "); ok {
			cur = name
			blocks[cur] = nil
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			blocks[cur] = append(blocks[cur], strings.TrimSpace(line))
		}
	}
	return blocks
}

func forwardAgentLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "ForwardAgent ") {
			out = append(out, l)
		}
	}
	return out
}

func TestRegenerateConfig_ForwardAgentPerRepo(t *testing.T) {
	isolateSharedDir(t)
	reg := &registry.Registry{
		Repos: []registry.Repo{
			{Name: "fwd", Aliases: []string{"o/fwd"}, Remote: "git@github.com:o/fwd.git"},
			{Name: "nofwd", Aliases: []string{"o/nofwd"}, Remote: "https://github.com/o/nofwd.git", HasToken: true},
		},
		Branches: []registry.Branch{
			{Repo: "fwd", Slug: "fwd-main", IncusName: "ahjo-fwd", SSHPort: 20001},
			{Repo: "nofwd", Slug: "nofwd-main", IncusName: "ahjo-nofwd", SSHPort: 20002},
			{Repo: "gone", Slug: "gone-main", IncusName: "ahjo-gone", SSHPort: 20003},
		},
	}

	var seenNil bool
	forward := func(r *registry.Repo) bool {
		if r == nil {
			seenNil = true
			return true
		}
		return r.Name == "fwd"
	}

	cases := []struct {
		name    string
		forward func(*registry.Repo) bool
		want    map[string]string
	}{
		{
			name:    "policy func",
			forward: forward,
			want: map[string]string{
				"ahjo-fwd-main":   "ForwardAgent yes",
				"ahjo-nofwd-main": "ForwardAgent no",
				// The func is called with nil for a branch whose repo row is
				// missing; this one forwards on nil, proving the decision is
				// the func's, not hardcoded.
				"ahjo-gone-main": "ForwardAgent yes",
			},
		},
		{
			name: "policy func rejecting nil",
			forward: func(r *registry.Repo) bool {
				return r != nil && r.Name == "fwd"
			},
			want: map[string]string{
				"ahjo-fwd-main":   "ForwardAgent yes",
				"ahjo-nofwd-main": "ForwardAgent no",
				"ahjo-gone-main":  "ForwardAgent no",
			},
		},
		{
			name:    "nil func forwards nothing",
			forward: nil,
			want: map[string]string{
				"ahjo-fwd-main":   "ForwardAgent no",
				"ahjo-nofwd-main": "ForwardAgent no",
				"ahjo-gone-main":  "ForwardAgent no",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := RegenerateConfig(reg, c.forward); err != nil {
				t.Fatalf("RegenerateConfig: %v", err)
			}
			raw, err := os.ReadFile(paths.SSHConfigPath())
			if err != nil {
				t.Fatalf("read ssh-config: %v", err)
			}
			blocks := hostBlocks(t, string(raw))
			if len(blocks) != len(c.want) {
				t.Fatalf("got %d Host blocks, want %d:\n%s", len(blocks), len(c.want), raw)
			}
			for host, want := range c.want {
				lines, ok := blocks[host]
				if !ok {
					t.Fatalf("no Host %s block:\n%s", host, raw)
				}
				got := forwardAgentLines(lines)
				if len(got) != 1 || got[0] != want {
					t.Fatalf("Host %s: ForwardAgent lines = %q, want exactly [%q]", host, got, want)
				}
			}
		})
	}
	if !seenNil {
		t.Fatalf("forwardAgent was never called with nil for the branch whose repo row is missing")
	}
}
