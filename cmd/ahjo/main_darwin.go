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
//   - `ahjo ssh <alias>` — exec ssh on the Mac, using the generated
//     ssh-config that the in-VM ahjo writes to the Lima 9p mount and the
//     adjacent alias→slug map.
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

	"github.com/lasselaakkonen/ahjo/internal/git"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/lima"
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
		if err := runMacInit(yes); err != nil {
			fmt.Fprintln(os.Stderr, "ahjo:", err)
			os.Exit(1)
		}
		return
	case "update":
		yes := hasFlag(args[1:], "-y", "--yes")
		if err := runMacUpdate(yes); err != nil {
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
		if len(args) >= 2 {
			execSSHFromMac(args[1])
			return
		}
	case "doctor":
		fix := hasFlag(args[1:], "--fix")
		macFail := runMacDoctor(os.Stdout, fix)
		vmFail := false
		if vmRunning() {
			cmd := lima.Cmd("shell", vmName, "ahjo", "doctor")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				vmFail = true
			}
		}
		if macFail || vmFail {
			os.Exit(1)
		}
		return
	case "mirror":
		// Run the "is the target dir clean?" check on Mac before relaying.
		// The same check inside the VM reads the Mac repo through virtiofs and
		// reports false positives (file mode + stat-cache mismatches), even
		// when `git status` on the Mac shows clean. Mac-side is the source of
		// truth; if it passes we tell the in-VM activate to skip its own check
		// by passing --force.
		newArgs, err := preflightMirrorOnMac(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ahjo:", err)
			os.Exit(1)
		}
		args = newArgs
	}

	if err := preflightLima(); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo:", err)
		os.Exit(1)
	}
	if _, err := exec.LookPath("limactl"); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: limactl not on PATH; run `ahjo init` first")
		os.Exit(1)
	}
	// Bridge the host's git identity into the VM. The in-VM ahjo's
	// `git config --global` and `gh auth status` see a clean Linux user
	// home that nothing else maintains, so VM-side identity-resolution
	// would always come up empty. Resolving here (where `git` and `gh`
	// actually live) and stuffing the result into env vars on the relay
	// command lets `ahjo repo add`'s in-container `git config user.*`
	// seed succeed without nagging the user to dual-maintain identity in
	// the VM. ResolveHost errors are non-fatal at this stage: the
	// in-VM path still falls back to its own resolution and surfaces
	// the same friendly error message if it also comes up empty.
	relayPrefix := []string{"shell", vmName}
	if id, err := git.ResolveHost(); err == nil {
		nameKey, emailKey, sourceKey := git.EnvKeys()
		relayPrefix = append(relayPrefix,
			"env",
			nameKey+"="+id.Name,
			emailKey+"="+id.Email,
			sourceKey+"="+id.Source,
		)
	}
	relayArgs := append(relayPrefix, append([]string{"ahjo"}, args...)...)
	if err := lima.Exec(relayArgs...); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: exec limactl:", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`ahjo — sandboxed Claude Code branches on Incus, via the %q Lima VM.

Usage:
  ahjo init [--yes]              one-time setup, host + VM, end-to-end
  ahjo update [--yes]            push the current ahjo binary into the VM, then refresh
                                 claude + the ahjo-base image (devcontainer Feature
                                 pipeline) inside the VM
  ahjo nuke [--yes]              tear down the VM + cache; keep ~/.ahjo configs
  ahjo ssh <alias>               ssh into a branch by alias (resolves Mac-side via the generated config)
  ahjo <subcommand> [args...]    relayed into the VM and run there
