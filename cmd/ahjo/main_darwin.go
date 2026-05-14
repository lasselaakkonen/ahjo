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
	"github.com/lasselaakkonen/ahjo/internal/paste"
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
	case "paste-daemon":
		// Hidden launchd entry point. The plist written by
		// paste.EnsureRunning targets `ahjo paste-daemon`; launchd
		// supervises this process with KeepAlive=true. Errors bubble
		// out so launchd's respawn loop can do its job.
		if err := paste.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "ahjo paste-daemon:", err)
			os.Exit(1)
		}
		return
	case "ssh":
		if len(args) >= 2 {
			execSSHFromMac(args[1])
			return
		}
	case "top":
		if err := runMacTop(); err != nil {
			fmt.Fprintln(os.Stderr, "ahjo:", err)
			os.Exit(1)
		}
		return
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
		// The in-VM mirror has no equivalent cleanliness check (v3 lives off
		// systemd + incus device state), and even if it did it would read
		// through virtiofs and false-positive on file-mode/stat-cache deltas.
		// Mac-side is the only place we can answer the question correctly;
		// `--force` is a Mac-only override and is stripped before relay.
		newArgs, err := preflightMirrorOnMac(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ahjo:", err)
			os.Exit(1)
		}
		args = newArgs
	}

	// Per-repo PAT handling: on macOS we keep PATs in the user's login
	// Keychain instead of as plaintext on the shared disk. The shim is the
	// only writer; the in-VM ahjo reads through GH_TOKEN injected on the
	// relay command line. Errors here are fatal — they only happen when the
	// Keychain is locked or `security` is broken, both of which the user
	// must fix before continuing.
	newArgs, repoEnv, err := interceptRepoSubcommand(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ahjo:", err)
		os.Exit(1)
	}
	args = newArgs

	if err := preflightLima(); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo:", err)
		os.Exit(1)
	}
	if _, err := exec.LookPath("limactl"); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: limactl not on PATH; run `ahjo init` first")
		os.Exit(1)
	}

	// Bring up the host-side paste-daemon (NSPasteboard -> HTTP bridge at
	// 127.0.0.1:18340) before relaying into the VM. Hot path: a 200ms
	// healthz probe returns immediately when launchd already has it
	// running. Cold path: writes ~/Library/LaunchAgents plist and
	// bootstraps the service. Failures are non-fatal — paste image into
	// `claude` won't work, but every other ahjo subcommand still does.
	if err := paste.EnsureRunning(); err != nil {
		fmt.Fprintln(os.Stderr, "warn: paste-daemon:", err)
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
	envPairs := []string{}
	if id, err := git.ResolveHost(); err == nil {
		nameKey, emailKey, sourceKey := git.EnvKeys()
		envPairs = append(envPairs,
			nameKey+"="+id.Name,
			emailKey+"="+id.Email,
			sourceKey+"="+id.Source,
		)
	}
	// Bridge the Mac home so in-VM code paths that copy host-side dotfiles
	// (e.g. pushClaudeConfig sourcing ~/.claude/CLAUDE.md, skills/, agents/)
	// read from the user's actual Mac config rather than the sparse VM
	// home. /Users is reverse-mounted into the VM by Lima's default
	// template, so the same path resolves on both sides without
	// translation. Unset on Linux bare-metal, where os.UserHomeDir() is
	// already the right answer.
	if home, err := os.UserHomeDir(); err == nil {
		envPairs = append(envPairs, "AHJO_HOST_HOME="+home)
	}
	envPairs = append(envPairs, repoEnv...)
	if len(envPairs) > 0 {
		relayPrefix = append(relayPrefix, "env")
		relayPrefix = append(relayPrefix, envPairs...)
	}
	relayArgs := append(relayPrefix, append([]string{"ahjo"}, args...)...)

	// `rm` and `repo rm` need to drop their Keychain rows after the in-VM
	// ahjo decides whether the alias actually removed the repo (true for the
	// default branch, false otherwise). The in-VM ahjo writes a marker file
	// under <SharedDir>/.keychain-cleanup/; we sweep it after relay returns.
	// `lima.Exec` syscall.Exec's into limactl, so for the sweep to run we
	// must use a cmd.Run + propagate-exit pattern instead.
	if needsKeychainSweep(args) {
		cmd := exec.Command("limactl", relayArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		sweepKeychainCleanup()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			fmt.Fprintln(os.Stderr, "ahjo: exec limactl:", err)
			os.Exit(1)
		}
		return
	}
	if err := lima.Exec(relayArgs...); err != nil {
		fmt.Fprintln(os.Stderr, "ahjo: exec limactl:", err)
		os.Exit(1)
	}
}

