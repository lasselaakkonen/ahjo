//go:build darwin

package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/lima"
)

// Default disk size in GiB. Unlike CPU/RAM there is no useful host figure to
// scale against — the image is sparse and grows on demand, so we just cap it
// generously.
const defaultDiskGiB = 100

// createVMStep returns the init step that provisions the Lima VM. It replaces
// the runner's generic "Run? [y/N]" confirmation (NoConfirm: true) with three
// sizing prompts — vCPUs, memory, and disk — defaulting to a fraction of the
// host's resources. The chosen values are passed to `limactl start`.
//
// On macOS the VM runs under Apple's Virtualization.framework (vz), where
// CPU/RAM are soft upper bounds rather than dedicated reservations and the
// disk image is sparse; createVMStep explains this before prompting so the
// user can comfortably size near their machine's totals.
func createVMStep(yes bool) initflow.Step {
	return initflow.Step{
		Title:     fmt.Sprintf("Create %q VM (vz + rosetta + writable mount + vzNAT)", vmName),
		NoConfirm: true,
		Skip: func() (bool, string, error) {
			if vmExists() {
				return true, "VM already exists", nil
			}
			return false, "", nil
		},
		Show: "size and create the VM with `limactl start` " +
			"(--vm-type=vz --rosetta --mount-writable --network=vzNAT, ssh agent forwarding on)",
		Action: func(out io.Writer) error {
			cpus, mem, disk, err := promptVMSizing(out, yes)
			if err != nil {
				return err
			}
			cmd := []string{
				"limactl", "start",
				"--tty=false",
				"--name=" + vmName,
				fmt.Sprintf("--cpus=%d", cpus),
				fmt.Sprintf("--memory=%d", mem),
				fmt.Sprintf("--disk=%d", disk),
				"--vm-type=vz", "--rosetta",
				"--mount-writable", "--network=vzNAT",
				"--set=.ssh.forwardAgent=true",
				"template:ubuntu-lts",
			}
			fmt.Fprintf(out, "  > limactl start --tty=false \\\n"+
				"  --name=%s --cpus=%d --memory=%d --disk=%d \\\n"+
				"  --vm-type=vz --rosetta --mount-writable --network=vzNAT \\\n"+
				"  --set='.ssh.forwardAgent=true' \\\n"+
				"  template:ubuntu-lts\n",
				vmName, cpus, mem, disk)
			return initflow.RunShellEnv(out, lima.Env(), "", cmd...)
		},
	}
}

// promptVMSizing prints the resource-sizing explanation and reads vCPU,
// memory (GiB), and disk (GiB) from stdin, each defaulting to a fraction of
// the host. With --yes it returns the defaults without prompting.
func promptVMSizing(out io.Writer, yes bool) (cpus, mem, disk int, err error) {
	defCPUs := scalePositive(runtime.NumCPU(), 0.66)
	defMem := scalePositive(hostMemGiB(), 0.50)
	defDisk := defaultDiskGiB

	fmt.Fprintln(out, "  On macOS the VM runs under Apple's vz hypervisor. CPU and memory are")
	fmt.Fprintln(out, "  upper bounds, not dedicated reservations — the VM uses up to this many")
	fmt.Fprintln(out, "  vCPUs and up to this much RAM, and the host reclaims whatever it isn't")
	fmt.Fprintln(out, "  using. The disk image is sparse and grows on demand up to the cap. So")
	fmt.Fprintln(out, "  it's safe to size these near your machine's totals.")

	if yes {
		fmt.Fprintf(out, "  --yes: using %d vCPU, %d GiB RAM, %d GiB disk\n", defCPUs, defMem, defDisk)
		return defCPUs, defMem, defDisk, nil
	}

	in := bufio.NewScanner(os.Stdin)
	if cpus, err = promptInt(out, in, "vCPUs", defCPUs); err != nil {
		return 0, 0, 0, err
	}
	if mem, err = promptInt(out, in, "Memory (GiB)", defMem); err != nil {
		return 0, 0, 0, err
	}
	if disk, err = promptInt(out, in, "Disk (GiB)", defDisk); err != nil {
		return 0, 0, 0, err
	}
	return cpus, mem, disk, nil
}

// promptInt prints "  <label> [<def>]: " and reads one line. Empty input (or
// EOF) yields the default; a non-positive or unparsable value is an error.
func promptInt(out io.Writer, sc *bufio.Scanner, label string, def int) (int, error) {
	fmt.Fprintf(out, "  %s [%d]: ", label, def)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return 0, err
		}
		return def, nil
	}
	s := strings.TrimSpace(sc.Text())
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return v, nil
}

// scalePositive returns round(n*frac), clamped to a minimum of 1.
func scalePositive(n int, frac float64) int {
	v := int(math.Round(float64(n) * frac))
	if v < 1 {
		return 1
	}
	return v
}

// hostMemGiB returns the Mac's total physical RAM in GiB via `sysctl -n
// hw.memsize`. On any failure it falls back to 8 so sizing can still proceed.
func hostMemGiB() int {
	const fallback = 8
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return fallback
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || bytes <= 0 {
		return fallback
	}
	return int(bytes / (1 << 30))
}
