package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/devcontainer"
	"github.com/lasselaakkonen/ahjo/internal/idmap"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/initflow"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/tokenstore"
)

func newInitCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "One-time setup: Incus, ahjo-base image (devcontainer Feature pipeline), ~/.ahjo/ skeleton, claude setup-token",
		Long: `init walks through the verified bring-up sequence step by step:

  1. Zabbly apt signing key + sources list
  2. apt install incus
  3. incus admin init via preseed (fixed subnet 10.20.30.1/24)
  4. usermod -aG incus-admin (re-exec under sg, no re-shell required)
  5. ahjo-base image: pull images:ubuntu/24.04, launch a transient
     container, apply the embedded ahjo-runtime devcontainer Feature,
     publish as ahjo-base
  6. create ~/.ahjo/ skeleton
  7. install Claude Code if missing (curl -fsSL https://claude.ai/install.sh | bash — Anthropic's native installer, auto-updating)
  8. claude setup-token (saves token to ~/.ahjo/.env)

Each step detects whether it's already done and skips, so re-runs are safe.
The flow runs end-to-end in a single invocation.

Use 'ahjo update' afterwards whenever you change the host binary or the
embedded ahjo-runtime Feature — it pushes the new binary into the VM (on
macOS) and rebuilds the ahjo-base image without re-running the rest of
the init pipeline.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !insideLinuxVM() {
				return fmt.Errorf("the in-VM phase of `ahjo init` only runs on Linux; on macOS the same `ahjo init` first brings up the Lima VM and tells you to enter it before re-running")
			}
			r := initflow.Runner{Yes: yes, In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
			return r.Execute(vmInitSteps(yes))
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip per-step confirmation prompts")
	return cmd
}

func insideLinuxVM() bool {
	// Heuristic: we run on linux. ahjo-mac handles the macOS pre-VM phase.
	return runtimeIsLinux()
}

func vmInitSteps(yes bool) []initflow.Step {
	username := currentUsername()
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
		subuidGrantStep(),
		inotifySysctlStep(),
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
	steps = append(steps, []initflow.Step{
		{
			Title: "Build ahjo-base image via the ahjo-runtime devcontainer Feature (~5 min on first run)",
			Skip: func() (bool, string, error) {
				exists, err := incus.ImageAliasExists(devcontainer.AhjoBaseAlias)
				if err != nil {
					return false, "", err
				}
				if exists {
					return true, "ahjo-base image present", nil
				}
				return false, "", nil
			},
			Note: "pulls images:ubuntu/24.04 once into the local image store (alias " + devcontainer.OSBaseAlias + "), launches a transient container, runs the embedded ahjo-runtime install.sh as root, publishes the result as ahjo-base, and deletes the transient container.",
			Show: "incus image copy " + devcontainer.UpstreamRemote + " local: --alias " + devcontainer.OSBaseAlias + "\n" +
				"incus launch " + devcontainer.OSBaseAlias + " ahjo-build-<rand>\n" +
				"apply ahjo-runtime Feature (sshd + ahjo-claude-prepare + Node + corepack)\n" +
				"incus publish ahjo-build-<rand> --alias " + devcontainer.AhjoBaseAlias + "\n" +
				"incus delete ahjo-build-<rand>",
			Action: func(out io.Writer) error {
				return devcontainer.BuildAhjoBase(out, false)
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
			Show: "creates ~/.ahjo/{host-keys,profiles,shared}",
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
		},
		{
			Title: "Mark Claude onboarding complete in ~/.claude.json",
			Skip: func() (bool, string, error) {
				ok, _, err := claudeOnboardingMarked()
				if err != nil {
					return false, "", err
				}
				if ok {
					return true, "hasCompletedOnboarding already true", nil
				}
				return false, "", nil
			},
			Note: "ahjo repo add pushes the host's ~/.claude.json into every container, " +
				"overwriting whatever the ahjo-base image baked in. Without `hasCompletedOnboarding: true` " +
				"on the host, every container greets the user with claude's first-run flow (theme + login picker). " +
				"This step writes the marker once on the host so containers start post-onboarding. " +
				"See CLAUDE-SETTING.md for the full picture.",
			Show: `merge {"hasCompletedOnboarding": true, "lastOnboardingVersion": "` + claudeOnboardingVersion + `"} into ~/.claude.json`,
			Action: func(out io.Writer) error {
				p, err := claudeJSONPath()
				if err != nil {
					return err
				}
				if err := mergeClaudeOnboardingMarker(p); err != nil {
					return err
				}
				fmt.Fprintln(out, "  → merged hasCompletedOnboarding=true into "+p)
				return nil
			},
			Post: "\nDone. Try:\n  ahjo doctor                              # green check\n  ahjo repo add <git-url>                  # clone into a default container\n  ahjo new <repo-alias> <branch>           # create a COW branch container",
		},
	}...)
	return steps
}

// subuidGrantStep ensures /etc/subuid + /etc/subgid grant the Incus daemon
// permission to delegate the in-VM host UID/GID into a container's userns.
// Required so the per-container `raw.idmap` ahjo applies in cli/repo.go
// (default container) and cli/new.go (COW-cloned branch containers) is
// honored at start; without these lines, `newuidmap` rejects the mapping
// and the container fails to come up.
//
// Shared between `ahjo init` and `ahjo update` — both need to assert the
// invariant. The step is idempotent: re-runs detect the lines and skip the
// daemon restart entirely.
//
// Background: ahjo's containers run on the VM's local btrfs pool, not on a
// VM-level virtiofs share, so workspace UID mapping must happen at the
// container userns layer rather than at the VM mount layer. See
// CONTAINER-ISOLATION.md "Workspace UID mapping".
func subuidGrantStep() initflow.Step {
	uid := os.Getuid()
	gid := os.Getgid()
	subuidLine := fmt.Sprintf("root:%d:1", uid)
	subgidLine := fmt.Sprintf("root:%d:1", gid)
	return initflow.Step{
		Title: "Grant Incus daemon subuid/subgid for the in-VM host user",
		Skip: func() (bool, string, error) {
			ok, err := idmap.HasSubuidGrants(uid, gid)
			if err != nil {
				return false, "", err
			}
			if ok {
				return true, "/etc/subuid and /etc/subgid already grant " + subuidLine, nil
			}
			return false, "", nil
		},
		Note: "Required so each ahjo container's `raw.idmap` (which maps the VM " +
			"host user onto the in-container `ubuntu` user) is honored at start.",
		Show: fmt.Sprintf("echo '%s' | sudo tee -a /etc/subuid\n", subuidLine) +
			fmt.Sprintf("echo '%s' | sudo tee -a /etc/subgid\n", subgidLine) +
			"sudo systemctl restart incus  (only if either file changed)",
		Action: func(out io.Writer) error {
			changed, err := idmap.EnsureSubuidGrants(uid, gid, out)
			if err != nil {
				return err
			}
			if !changed {
				fmt.Fprintln(out, "  → no change; skipping daemon restart")
				return nil
			}
			fmt.Fprintln(out, "  → restarting incus to pick up new subuid/subgid grants")
			return initflow.RunShell(out, "", "sudo", "systemctl", "restart", "incus")
		},
	}
}

// inotifySysctlStep writes /etc/sysctl.d/99-ahjo.conf with two values
// sized for the v3 ahjo-mirror daemon's worst case (full-tree bootstrap of
// a 30k-file monorepo, with an agent burst running concurrently). Same
// code path on Mac (writes inside the Lima VM, where `ahjo init` runs after
// the macOS-side bringup) and bare-metal Linux (writes on the host).
//
// `linux.sysctl.fs.inotify.*` is NOT among the namespace-scoped sysctls
// Incus exposes via per-container `linux.sysctl.*` (only `net.*` is), so
// the bump must land on the kernel hosting the containers — i.e. the
// VM/host, not the container.
//
// See designdocs/in-container-mirror.md "Lifecycle hazards #3" + Open
// question 4.
func inotifySysctlStep() initflow.Step {
	const (
		sysctlPath  = "/etc/sysctl.d/99-ahjo.conf"
		sysctlBody  = "# managed by `ahjo init` — see designdocs/in-container-mirror.md\n" +
			"fs.inotify.max_user_watches=1048576\n" +
			"fs.inotify.max_queued_events=65536\n"
	)
	return initflow.Step{
		Title: "Bump fs.inotify.{max_user_watches,max_queued_events} for ahjo-mirror",
		Skip: func() (bool, string, error) {
			b, err := os.ReadFile(sysctlPath)
			if err != nil {
				if os.IsNotExist(err) {
					return false, "", nil
				}
				return false, "", err
			}
			if string(b) == sysctlBody {
				return true, sysctlPath + " already has the expected values", nil
			}
			return false, "", nil
		},
		Note: "Required so the ahjo-mirror daemon can install one watch per source-tree directory on big monorepos. fs.inotify is not namespace-scoped, so the bump goes on the kernel hosting the containers (Lima VM under macOS, host kernel under bare-metal Linux).",
		Show: fmt.Sprintf("write %s with:\n  fs.inotify.max_user_watches=1048576\n  fs.inotify.max_queued_events=65536\nsudo sysctl --system", sysctlPath),
		Action: func(out io.Writer) error {
			if err := initflow.RunBash(out, sysctlBody, "sudo tee "+sysctlPath+" >/dev/null"); err != nil {
				return fmt.Errorf("write %s: %w", sysctlPath, err)
			}
			return initflow.RunShell(out, "", "sudo", "sysctl", "--system")
		},
	}
}

// claudeOnboardingVersion is the value written to ~/.claude.json's
// `lastOnboardingVersion`. Bump this only if a future Claude release introduces
// a new onboarding gate that re-prompts users above some version threshold.
const claudeOnboardingVersion = "2.1.126"

func claudeJSONPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// claudeOnboardingMarked reports whether ~/.claude.json exists and has
// `hasCompletedOnboarding: true`. A missing file or parse error returns false
// without an error so the caller treats it as "not marked yet".
func claudeOnboardingMarked() (bool, string, error) {
	p, err := claudeJSONPath()
	if err != nil {
		return false, "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, p, nil
		}
		return false, p, err
	}
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		return false, p, nil
	}
	v, _ := d["hasCompletedOnboarding"].(bool)
	return v, p, nil
}

// mergeClaudeOnboardingMarker reads ~/.claude.json (creating an empty object if
// missing or unparseable), sets hasCompletedOnboarding/lastOnboardingVersion,
// and writes the result back at mode 0600.
func mergeClaudeOnboardingMarker(path string) error {
	d := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &d)
	} else if !os.IsNotExist(err) {
		return err
	}
	if d == nil {
		d = map[string]any{}
	}
	d["hasCompletedOnboarding"] = true
	d["lastOnboardingVersion"] = claudeOnboardingVersion
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o600)
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
