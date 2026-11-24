package preflight

import (
	"errors"
	"strings"
	"testing"

	"github.com/lasselaakkonen/ahjo/internal/incus"
)

func TestNetworkHardeningProblem(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		p := networkHardeningProblem(incus.NetworkHardeningStatus{EgressFiltered: true, PortIsolated: true}, nil)
		if p.Severity != OK || p.Fix != "" {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("not hardened", func(t *testing.T) {
		p := networkHardeningProblem(incus.NetworkHardeningStatus{PortIsolated: true, Reason: "no egress ACL"}, nil)
		if p.Severity != Warn || p.Fix != "ahjo update" || !strings.Contains(p.Detail, "no egress ACL") {
			t.Fatalf("got %+v", p)
		}
	})
	t.Run("error", func(t *testing.T) {
		// Status would read OK; the error must still win.
		p := networkHardeningProblem(incus.NetworkHardeningStatus{EgressFiltered: true, PortIsolated: true}, errors.New("boom"))
		if p.Severity != Warn || p.Detail != "boom" {
			t.Fatalf("got %+v", p)
		}
	})
}

func TestPersistedTokenProblem(t *testing.T) {
	const secret = "ghp_s3cr3tTokenValue"
	cases := []struct {
		name        string
		gh, github  string
		wantProblem bool
	}{
		{"both empty", "", "", false},
		{"GH_TOKEN set", secret, "", true},
		{"GITHUB_TOKEN set", "", secret, true},
		{"both set", secret, secret, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ok := persistedTokenProblem("ahjo-o-r", "o/r", c.gh, c.github)
			if ok != c.wantProblem {
				t.Fatalf("ok = %v, want %v", ok, c.wantProblem)
			}
			if !ok {
				return
			}
			if p.Severity != Warn {
				t.Errorf("severity = %v, want Warn", p.Severity)
			}
			if !strings.Contains(p.Title, "ahjo-o-r") {
				t.Errorf("title %q does not name the container", p.Title)
			}
			if !strings.HasPrefix(p.Fix, "ahjo repo set-token o/r") {
				t.Errorf("fix %q does not name the alias", p.Fix)
			}
			for field, v := range map[string]string{"Title": p.Title, "Detail": p.Detail, "Fix": p.Fix} {
				if strings.Contains(v, secret) {
					t.Errorf("%s leaks the token value: %q", field, v)
				}
			}
		})
	}
}
