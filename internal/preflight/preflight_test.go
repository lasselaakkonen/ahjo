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
