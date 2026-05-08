//go:build !darwin

package cli

import "github.com/lasselaakkonen/ahjo/internal/tui/top"

func defaultMacHostStatus() top.HostStatus {
	return top.HostStatus{
		Title: "host",
		Lines: []string{"non-darwin host"},
	}
}
