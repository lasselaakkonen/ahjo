//go:build darwin

// On macOS, ahjo is a thin shim. It pre-flights the Lima VM and
// `exec`s `limactl shell <vm> ahjo <args...>` so that the user always
// types `ahjo <action>` regardless of which side of the VM they're on.
//
// Two subcommands are handled Mac-side directly:
//   - `ahjo init` — full bring-up: brew/lima/VM, then drops the matching
//     ahjo-linux-<arch> into the VM (resolved locally or fetched from the
//     release that matches this binary's version) and runs the in-VM init
//     transparently.
//   - `ahjo ssh <repo> <branch>` — exec ssh on the Mac, using the generated
//     ssh-config that the in-VM ahjo writes to the Lima 9p mount.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/lasselaakkonen/ahjo/internal/initflow"
)

const (
	vmName     = "ahjo"
	vmAhjoPath = "/usr/local/bin/ahjo"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		os.Exit(0)
	}
	switch args[0] {
	case "help", "--help", "-h":
		printUsage()
		return
	case "version", "--version":
		fmt.Println(version)
		return
	case "init":
		yes := hasFlag(args[1:], "-y", "--yes")
		buildCOI := hasFlag(args[1:], "--build-coi")
		if err := runMacInit(yes, buildCOI); err != nil {
			fmt.Fprintln(os.Stderr, "ahjo:", err)
			os.Exit(1)
		}
		return
	case "nuke":
		yes := hasFlag(args[1:], "-y", "--yes")
		if err := runMacNuke(yes); err != nil {
			fmt.Fprintln(os.Stderr, "ahjo:", err)
			os.Exit(1)
		}
		return
	case "ssh":
		if len(args) >= 3 {
			execSSHFromMac(args[1], args[2])
			return
		}
	}

	if err := preflightLima(); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo:", err)
		os.Exit(1)
	}
	bin, err := exec.LookPath("limactl")
	if err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: limactl not on PATH; run `ahjo init` first")
		os.Exit(1)
	}
	exe := append([]string{"limactl", "shell", vmName, "ahjo"}, args...)
	if err := syscall.Exec(bin, exe, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: exec limactl:", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`ahjo — sandboxed Claude Code branches on Incus, via the %q Lima VM.

Usage:
  ahjo init [--yes] [--build-coi]
                                 one-time setup, host + VM, end-to-end
                                 (--build-coi: build COI from source instead of downloading)
  ahjo nuke [--yes]              tear down the VM + cache; keep ~/.ahjo configs
  ahjo ssh <repo> <branch>       ssh into a worktree (resolves Mac-side via the generated config)
  ahjo <subcommand> [args...]    relayed into the VM and run there
`, vmName)
}

func buildCOIArg(buildCOI bool) string {
	if buildCOI {
		return " --build-coi"
	}
	return ""
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

func preflightLima() error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return fmt.Errorf("limactl not on PATH; run `ahjo init` to install it")
	}
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}} {{.Status}}", vmName).Output()
	if err != nil {
		return fmt.Errorf("limactl list: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return fmt.Errorf("VM %q does not exist; run `ahjo init`", vmName)
	}
	parts := strings.Fields(line)
	if len(parts) < 2 || parts[1] != "Running" {
		return fmt.Errorf("VM %q not running; start it: limactl start %s", vmName, vmName)
	}
	return nil
}

func execSSHFromMac(repo, branch string) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: ssh not on PATH")
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ahjo:", err)
		os.Exit(1)
	}
	cfg := filepath.Join(home, ".ahjo-shared", "ssh-config")
	if _, err := os.Stat(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ahjo: %s missing; run `ahjo new %s %s` first\n", cfg, repo, branch)
		os.Exit(1)
	}
	slug := sanitizeSlug(repo, branch)
	host := "ahjo-" + slug
	if err := syscall.Exec(bin, []string{"ssh", "-F", cfg, host}, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: exec ssh:", err)
		os.Exit(1)
	}
}

func sanitizeSlug(repo, branch string) string {
	b := strings.ToLower(repo + "-" + branch)
	out := make([]rune, 0, len(b))
	for _, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	s := strings.Trim(string(out), "-")
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// runMacInit drives the full host+VM setup. Each step detects its own
// completion so re-runs are idempotent.
func runMacInit(yes, buildCOI bool) error {
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("Homebrew is required to install Lima. Install it from https://brew.sh and re-run `ahjo init`")
	}
	r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
	return r.Execute(macInitSteps(buildCOI))
}

func macInitSteps(buildCOI bool) []initflow.Step {
	// linuxBin is resolved by the "Resolve" step and consumed by the "Install
	// into VM" step. Captured by closure across steps.
	var linuxBin string

	return []initflow.Step{
		{
			Title: "Install Lima",
			Skip: func() (bool, string, error) {
				if _, err := exec.LookPath("limactl"); err == nil {
					return true, "limactl on PATH", nil
				}
				return false, "", nil
			},
			Show: "brew install lima",
			Action: func(out io.Writer) error {
				return initflow.RunShell(out, "", "brew", "install", "lima")
			},
		},
		{
			Title: fmt.Sprintf("Create %q VM (vz + rosetta + writable mount + vzNAT)", vmName),
			Skip: func() (bool, string, error) {
				if vmExists() {
					return true, "VM already exists", nil
				}
				return false, "", nil
			},
			Show: fmt.Sprintf(`limactl start \
  --name=%s --cpus=4 --memory=8 --disk=50 \
  --vm-type=vz --rosetta --mount-writable --network=vzNAT \
  template://ubuntu-lts`, vmName),
			Action: func(out io.Writer) error {
				return initflow.RunShell(out, "",
					"limactl", "start",
					"--name="+vmName,
					"--cpus=4", "--memory=8", "--disk=50",
					"--vm-type=vz", "--rosetta",
					"--mount-writable", "--network=vzNAT",
					"template://ubuntu-lts",
				)
			},
		},
		{
			Title: fmt.Sprintf("Ensure VM %q is running", vmName),
			Skip: func() (bool, string, error) {
				if vmRunning() {
					return true, "VM running", nil
				}
				return false, "", nil
			},
			Show: "limactl start " + vmName,
			Action: func(out io.Writer) error {
				return initflow.RunShell(out, "", "limactl", "start", vmName)
			},
		},
		{
			Title: fmt.Sprintf("Resolve ahjo-linux-%s for the VM", runtime.GOARCH),
			Skip: func() (bool, string, error) {
				if p := resolveLinuxBinaryLocal(version, runtime.GOARCH); p != "" {
					linuxBin = p
					return true, p, nil
				}
				return false, "", nil
			},
			Show: fmt.Sprintf("download ahjo-linux-%s from release %s, verify SHA256, cache under ~/.ahjo/cache/", runtime.GOARCH, version),
			Action: func(out io.Writer) error {
				p, err := downloadLinuxBinary(out, version, runtime.GOARCH)
				if err != nil {
					return err
				}
				linuxBin = p
				return nil
			},
		},
		{
			Title: "Install ahjo into VM at " + vmAhjoPath,
			Skip: func() (bool, string, error) {
				// dev builds always re-install: both sides report "dev" so a
				// version match tells us nothing about whether the bytes match.
				if version == "dev" || version == "" {
					return false, "", nil
				}
				out, err := exec.Command("limactl", "shell", vmName, "--", vmAhjoPath, "version").Output()
				if err != nil {
					return false, "", nil
				}
				if strings.TrimSpace(string(out)) == version {
					return true, "in-VM ahjo " + version + " already installed", nil
				}
				return false, "", nil
			},
			Show: fmt.Sprintf("limactl shell %s -- sudo install -m 0755 /dev/stdin %s   (binary piped from %s)", vmName, vmAhjoPath, "<resolved local copy>"),
			Action: func(out io.Writer) error {
				if linuxBin == "" {
					return fmt.Errorf("internal: linux binary path not set by Resolve step")
				}
				f, err := os.Open(linuxBin)
				if err != nil {
					return err
				}
				defer f.Close()
				cmd := exec.Command("limactl", "shell", vmName, "--",
					"sudo", "install", "-m", "0755", "/dev/stdin", vmAhjoPath)
				cmd.Stdout = out
				cmd.Stderr = out
				cmd.Stdin = f
				return cmd.Run()
			},
		},
		{
			Title: "Run in-VM bring-up (Incus + COI + ahjo-base + claude setup-token)",
			Note:  "interactive only for claude setup-token: it prints a URL — open it in this Mac's browser, complete the flow, paste the code back, then paste the resulting sk-ant-oat01-… token when ahjo asks. The in-VM init detects it's running under Lima and runs COI's installer non-interactively (ufw disabled, NONINTERACTIVE=1) and sets COI to `mode = \"open\"`. After usermod the in-VM init re-execs itself under `sg incus-admin` so everything runs end-to-end in a single call.",
			Show:  fmt.Sprintf("limactl shell %s ahjo init -y%s", vmName, buildCOIArg(buildCOI)),
			Action: func(out io.Writer) error {
				argv := []string{"limactl", "shell", vmName, "ahjo", "init", "-y"}
				if buildCOI {
					argv = append(argv, "--build-coi")
				}
				return initflow.RunShell(out, "", argv...)
			},
			Post: "\nDone. Try:\n  ahjo doctor\n  ahjo repo add <name> <git-url>\n  ahjo new <name> <branch>",
		},
	}
}

