//go:build linux

package paths

import (
	"bufio"
	"os"
	"strings"
)

// MacHostHome reports the Mac host home as visible inside the Lima VM via
// virtiofs, if any. Returns ("", false) on standalone Linux (no such mount).
func MacHostHome() (string, bool) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Fields(s.Text())
		if len(p) >= 3 && p[2] == "virtiofs" && strings.HasPrefix(p[1], "/Users/") {
			return p[1], true
		}
	}
	return "", false
}
