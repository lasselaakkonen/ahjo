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
