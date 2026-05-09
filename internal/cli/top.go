package cli

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
	"github.com/lasselaakkonen/ahjo/internal/tui/top"
)

func newTopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "top",
		Short: "Open the Miller-columns TUI: repos · worktrees · details",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			deps := top.Deps{
				ResolveContainerName: resolveContainerName,
				FormatExposed:        formatExposed,
				HostStatus:           hostStatusForTop,
				ToggleExpose:         toggleExposeForTop,
			}
			_, err := tea.NewProgram(top.New(deps)).Run()
			return err
		},
	}
}

func hostStatusForTop() top.HostStatus {
	if runtimeIsLinux() {
		return top.HostStatus{
			Title: "host",
			Lines: []string{"linux (native or in-VM)"},
		}
	}
	return defaultMacHostStatus()
}

// toggleExposeForTop flips the branch's container between "all listening
// ports exposed" and "no ports exposed". The state is observed from the
// container's current proxy devices: if any auto- or manual- expose device
// exists, we strip them all; otherwise we add an auto-expose proxy for every
// TCP listener inside the container above the configured min_port (skipping
// SSH).
func toggleExposeForTop(br *registry.Branch) (string, error) {
	release, err := lockfile.Acquire()
	if err != nil {
		return "", err
	}
	defer release()

	containerName, err := resolveContainerName(br)
	if err != nil {
		return "", err
	}

	devices, err := incus.ListProxyDevices(containerName)
	if err != nil {
		return "", err
	}

	var exposed []incus.ProxyDevice
	for _, d := range devices {
		if strings.HasPrefix(d.Name, autoExposeDevicePrefix) ||
			strings.HasPrefix(d.Name, "ahjo-expose-") {
			exposed = append(exposed, d)
		}
	}

	if len(exposed) > 0 {
		return removeAllExposed(containerName, br.Slug, exposed)
	}
	return forceExposeAllListening(containerName, br)
}

// removeAllExposed deletes every auto/manual expose proxy device on the
// container and frees its port allocation. Returns a flash-friendly summary.
func removeAllExposed(containerName, slug string, exposed []incus.ProxyDevice) (string, error) {
	pp, err := ports.Load()
	if err != nil {
		return "", err
	}
	for _, d := range exposed {
		if err := incus.RemoveDevice(containerName, d.Name); err != nil {
			return "", err
		}
		cport, ok := portFromAddr(d.Connect)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(d.Name, autoExposeDevicePrefix):
			pp.FreePurpose(slug, ports.AutoExposePrefix+strconv.Itoa(cport))
		case strings.HasPrefix(d.Name, "ahjo-expose-"):
			pp.FreePurpose(slug, ports.ExposePrefix+strconv.Itoa(cport))
		}
	}
	if err := pp.Save(); err != nil {
		return "", err
	}
	return fmt.Sprintf("unexposed %d port(s)", len(exposed)), nil
}

// forceExposeAllListening mirrors the add-half of reconcileAutoExpose but
// bypasses the global/customizations.ahjo "enabled" check so the user's `e`
// toggle works even when auto-expose is disabled by config.
func forceExposeAllListening(containerName string, br *registry.Branch) (string, error) {
	listening, err := containerListeningPorts(containerName)
	if err != nil {
		return "", fmt.Errorf("scan listening ports: %w", err)
	}

	devices, err := incus.ListProxyDevices(containerName)
	if err != nil {
		return "", err
	}
	have := autoDevicesByPort(devices)

	pp, err := ports.Load()
	if err != nil {
		return "", err
	}

	const minPort = 1024
	added := 0
	for _, cport := range listening {
		if cport == 22 || cport < minPort {
			continue
		}
		if _, exists := have[cport]; exists {
			continue
		}
		hostPort, err := pp.Allocate(br.Slug, ports.AutoExposePrefix+strconv.Itoa(cport))
		if err != nil {
			return "", err
		}
		if err := incus.AddProxyDevice(
			containerName,
			autoExposeDevicePrefix+strconv.Itoa(cport),
			fmt.Sprintf("tcp:127.0.0.1:%d", hostPort),
			fmt.Sprintf("tcp:127.0.0.1:%d", cport),
		); err != nil {
			return "", err
		}
		added++
	}
	if err := pp.Save(); err != nil {
		return "", err
	}
	if added == 0 {
		return "no listening ports to expose", nil
	}
	return fmt.Sprintf("exposed %d port(s)", added), nil
}

