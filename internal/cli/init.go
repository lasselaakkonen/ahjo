package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/coi"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/lima"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/profile"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

func newInitCmd() *cobra.Command {
	var yes bool
	var buildCOI bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "One-time setup: Incus, COI, ahjo-base image, ~/.ahjo/ skeleton, claude setup-token",
		Long: `init walks through the verified bring-up sequence step by step:

  1. Zabbly apt signing key + sources list
  2. apt install incus
  3. incus admin init via preseed (fixed subnet 10.20.30.1/24)
  4. usermod -aG incus-admin (re-exec under sg, no re-shell required)
  5. install COI (under Lima: non-interactive — ufw disabled, NONINTERACTIVE=1; on bare-metal Linux: interactive, you pick ufw/firewalld). Pre-built COI binary by default; pass --build-coi to build from source instead.
  6. configure ~/.coi/config.toml for open networking (required on macOS)
  7. coi build (builds coi-default image, ~5 min)
  8. ahjo-base image (extends coi-default with sshd)
  9. create ~/.ahjo/ skeleton
 10. install Claude Code if missing (curl -fsSL https://claude.ai/install.sh | bash — Anthropic's native installer, auto-updating)
 11. claude setup-token (saves token to ~/.ahjo/.env)

Each step detects whether it's already done and skips, so re-runs are safe.
The flow runs end-to-end in a single invocation.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !insideLinuxVM() {
				return fmt.Errorf("the in-VM phase of `ahjo init` only runs on Linux; on macOS the same `ahjo init` first brings up the Lima VM and tells you to enter it before re-running")
			}
			r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
			return r.Execute(vmInitSteps(yes, buildCOI))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip per-step confirmation prompts")
	cmd.Flags().BoolVar(&buildCOI, "build-coi", false, "build COI from source instead of downloading the pre-built binary")
	return cmd
}

func insideLinuxVM() bool {
	// Heuristic: we run on linux. ahjo-mac handles the macOS pre-VM phase.
	return runtimeIsLinux()
}

func vmInitSteps(yes, buildCOI bool) []initflow.Step {
	username := currentUsername()
	onLima := lima.IsGuest()
	steps := []initflow.Step{
		{
			Title: "Install Zabbly apt signing key",
			Skip: func() (bool, string, error) {
				if _, err := os.Stat("/etc/apt/keyrings/zabbly.asc"); err == nil {
					return true, "/etc/apt/keyrings/zabbly.asc present", nil
				}
				return false, "", nil
			},
			Show: "sudo install -d -m 0755 /etc/apt/keyrings\n" +
				"sudo curl -fsSL https://pkgs.zabbly.com/key.asc -o /etc/apt/keyrings/zabbly.asc",
			Action: func(out io.Writer) error {
				if err := initflow.RunShell(out, "", "sudo", "install", "-d", "-m", "0755", "/etc/apt/keyrings"); err != nil {
					return err
				}
				return initflow.RunShell(out, "",
					"sudo", "curl", "-fsSL", "https://pkgs.zabbly.com/key.asc",
					"-o", "/etc/apt/keyrings/zabbly.asc")
			},
		},
		{
			Title: "Add Zabbly Incus stable repo",
			Skip: func() (bool, string, error) {
				if _, err := os.Stat("/etc/apt/sources.list.d/zabbly-incus-stable.sources"); err == nil {
					return true, "sources file present", nil
				}
				return false, "", nil
			},
			Show: "writes /etc/apt/sources.list.d/zabbly-incus-stable.sources\n" +
				"(Suites: noble — Zabbly only builds for LTS codenames)",
			Action: func(out io.Writer) error {
				arch, err := exec.Command("dpkg", "--print-architecture").Output()
				if err != nil {
					return fmt.Errorf("dpkg --print-architecture: %w", err)
				}
				body := fmt.Sprintf(`Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: noble
Components: main
Architectures: %s
Signed-By: /etc/apt/keyrings/zabbly.asc
`, strings.TrimSpace(string(arch)))
				return initflow.RunBash(out, body,
					"sudo tee /etc/apt/sources.list.d/zabbly-incus-stable.sources >/dev/null")
			},
		},
		{
			Title: "apt-get update && install incus",
			Skip: func() (bool, string, error) {
				if _, err := exec.LookPath("incus"); err == nil {
					return true, "incus already on PATH", nil
				}
				return false, "", nil
			},
			Show: "sudo apt-get update\nsudo apt-get install -y incus",
			Action: func(out io.Writer) error {
				if err := initflow.RunShell(out, "", "sudo", "apt-get", "update"); err != nil {
					return err
				}
				return initflow.RunShell(out, "", "sudo", "apt-get", "install", "-y", "incus")
			},
		},
		{
			Title: "Initialize Incus with explicit subnet (preseed)",
			Skip: func() (bool, string, error) {
				if err := exec.Command("sudo", "incus", "network", "show", "incusbr0").Run(); err == nil {
					return true, "incusbr0 already configured", nil
				}
				return false, "", nil
			},
			Note: "auto-init fails inside Lima because vzNAT/Rosetta routes collide; we use a fixed 10.20.30.1/24 subnet",
			Show: "echo '<preseed>' | sudo incus admin init --preseed",
			Action: func(out io.Writer) error {
				return initflow.RunShell(out, initflow.IncusPreseed(),
					"sudo", "incus", "admin", "init", "--preseed")
			},
		},
		{
			Title: "Join incus-admin group",
			Skip: func() (bool, string, error) {
				out, err := exec.Command("id", "-nG").Output()
				if err != nil {
					return false, "", err
				}
				if hasGroup(string(out), "incus-admin") {
					return true, "already in incus-admin group", nil
				}
				return false, "", nil
			},
			Note: "after usermod ahjo re-execs itself under `sg incus-admin` so the rest of init picks up the new group without re-shelling",
			Show: fmt.Sprintf("sudo usermod -aG incus-admin %s\nexec sg incus-admin -c \"ahjo init -y\"", username),
			Action: func(out io.Writer) error {
				if err := initflow.RunShell(out, "", "sudo", "usermod", "-aG", "incus-admin", username); err != nil {
					return err
				}
				return reExecUnderSg(out, yes)
			},
		},
	}
	steps = append(steps, coiInstallSteps(onLima, buildCOI)...)
	steps = append(steps, []initflow.Step{
		{
			Title: "Build coi-default image (~5 min on first run)",
			Skip: func() (bool, string, error) {
				exists, err := incus.ImageAliasExists("coi-default")
				if err != nil {
					return false, "", err
				}
				if exists {
					return true, "coi-default image present", nil
				}
				return false, "", nil
			},
			Note: "applies an in-flight workaround: COI's default build.sh runs `mise use --global ... pnpm@latest ...`, but mise's aqua backend can't fetch pnpm@latest because aqua-registry expects asset name pnpm-linux-arm64 while pnpm v11 only publishes pnpm-linux-arm64.tar.gz. ahjo fetches COI's build.sh, rewrites pnpm@latest → npm:pnpm@latest, drops it into a CWD-relative profiles/default/build.sh (which COI prefers over its embedded copy), and runs `coi build` from there. Drop this when aqua-registry's pnpm mapping catches up.",
			Show: "patch profiles/default/build.sh in a temp dir (mise pnpm@latest → npm:pnpm@latest), then `coi build` with that as CWD",
			Action: func(out io.Writer) error {
				return buildCOIDefaultWithMisePatch(out)
			},
		},
		{
			Title: "Materialize ahjo-base profile and build the ahjo-base image",
			Skip: func() (bool, string, error) {
				exists, err := incus.ImageAliasExists("ahjo-base")
				if err != nil {
					return false, "", err
				}
				if exists {
					return true, "ahjo-base image present", nil
				}
				return false, "", nil
			},
			Show: "writes ~/.ahjo/profiles/ahjo-base/{config.toml,build.sh}\n" +
				"mirrors them as file-level symlinks under ~/.coi/profiles/ahjo-base/ (COI's loader skips dir-symlinks)\n" +
				"coi build --profile ahjo-base",
			Action: func(out io.Writer) error {
				if err := paths.EnsureSkeleton(); err != nil {
					return err
				}
				if err := profile.Materialize(); err != nil {
					return err
				}
				return coi.Build(paths.AhjoBaseProfile, false)
			},
		},
		{
			Title: "Ensure ~/.ahjo/ skeleton",
			Skip: func() (bool, string, error) {
				if _, err := os.Stat(paths.RegistryPath()); err == nil {
					return true, "registry.toml present", nil
				}
				if _, err := os.Stat(paths.AhjoDir()); err == nil {
					return true, paths.AhjoDir() + " exists", nil
				}
				return false, "", nil
			},
			Show: "creates ~/.ahjo/{repos,worktrees,host-keys,profiles,shared}",
			Action: func(_ io.Writer) error {
				return paths.EnsureSkeleton()
			},
		},
		{
			Title: "Install Claude Code (curl -fsSL https://claude.ai/install.sh | bash)",
			Skip: func() (bool, string, error) {
				if claudeOnPath() {
					return true, "claude already resolves under `bash -l`", nil
				}
				return false, "", nil
			},
			Note: "ahjo is Claude+GitHub-only, so `claude` is required. ahjo-base intentionally doesn't embed it. Uses Anthropic's native installer (per https://code.claude.com/docs/en/quickstart) — installs to ~/.local/bin/claude and auto-updates in the background. We run via `bash -lc` so the post-install PATH (which the installer prepends ~/.local/bin to via shell rc) is honored.",
			Show: "bash -lc 'curl -fsSL https://claude.ai/install.sh | bash'",
			Action: func(out io.Writer) error {
				if err := initflow.RunShell(out, "", "bash", "-lc", "curl -fsSL https://claude.ai/install.sh | bash"); err != nil {
					return fmt.Errorf("install.sh: %w", err)
				}
				if !claudeOnPath() {
					return fmt.Errorf("claude still not on PATH after install — the installer likely added ~/.local/bin via shell rc, but `bash -lc` could not find it; open a fresh shell and re-run `ahjo init`")
				}
				return nil
			},
		},
		{
			Title: "Authenticate Claude Code (claude setup-token)",
			Skip: func() (bool, string, error) {
				if os.Getenv(tokenstore.TokenEnv) != "" {
					return true, tokenstore.TokenEnv + " already set", nil
				}
				return false, "", nil
			},
			Note: "interactive: claude prints a URL — open it in your Mac browser, complete the flow, paste the code back. claude then prints a sk-ant-oat01-… token; ahjo asks you to paste that token once more so it can save it to ~/.ahjo/.env (mode 0600). The saved token is what ahjo forwards into every container.",
			Show: "bash -lc 'claude setup-token'",
			Action: func(out io.Writer) error {
				if !claudeOnPath() {
					return fmt.Errorf("claude CLI not on PATH inside the VM (the install step should have handled this — re-run `ahjo init`)")
				}
				if err := initflow.RunShell(out, "", "bash", "-lc", "claude setup-token"); err != nil {
					return fmt.Errorf("claude setup-token: %w", err)
				}
				fmt.Fprint(out, "\nPaste the sk-ant-oat01-… token printed above: ")
				sc := bufio.NewScanner(os.Stdin)
				if !sc.Scan() {
					if err := sc.Err(); err != nil {
						return fmt.Errorf("read token: %w", err)
					}
					return fmt.Errorf("no token entered")
				}
				tok := strings.TrimSpace(sc.Text())
				if !strings.HasPrefix(tok, "sk-ant-oat01-") {
					return fmt.Errorf("token must start with sk-ant-oat01- (got %q)", trunc(tok, 16))
				}
				if err := tokenstore.SetToken(tok); err != nil {
					return fmt.Errorf("save token: %w", err)
				}
				if err := os.Setenv(tokenstore.TokenEnv, tok); err != nil {
					return err
				}
				fmt.Fprintln(out, "  → saved to "+tokenstore.Path())
				return nil
			},
			Post: "\nDone. Try:\n  ahjo doctor                              # green check\n  ahjo repo add <name> <git-url>           # register a repo\n  ahjo new <name> <branch>                 # create a sandboxed worktree",
		},
	}...)
	return steps
}

// coiInstallSteps returns the COI install (and, under Lima, open-mode config)
// steps. Under Lima we know the VM is already firewalled by macOS/vzNAT and
// only `mode = "open"` is useful, so we pre-disable ufw and run install.sh
// with NONINTERACTIVE=1 — every prompt flows through defaults. On bare-metal
// Linux the user's ufw/firewalld setup may matter, so we let install.sh
// prompt and skip the open-mode override.
//
// install.sh exposes only one knob for the install-method choice: the second
// arg to its `prompt_choice` helper (the "default"). With NONINTERACTIVE=1
// that default is what gets used. To pick "build from source" we sed-patch
// the script's default from "1" to "2" before piping to bash; in interactive
// mode the same patch flips which choice gets picked when the user hits enter.
func coiInstallSteps(onLima, buildCOI bool) []initflow.Step {
	curl := "curl -fsSL https://raw.githubusercontent.com/mensfeld/code-on-incus/master/install.sh"
	patch := `sed 's|prompt_choice "Choose \[1/2\] (default: 1): " "1"|prompt_choice "Choose [1/2] (default: 2): " "2"|'`
	pipeline := func(env string) string {
		if buildCOI {
			return curl + " | " + patch + " | " + env + "bash"
		}
		return curl + " | " + env + "bash"
	}

	if onLima {
		return []initflow.Step{
			{
				Title: "Install COI (Lima: non-interactive)",
				Skip: func() (bool, string, error) {
					if _, err := exec.LookPath("coi"); err == nil {
						return true, "coi already on PATH", nil
					}
					return false, "", nil
				},
				Note: coiNote(true, buildCOI),
				Show: "sudo systemctl disable --now ufw\n" + pipeline("NONINTERACTIVE=1 "),
				Action: func(out io.Writer) error {
					// Best-effort: ufw may already be inactive on minimal images.
					_ = initflow.RunShell(out, "", "sudo", "systemctl", "disable", "--now", "ufw")
					return initflow.RunBash(out, "", pipeline("NONINTERACTIVE=1 "))
				},
			},
			{
				Title: "Configure COI for open networking",
				Skip: func() (bool, string, error) {
					h, err := os.UserHomeDir()
					if err != nil {
						return false, "", err
					}
					b, err := os.ReadFile(filepath.Join(h, ".coi", "config.toml"))
					if err != nil {
						return false, "", nil
					}
					if strings.Contains(string(b), `mode = "open"`) {
						return true, `~/.coi/config.toml has mode = "open"`, nil
					}
					return false, "", nil
				},
				Note: "required under Lima — firewalld isn't usable in the VM and the Mac edge already firewalls it",
				Show: "writes ~/.coi/config.toml with [network] mode = \"open\"",
				Action: func(_ io.Writer) error {
					h, err := os.UserHomeDir()
					if err != nil {
						return err
					}
					dir := filepath.Join(h, ".coi")
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "config.toml"), []byte(initflow.CoiOpenNetworkConfig()), 0o644)
				},
			},
		}
	}
	return []initflow.Step{
		{
			Title: "Install COI (Linux: interactive)",
			Skip: func() (bool, string, error) {
				if _, err := exec.LookPath("coi"); err == nil {
					return true, "coi already on PATH", nil
				}
				return false, "", nil
			},
			Note: coiNote(false, buildCOI),
			Show: pipeline(""),
			Action: func(out io.Writer) error {
				return initflow.RunBash(out, "", pipeline(""))
			},
		},
	}
}

func coiNote(onLima, buildCOI bool) string {
	source := "pre-built binary"
	if buildCOI {
		source = "build from source (--build-coi)"
	}
	if onLima {
		return "ufw is disabled first (Lima/vzNAT already firewalls the VM and ahjo runs COI in `mode = \"open\"`), then install.sh runs with NONINTERACTIVE=1 so ufw/firewalld prompts flow through defaults. Install method: " + source + "."
	}
	return "interactive: COI's installer prompts for ufw vs firewalld. Install method: " + source + " (you can still hit enter to accept it). ahjo does not write `mode = \"open\"` on bare-metal Linux — if you keep ufw and skip firewalld you may want to set it manually in ~/.coi/config.toml afterwards."
}