// needsKeychainSweep reports whether the relay must run as a child (so the
// shim can sweep cleanup markers after it returns) instead of `syscall.Exec`.
// `rm <alias>` may end up removing a repo if alias names the default branch;
// `repo rm <alias>` always does. Either way, the in-VM ahjo writes a marker
// file the shim consumes here.
func needsKeychainSweep(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "rm" {
		return true
	}
	if args[0] == "repo" && len(args) >= 2 && args[1] == "rm" {
		return true
	}
	return false
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
// in the target on Mac, where the user's view is authoritative. If the
// target is dirty, it errors here so the user can commit/stash; `--force`
// is the Mac-only override that bypasses the check. The in-VM mirror has
// no `--force` flag (v3 has no in-VM cleanliness check to skip), so it's
// always stripped before relay.
//
// Returns the (possibly-modified) args to relay. Forms that don't activate
// (off, status, --daemon, no alias arg) are passed through unchanged.
func preflightMirrorOnMac(args []string) ([]string, error) {
	// args[0] == "mirror". Inspect args[1:].
	rest := args[1:]
	var alias, target string
	force := false
	stripped := make([]string, 0, len(args))
	stripped = append(stripped, args[0])
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "off", a == "status":
			return args, nil
		case a == "--daemon":
			return args, nil
		case a == "--force":
			force = true
			// Mac-only flag; do not relay.
			continue
		case a == "--target":
			if i+1 < len(rest) {
				target = rest[i+1]
				stripped = append(stripped, a, rest[i+1])
				i++
				continue
			}
		case strings.HasPrefix(a, "--target="):
			target = strings.TrimPrefix(a, "--target=")
		case strings.HasPrefix(a, "-"):
			// other flags: pass through
		default:
			if alias == "" {
				alias = a
			}
		}
		stripped = append(stripped, a)
	}
	if alias == "" || force || target == "" {
		// No activation, user opted out of the Mac check, or target left to
		// per-repo default (which lives in the in-VM registry — we can't see
		// it from here). Relay without the Mac-only --force.
		return stripped, nil
	}
	target = expandHomeOnMac(target)
	if !filepath.IsAbs(target) {
		return stripped, nil
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		return stripped, nil
	}
	out, err := exec.Command("git", "-C", target, "status", "--porcelain").Output()
	if err != nil {
		// git missing or broken: defer. Best-effort.
		return stripped, nil
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return nil, fmt.Errorf("target %q has uncommitted changes; commit/stash first or pass --force", target)
	}
	return stripped, nil
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
		return fmt.Errorf("homebrew is required to install Lima — install it from https://brew.sh and re-run `ahjo init`")
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
			Title: "Add Include directive for ahjo aliases to ~/.ssh/config",
			Skip: func() (bool, string, error) {
				st, err := sshIncludeStatus()
				if err != nil {
					return false, "", err
				}
				switch st {
				case sshIncludePresent:
					return true, "ahjo-managed Include block already present", nil
				case sshIncludePresentManual:
					return true, "user already maintains an Include line; ahjo will not modify ~/.ssh/config", nil
				}
				return false, "", nil
			},
			Show: "write ahjo-managed `Include ~/.ahjo-shared/ssh-config` block into ~/.ssh/config so system ssh / cursor / vscode resolve `ahjo-<slug>` Host aliases",
			Action: func(out io.Writer) error {
				added, err := ensureSSHInclude()
				if err != nil {
					return err
				}
				if added {
					fmt.Fprintln(out, "  added ahjo-managed Include block to ~/.ssh/config")
				}
				return nil
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
			Post: "\nDone. Try:\n  ahjo doctor\n  ahjo repo add <git-url>           # alias derived from URL, or pass --as <alias>\n  ahjo create <repo-alias> <branch> # auto-aliased <repo-alias>@<branch>, or pass --as <alias>",
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
		fmt.Println("  - unload the paste-daemon launchd agent (~/Library/LaunchAgents/net.ahjo.paste-daemon.plist)")
		switch st, _ := sshIncludeStatus(); st {
		case sshIncludePresent:
			fmt.Println("  - strip the ahjo-managed Include block from ~/.ssh/config")
		case sshIncludePresentManual:
			fmt.Println("  - leave ~/.ssh/config alone (Include line is user-managed, not ours)")
		default:
			fmt.Println("  - skip ~/.ssh/config (no ahjo Include block present)")
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

	fmt.Println("[step] Unload paste-daemon launchd agent")
	if err := paste.Unload(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: paste-daemon unload: %v\n", err)
	} else {
		fmt.Println("[ok]   unloaded")
	}

	fmt.Println("[step] Strip ahjo-managed Include block from ~/.ssh/config")
	if removed, err := removeSSHInclude(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: ssh-include cleanup: %v\n", err)
	} else if removed {
		fmt.Println("[ok]   removed")
	} else {
		fmt.Println("[skip] no ahjo-managed block present")
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
