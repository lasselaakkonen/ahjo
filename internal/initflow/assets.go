package initflow

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed all:assets
var assetsFS embed.FS

// IncusPreseed returns the YAML body for `incus admin init --preseed`, with
// the incusbr0 gateway interpolated from the picked CIDR (e.g. "10.20.30.1/24").
// The CIDR is chosen by PickGatewayCIDR so nested ahjo-in-ahjo containers
// don't collide with the outer bridge's subnet.
func IncusPreseed(gatewayCIDR string) string {
	b, err := assetsFS.ReadFile("assets/incus-preseed.yaml.tmpl")
	if err != nil {
		panic(err)
	}
	t, err := template.New("incus-preseed").Parse(string(b))
	if err != nil {
		panic(fmt.Errorf("parse preseed template: %w", err))
	}
	var sb strings.Builder
	if err := t.Execute(&sb, struct{ GatewayCIDR string }{gatewayCIDR}); err != nil {
		panic(fmt.Errorf("execute preseed template: %w", err))
	}
	return sb.String()
}
