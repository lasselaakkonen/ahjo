//go:build darwin

package cli

import (
	"os/exec"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

// defaultMacHostStatus probes Lima for the ahjo VM state. Best-effort.
func defaultMacHostStatus() top.HostStatus {
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}}\t{{.Status}}\t{{.SSHLocalPort}}").Output()
	if err != nil {
		return top.HostStatus{
			Title: "lima",
			Lines: []string{"limactl unavailable: " + err.Error()},
		}
	}
	var lines []string
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		fields := strings.Split(raw, "\t")
		switch len(fields) {
		case 3:
			lines = append(lines, fields[0]+"  "+fields[1]+"  ssh:"+fields[2])
		case 2:
			lines = append(lines, fields[0]+"  "+fields[1])
		default:
			lines = append(lines, raw)
		}
	}
	if len(lines) == 0 {
		lines = []string{"no Lima VMs"}
	}
	return top.HostStatus{Title: "lima", Lines: lines}
}