func vmExists() bool {
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}}").Output()
	if err != nil {
		return false
	}
	for _, n := range strings.Fields(string(out)) {
		if n == vmName {
			return true
		}
	}
	return false
}

func vmRunning() bool {
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}} {{.Status}}", vmName).Output()
	if err != nil {
		return false
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	return len(parts) >= 2 && parts[1] == "Running"
}

// runMacNuke tears down the Lima VM and the host-side Linux-binary cache.
// It is intentionally narrow: it does not touch ~/.ahjo/{registry.toml,
// config.toml,profiles,repos} so that `ahjo init` can rebuild from scratch
// without losing user-curated state. Pre-existing VMs that ahjo did not
// create (e.g., a leftover "default" from earlier testing) are left alone.
func runMacNuke(yes bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cacheDir := filepath.Join(home, ".ahjo", "cache")
	limactlOnPath := false
	if _, err := exec.LookPath("limactl"); err == nil {
		limactlOnPath = true
	}

	if !yes {
		fmt.Println("ahjo nuke will:")
		switch {
		case !limactlOnPath:
			fmt.Println("  - skip VM tear-down (limactl not on PATH)")
		case vmExists():
			fmt.Printf("  - stop and delete the %q Lima VM (all in-VM ahjo state goes with it)\n", vmName)
		default:
			fmt.Printf("  - skip VM tear-down (no %q VM present)\n", vmName)
		}
		if _, err := os.Stat(cacheDir); err == nil {
			fmt.Printf("  - remove %s\n", cacheDir)
		} else {
			fmt.Printf("  - skip %s (already absent)\n", cacheDir)
		}
		fmt.Println("It will NOT touch ~/.ahjo/{config.toml,registry.toml,profiles,repos}.")
		fmt.Println("Re-run with -y to proceed.")
		return nil
	}

	if !limactlOnPath {
		fmt.Println("[skip] limactl not on PATH; nothing to do for the VM")
	} else if vmExists() {
		fmt.Printf("[step] Stop and delete VM %q\n", vmName)
		fmt.Printf("  > limactl stop -f %s\n", vmName)
		if err := runPassthrough("limactl", "stop", "-f", vmName); err != nil {
			fmt.Fprintf(os.Stderr, "warn: limactl stop: %v\n", err)
		}
		fmt.Printf("  > limactl delete -f %s\n", vmName)
		if err := runPassthrough("limactl", "delete", "-f", vmName); err != nil {
			return fmt.Errorf("limactl delete %s: %w", vmName, err)
		}
		fmt.Println("[ok]   VM removed")
	} else {
		fmt.Printf("[skip] VM %q not present\n", vmName)
	}

	if _, err := os.Stat(cacheDir); err == nil {
		fmt.Printf("[step] Remove %s\n", cacheDir)
		if err := os.RemoveAll(cacheDir); err != nil {
			return fmt.Errorf("remove %s: %w", cacheDir, err)
		}
		fmt.Println("[ok]   removed")
	} else {
		fmt.Printf("[skip] %s already absent\n", cacheDir)
	}

	fmt.Println("\nDone. Run `ahjo init` to rebuild.")
	return nil
}

func runPassthrough(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