`, vmName)
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

// preflightMirrorOnMac handles the Mac-side cleanliness check for
// `ahjo mirror <alias> --target <path>`. It runs `git status --porcelain`
// in the target on Mac, where the user's view is authoritative. If the target
// is clean, --force is appended so the in-VM activate skips its own check
// (which sees the dir through virtiofs and false-positives on file-mode and
// stat-cache deltas). If the target is dirty, it errors here instead of
// inside the VM, where the message would be muddled by virtiofs noise.
//
// Returns the (possibly-modified) args to relay. Forms that don't activate
// (off, status, --daemon, no alias arg) are passed through unchanged.
func preflightMirrorOnMac(args []string) ([]string, error) {
	// args[0] == "mirror". Inspect args[1:].
	rest := args[1:]
	var alias, target string
	force := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "off", a == "status":
			return args, nil
		case a == "--daemon":
			return args, nil
		case a == "--force":
			force = true
		case a == "--target":
			if i+1 < len(rest) {
				target = rest[i+1]
				i++
			}
		case strings.HasPrefix(a, "--target="):
			target = strings.TrimPrefix(a, "--target=")
		case strings.HasPrefix(a, "-"):
			// other flags: ignore
		default:
			if alias == "" {
				alias = a
			}
		}
	}
	if alias == "" || force || target == "" {
		// No activation, or user already passed --force, or target left to
		// per-repo default (which lives in the in-VM registry — we can't see
		// it from here). Defer to the in-VM check.
		return args, nil
	}
	target = expandHomeOnMac(target)
	if !filepath.IsAbs(target) {
		return args, nil
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		return args, nil
	}
	out, err := exec.Command("git", "-C", target, "status", "--porcelain").Output()
	if err != nil {
		// git missing or broken: defer to in-VM. Best-effort.
		return args, nil
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return nil, fmt.Errorf("target %q has uncommitted changes; commit/stash first or pass --force", target)
	}
	// Clean per Mac. Append --force so the in-VM check (which would
	// false-positive over virtiofs) is skipped.
	return append(args, "--force"), nil
}

func expandHomeOnMac(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
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

func execSSHFromMac(alias string) {
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
	shared := filepath.Join(home, ".ahjo-shared")
	cfg := filepath.Join(shared, "ssh-config")
	if _, err := os.Stat(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ahjo: %s: %v\n", cfg, err)
		os.Exit(1)
	}
	slug, err := lookupAlias(filepath.Join(shared, "aliases"), alias)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ahjo:", err)
		os.Exit(1)
	}
	host := "ahjo-" + slug
	if err := syscall.Exec(bin, []string{"ssh", "-F", cfg, host}, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: exec ssh:", err)
		os.Exit(1)
	}
}

// lookupAlias scans the alias→slug map (one "alias\tslug" per line) and
// returns the slug for alias. Lines starting with # are ignored.
func lookupAlias(path, alias string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] == alias {
			return parts[1], nil
		}
	}
	return "", fmt.Errorf("no worktree with alias %q (try `ahjo ls` to see aliases)", alias)
}

// runMacInit drives the full host+VM setup. Each step detects its own
// completion so re-runs are idempotent.
func runMacInit(yes bool) error {
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("Homebrew is required to install Lima. Install it from https://brew.sh and re-run `ahjo init`")
	}
	r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
	return r.Execute(macInitSteps(yes))
}

// runMacUpdate is the macOS half of `ahjo update`: refresh the in-VM ahjo
// binary (skipping the push if the VM already runs the same tagged version),
// then relay `ahjo update` into the VM so it refreshes claude + the
// ahjo-base image via the devcontainer Feature pipeline.
func runMacUpdate(yes bool) error {
	if _, err := exec.LookPath("limactl"); err != nil {
		return fmt.Errorf("limactl not on PATH; run `ahjo init` first")
	}
	if !vmExists() {
		return fmt.Errorf("VM %q does not exist; run `ahjo init` first", vmName)
	}
	r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
	return r.Execute(macUpdateSteps())
}

// macUpdateSteps reuses init's "Ensure VM running" + binary-resolve +
// binary-install steps, then relays `ahjo update -y` into the VM so the
// in-VM stack (claude + ahjo-base image) gets refreshed.
func macUpdateSteps() []initflow.Step {
	var linuxBin string

	return []initflow.Step{
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
				return initflow.RunShellEnv(out, lima.Env(), "", "limactl", "start", vmName)
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
				// dev/dirty builds always re-install (see init for rationale).
				if version == "dev" || version == "" || strings.HasSuffix(version, "-dirty") {
					return false, "", nil
				}
				out, err := lima.Cmd("shell", vmName, "--", vmAhjoPath, "version").Output()
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
				cmd := lima.Cmd("shell", vmName, "--",
					"sudo", "install", "-m", "0755", "/dev/stdin", vmAhjoPath)
				cmd.Stdout = out
				cmd.Stderr = out
				cmd.Stdin = f
				return cmd.Run()
			},
		},
		{
			// The in-VM `ahjo update` step prints its own Post message
			// describing the next step (`ahjo shell <alias> --update`),
			// and that output bubbles up through limactl. We deliberately
			// don't add another Post here so the user sees it only once.
			Title: "Refresh in-VM stack (claude + ahjo-base via the devcontainer Feature pipeline)",
			Show:  fmt.Sprintf("limactl shell %s ahjo update -y", vmName),
			Action: func(out io.Writer) error {
				argv := []string{"limactl", "shell", vmName, "ahjo", "update", "-y"}
				if err := initflow.RunShellEnv(out, lima.Env(), "", argv...); err != nil {
					return err
				}
				// Defensive symmetry with macInitSteps: refresh the lima ssh
				// ControlMaster so users whose master was opened before
				// `ahjo init`'s usermod (and therefore inherits stale
				// supplementary groups) recover by running `ahjo update`.
				if err := lima.CloseSSHControlMaster(vmName); err != nil {
					fmt.Fprintf(out, "  warn: could not close existing ssh master (%v); a `limactl stop %s && limactl start %s` may be needed\n", err, vmName, vmName)
				}
				return nil
			},
		},
	}
}

func macInitSteps(yes bool) []initflow.Step {
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
		pickAgentStep(yes),
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
  --set='.ssh.forwardAgent=true' \
  template://ubuntu-lts`, vmName),
			Action: func(out io.Writer) error {
				return initflow.RunShellEnv(out, lima.Env(), "",
					"limactl", "start",
					"--name="+vmName,
					"--cpus=4", "--memory=8", "--disk=50",
					"--vm-type=vz", "--rosetta",
					"--mount-writable", "--network=vzNAT",
					"--set=.ssh.forwardAgent=true",
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
				return initflow.RunShellEnv(out, lima.Env(), "", "limactl", "start", vmName)
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
				// dev/dirty builds always re-install: same version string can
				// cover different bytes (two builds on the same dirty commit
				// stamp identical "<sha>-dirty"), so a version match tells us
				// nothing about whether the bytes are current.
				if version == "dev" || version == "" || strings.HasSuffix(version, "-dirty") {
					return false, "", nil
				}
				out, err := lima.Cmd("shell", vmName, "--", vmAhjoPath, "version").Output()
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
				cmd := lima.Cmd("shell", vmName, "--",
					"sudo", "install", "-m", "0755", "/dev/stdin", vmAhjoPath)
				cmd.Stdout = out
				cmd.Stderr = out
				cmd.Stdin = f
				return cmd.Run()
			},
		},
		{
			Title: "Run in-VM bring-up (Incus + ahjo-base via the devcontainer Feature pipeline + claude setup-token)",
			Note:  "interactive only for claude setup-token: it prints a URL — open it in this Mac's browser, complete the flow, paste the code back, then paste the resulting sk-ant-oat01-… token when ahjo asks. After usermod the in-VM init re-execs itself under `sg incus-admin` so everything runs end-to-end in a single call.",
			Show:  fmt.Sprintf("limactl shell %s ahjo init -y", vmName),
			Action: func(out io.Writer) error {
				argv := []string{"limactl", "shell", vmName, "ahjo", "init", "-y"}
				if err := initflow.RunShellEnv(out, lima.Env(), "", argv...); err != nil {
					return err
				}
				// In-VM init added the user to incus-admin. Close the lima ssh
				// ControlMaster so the next limactl shell re-authenticates and
				// PAM activates the new supplementary group — same dynamic
				// CloseSSHControlMaster handles for SSH_AUTH_SOCK after agent
				// changes (see saveAgentChoice in agent_step_darwin.go).
				if err := lima.CloseSSHControlMaster(vmName); err != nil {
					fmt.Fprintf(out, "  warn: could not close existing ssh master (%v); a `limactl stop %s && limactl start %s` may be needed\n", err, vmName, vmName)
				}
				return nil
			},
			Post: "\nDone. Try:\n  ahjo doctor\n  ahjo repo add <git-url>           # alias derived from URL, or pass --as <alias>\n  ahjo new <repo-alias> <branch>    # auto-aliased <repo-alias>@<branch>, or pass --as <alias>",
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
