package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/config"
	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

const autoExposeDevicePrefix = "ahjo-auto-"

// reconcileAutoExpose makes the set of `ahjo-auto-*` proxy devices on the
// container match the set of TCP loopback listeners inside it (above the
// configured min_port threshold).
//
// Listening ports below the threshold and SSH (22) are ignored. Manual
// ahjo-expose-* devices are never touched.
//
// Idempotent: safe to call repeatedly. Reads + writes ~/.ahjo/ports.toml so
// callers should hold the ahjo lockfile when invoking this from a write
// context (e.g. ahjo expose --sync).
func reconcileAutoExpose(out io.Writer, br *registry.Branch) error {
	gcfg, err := config.Load()
	if err != nil {
		return err
	}
	containerName, err := resolveContainerName(br)
	if err != nil {
		return err
	}
	enabled, minPort := autoExposeSettings(gcfg, containerName)

	devices, err := incus.ListProxyDevices(containerName)
	if err != nil {
		return err
	}
	have := autoDevicesByPort(devices)

	want := map[int]struct{}{}
	if enabled {
		listening, err := containerListeningPorts(containerName)
		if err != nil {
			return fmt.Errorf("scan listening ports: %w", err)
		}
		for _, p := range listening {
			if p == 22 || p < minPort {
				continue
			}
			want[p] = struct{}{}
		}
	}

	pp, err := ports.Load()
	if err != nil {
		return err
	}
	dirty := false

	for cport := range have {
		if _, keep := want[cport]; keep {
			continue
		}
		if err := incus.RemoveDevice(containerName, autoExposeDevicePrefix+strconv.Itoa(cport)); err != nil {
			return err
		}
		pp.FreePurpose(br.Slug, ports.AutoExposePrefix+strconv.Itoa(cport))
		dirty = true
		fmt.Fprintf(out, "auto-expose: drop container :%d\n", cport)
	}

	wantSorted := make([]int, 0, len(want))
	for p := range want {
		wantSorted = append(wantSorted, p)
	}
	sort.Ints(wantSorted)
	for _, cport := range wantSorted {
		if _, exists := have[cport]; exists {
			continue
		}
		hostPort, err := pp.Allocate(br.Slug, ports.AutoExposePrefix+strconv.Itoa(cport))
		if err != nil {
			return err
		}
		dirty = true
		if err := incus.AddProxyDevice(
			containerName,
			autoExposeDevicePrefix+strconv.Itoa(cport),
			fmt.Sprintf("tcp:127.0.0.1:%d", hostPort),
			fmt.Sprintf("tcp:127.0.0.1:%d", cport),
		); err != nil {
			return err
		}
		fmt.Fprintf(out, "auto-expose: container :%d -> 127.0.0.1:%d\n", cport, hostPort)
	}

	if dirty {
		if err := pp.Save(); err != nil {
			return err
		}
	}
	return nil
}

// autoExposeSettings folds the global ~/.ahjo/config.toml [auto_expose]
// section together with any per-repo `customizations.ahjo.auto_expose`
// override declared in /repo/.devcontainer/devcontainer.json.
func autoExposeSettings(gcfg *config.Config, containerName string) (enabled bool, minPort int) {
	enabled = gcfg.AutoExpose.Enabled == nil || *gcfg.AutoExpose.Enabled
	minPort = gcfg.AutoExpose.MinPort
	if minPort == 0 {
		minPort = config.DefaultAutoExposeMinPort
	}
	dcConf, _, _ := devcontainer.LoadFromContainer(containerName)
	if dcConf != nil {
		ax := dcConf.Customizations.Ahjo.AutoExpose
		if ax.Enabled != nil {
			enabled = *ax.Enabled
		}
		if ax.MinPort != nil {
			minPort = *ax.MinPort
		}
	}
	return
}

// autoDevicesByPort returns just the auto-expose proxy devices keyed by the
// container port they connect to.
func autoDevicesByPort(devices []incus.ProxyDevice) map[int]incus.ProxyDevice {
	out := map[int]incus.ProxyDevice{}
	for _, d := range devices {
		if !strings.HasPrefix(d.Name, autoExposeDevicePrefix) {
			continue
		}
		cport, ok := portFromAddr(d.Connect)
		if !ok {
			continue
		}
		out[cport] = d
	}
	return out
}

// containerListeningPorts runs `ss -tlnH` inside the container and returns
// the set of TCP ports it reports as LISTEN (numeric, no headers).
func containerListeningPorts(containerName string) ([]int, error) {
	out, err := incus.Exec(containerName, "ss", "-tlnH")
	if err != nil {
		return nil, err
	}
	var found []int
	seen := map[int]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// `ss -tlnH` columns: State Recv-Q Send-Q Local-Address:Port Peer-Address:Port [Process]
		port, ok := portFromAddr(fields[3])
		if !ok {
			continue
		}
		if _, dup := seen[port]; dup {
			continue
		}
		seen[port] = struct{}{}
		found = append(found, port)
	}
	return found, nil
}

// portFromAddr extracts the numeric port from "tcp:127.0.0.1:8080",
// "127.0.0.1:8080", "[::]:80", or "*:443". Returns (0, false) on any failure.
func portFromAddr(addr string) (int, bool) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 || i == len(addr)-1 {
		return 0, false
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}