// coiDefaultBuildScriptURL is the upstream COI default profile's build.sh.
// We fetch and patch it at init time as a workaround for the aqua-registry
// vs. pnpm release-format mismatch (see step Note in vmInitSteps).
const coiDefaultBuildScriptURL = "https://raw.githubusercontent.com/mensfeld/code-on-incus/master/profiles/default/build.sh"

// buildCOIDefaultWithMisePatch builds the coi-default image with a patched
// build.sh that swaps mise's `pnpm@latest` (aqua backend, broken) for
// `npm:pnpm@latest` (npm backend, working). COI's resolveAsset prefers a
// CWD-relative profiles/default/build.sh over its embedded copy, so we run
// `coi build` from a temp dir containing only that one patched file.
func buildCOIDefaultWithMisePatch(out io.Writer) error {
	tmpdir, err := os.MkdirTemp("", "ahjo-coi-build-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmpdir)

	body, err := exec.Command("curl", "-fsSL", coiDefaultBuildScriptURL).Output()
	if err != nil {
		return fmt.Errorf("fetch %s: %w", coiDefaultBuildScriptURL, err)
	}
	const needle = "pnpm@latest"
	if !strings.Contains(string(body), needle) {
		return fmt.Errorf("upstream build.sh no longer contains %q — review ahjo's COI mise workaround in internal/cli/init.go", needle)
	}
	patched := strings.Replace(string(body), needle, "npm:pnpm@latest", 1)

	profilesDir := filepath.Join(tmpdir, "profiles", "default")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		return err
	}
	scriptPath := filepath.Join(profilesDir, "build.sh")
	if err := os.WriteFile(scriptPath, []byte(patched), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(out, "  → patched %s (mise pnpm@latest → npm:pnpm@latest)\n", scriptPath)

	cmd := exec.Command("coi", "build")
	cmd.Dir = tmpdir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// reExecUnderSg replaces the current process with `sg incus-admin -c "<self> init [-y]"`
// so the new group membership granted by usermod takes effect without a
// re-shell. On success this never returns.
func reExecUnderSg(out io.Writer, yes bool) error {
	fmt.Fprintln(out, "  → re-exec under `sg incus-admin` so the rest of init runs with the new group")
	sg, err := exec.LookPath("sg")
	if err != nil {
		return fmt.Errorf("sg not on PATH: %w (sg is from the `login` package; install it or re-shell and re-run `ahjo init`)", err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	cmd := shellQuote(self) + " init"
	if yes {
		cmd += " -y"
	}
	argv := []string{"sg", "incus-admin", "-c", cmd}
	return syscall.Exec(sg, argv, os.Environ())
}

func currentUsername() string {
	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username
	}
	// Last-ditch fallback. user.Current resolves the actual login uid via
	// getpwuid and is correct under Lima's vsock shells where $USER is the
	// host's username, not the in-VM user — that's the bug we're avoiding.
	return os.Getenv("USER")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// claudeOnPath reports whether `claude` resolves inside a login shell. We use
// `bash -lc` rather than exec.LookPath because Claude Code installs into the
// user's mise-managed npm prefix, and mise's shims dir is typically only on
// PATH after shell init runs.
func claudeOnPath() bool {
	return exec.Command("bash", "-lc", "command -v claude >/dev/null 2>&1").Run() == nil
}

func hasGroup(idOutput, group string) bool {
	for _, g := range strings.Fields(strings.TrimSpace(idOutput)) {
		if g == group {
			return true
		}
	}
	return false
}
