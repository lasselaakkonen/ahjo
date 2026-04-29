package initflow

import "embed"

//go:embed all:assets
var assetsFS embed.FS

// IncusPreseed returns the YAML body for `incus admin init --preseed`.
func IncusPreseed() string {
	b, err := assetsFS.ReadFile("assets/incus-preseed.yaml")
	if err != nil {
		panic(err)
	}
	return string(b)
}

// CoiOpenNetworkConfig returns the body of ~/.coi/config.toml that
// switches COI's network mode to "open" (required on macOS / Lima).
func CoiOpenNetworkConfig() string {
	b, err := assetsFS.ReadFile("assets/coi-config.toml")
	if err != nil {
		panic(err)
	}
	return string(b)
}
