// Package cli: ahjo mirror — activate a one-way push from /repo (in container)
// to a Mac-side directory (mounted into the container at /mirror via a
// writable Incus disk device on top of Lima virtiofs).
//
// State lives in incus device config + systemctl unit state. There is no
// separate state file — `incus config device list` and `systemctl is-active`
// are the source of truth, eliminating the "state file says X but daemon
// says Y" inconsistencies of v1.
//
// See designdocs/in-container-mirror.md for the full design.
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/ahjoruntime"
	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/mirror"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

const (
	mirrorDeviceName        = "mirror"
	mirrorContainerPath     = "/mirror"
	mirrorRepoPath          = "/repo"
	mirrorUnit              = "ahjo-mirror.service"
	mirrorUnitContainerPath = "/etc/systemd/system/ahjo-mirror.service"
	mirrorBinPath           = "/usr/local/bin/ahjo-mirror"
	mirrorDropInDir         = "/etc/systemd/system/ahjo-mirror.service.d"
	mirrorDropInPath        = "/etc/systemd/system/ahjo-mirror.service.d/flags.conf"
	mirrorNoSkiplistFlag    = "AHJO_MIRROR_NO_SKIPLIST=1"
)

func newMirrorCmd() *cobra.Command {
	var target string
	var noSkiplist bool
	var revert bool
	var skipRevert bool

	cmd := &cobra.Command{
		Use:   "mirror [<alias> | off | status | logs <alias>]",
		Short: "Mirror /repo to a Mac path via the in-container ahjo-mirror daemon",
		Long: `Activate a one-way push from /repo (inside the branch container) to a
Mac-side directory you choose with --target. The mirror runs as a systemd
unit inside the container; bootstrap and live event handling share a single
git-faithful gitignore matcher (go-git), so .gitignore'd files never reach
the Mac.

  ahjo mirror <alias> --target /Users/me/mirrors/foo
                                                — start; --target sticky per-repo
  ahjo mirror <alias> --no-skiplist             — also mirror node_modules etc
  ahjo mirror off                               — stop the active mirror (prompts to revert)
  ahjo mirror off --revert                      — stop and restore the Mac target to its pre-mirror state
  ahjo mirror off --skip-revert                 — stop and leave the mirrored files in place
  ahjo mirror status                            — list mirrors across the registry
  ahjo mirror logs <alias>                      — tail the daemon's journal

Mirror is one-way: container → Mac. Mac-side edits to mirrored files are
clobbered on the next event. .git/ is never propagated. When the target is a
git work tree (or empty), 'mirror off' can revert it to its exact pre-mirror
state: tracked files restored, mirror-added files removed, and gitignored files
like .env kept. Files added under --no-skiplist (e.g. node_modules) are not
garbage-collected by the revert. See designdocs/in-container-mirror.md for the
full design.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			ver := c.Root().Version
			switch {
			case len(args) == 0:
				return runMirrorStatus()
			case args[0] == "status":
				return runMirrorStatus()
			case args[0] == "off":
				return runMirrorOff(revert, skipRevert)
			case args[0] == "logs":
				if len(args) != 2 {
					return fmt.Errorf("usage: ahjo mirror logs <alias>")
				}
				return runMirrorLogs(args[1])
			default:
				if len(args) != 1 {
					return fmt.Errorf("usage: ahjo mirror <alias> [--target …] [--no-skiplist]")
				}
				return runMirrorOn(args[0], target, noSkiplist, ver)
			}
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Mac path to mirror into (sticky per-repo after first activation)")
	cmd.Flags().BoolVar(&noSkiplist, "no-skiplist", false, "skip the static skiplist (still honors .gitignore)")
	cmd.Flags().BoolVar(&revert, "revert", false, "with off: restore the Mac target to its pre-mirror state without prompting")
	cmd.Flags().BoolVar(&skipRevert, "skip-revert", false, "with off: leave the mirrored files on the Mac target without prompting")
	cmd.MarkFlagsMutuallyExclusive("revert", "skip-revert")
	return cmd
}

// runMirrorOn implements the v3 activation flow. Wrapped in lockfile.Acquire
// so two concurrent `ahjo mirror …` invocations don't race on incus device
// config or systemd state.
func runMirrorOn(alias, targetFlag string, noSkiplist bool, version string) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil {
		return fmt.Errorf("no branch registered for %q (run `ahjo create %s` first)", alias, alias)
	}
	containerName := br.IncusName
	if containerName == "" {
		return fmt.Errorf("registry row for %q has no incus_name; recreate with `ahjo rm %s && ahjo create`", alias, alias)
	}

	repo := reg.FindRepo(br.Repo)
	if repo == nil {
		return fmt.Errorf("internal: branch %q references missing repo %q", alias, br.Repo)
	}

	// Resolve target: explicit flag wins; else fall back to the per-repo
	// sticky path stored at first activation.
	targetPath := strings.TrimSpace(targetFlag)
	if targetPath == "" {
		targetPath = repo.MacMirrorTarget
	}
	if targetPath == "" {
		return fmt.Errorf("no target dir set for repo %q; pass --target </absolute/path>", repo.Name)
	}
	targetPath = paths.Expand(targetPath)
	if !filepath.IsAbs(targetPath) {
		return fmt.Errorf("--target must be absolute (got %q)", targetPath)
	}
	if err := validateMirrorTarget(targetPath); err != nil {
		return err
	}

	// Refuse if the container is stopped — activation must not become a
	// hidden way to start containers (memory: project_ahjo_mirror_lifecycle_coupling.md).
	status, err := incus.ContainerStatus(containerName)
	if err != nil {
		return err
	}
	if !strings.EqualFold(status, "Running") {
		return fmt.Errorf("container %s is %q; run `ahjo shell %s` first", containerName, status, alias)
	}

	// Single-active mirror: refuse if any OTHER container has the device.
	// Re-running on the same container is fine (idempotent reconfigure).
	for i := range reg.Branches {
		b := &reg.Branches[i]
		if b.IncusName == "" || b.IncusName == containerName {
			continue
		}
		has, err := incus.HasDevice(b.IncusName, mirrorDeviceName)
		if err != nil || !has {
			continue
		}
		return fmt.Errorf("another container already mirrors: %s — run `ahjo mirror off` first", b.IncusName)
	}

	// Snapshot the pre-mirror state of the host target so `mirror off` can
	// restore it. Done before any container mutation, so a cancel (or an error)
	// here leaves nothing observable changed. A cancel returns nil — the user
	// declined, not a failure.
	if cancelled, err := captureMirrorSnapshot(targetPath, br.Slug); err != nil {
		return err
	} else if cancelled {
		return nil
	}

	// Reconcile the daemon binary + unit. Stop the unit before pushing if
	// it's running so we don't ambiguously replace text segments live.
	if err := reconcileDaemonAssets(containerName, version); err != nil {
		return err
	}

	// Reconcile the --no-skiplist drop-in. Each ON call is the reconciliation
	// point; OFF leaves the file alone (harmless when the unit is disabled).
	if err := reconcileNoSkiplistDropIn(containerName, noSkiplist); err != nil {
		return err
	}

	// mkdir -p the target dir as the user (we're running inside the VM as
	// the user, so plain os.MkdirAll suffices — virtiofs writes through to
	// the Mac with the user's uid).
	if err := os.MkdirAll(targetPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", targetPath, err)
	}

	// Add the disk device. Idempotent (tolerates "already exists").
	if err := incus.AddDiskDevice(containerName, mirrorDeviceName, targetPath, mirrorContainerPath, false); err != nil {
		return err
	}

	if err := incus.SystemctlDaemonReload(containerName); err != nil {
		return err
	}
	if err := incus.SystemctlEnableNow(containerName, mirrorUnit); err != nil {
		return err
	}

	// Persist target as the per-repo default so subsequent `mirror on`s
	// can omit --target.
	if repo.MacMirrorTarget != targetPath {
		repo.MacMirrorTarget = targetPath
		if err := reg.Save(); err != nil {
			return fmt.Errorf("persist mirror target: %w", err)
		}
	}

	fmt.Printf("mirror: %s → %s (container %s)\n", alias, targetPath, containerName)
	fmt.Printf("  logs: ahjo mirror logs %s\n", alias)
	fmt.Println("  note: host-side file watchers (TS server, ESLint, Vite HMR) may need polling mode — virtiofs FSEvents coverage is partial.")

	if matched, err := skiplistPresence(containerName, noSkiplist); err == nil && len(matched) > 0 {
		fmt.Printf("warn: these directories will not be mirrored — pass --no-skiplist if you need them:\n")
		for _, p := range matched {
			fmt.Printf("  - %s\n", p)
		}
	}

	refreshAhjoState(alias)
	return nil
}

// captureMirrorSnapshot snapshots the pre-mirror state of targetPath so a later
// `mirror off` can restore it. The mechanism depends on the target type; for a
// dirty git tree (or a non-git non-empty dir) it confirms with the user first.
// Returns cancelled=true when the user (or a non-TTY) declines, in which case
// the caller must abort `mirror on` having changed nothing.
func captureMirrorSnapshot(targetPath, slug string) (cancelled bool, err error) {
	mode, err := mirror.DetectMode(targetPath)
	if err != nil {
		return false, err
	}
	switch mode {
	case mirror.ModeGit:
		summary, dirty, err := mirror.TargetDirty(targetPath)
		if err != nil {
			return false, err
		}
		if dirty {
			q := fmt.Sprintf("target %q has uncommitted changes (%s) that the mirror will clobber (restorable via 'ahjo mirror off --revert'); continue?", targetPath, summary)
			if !isTerminal(os.Stdin) || !promptYesNo(q) {
				fmt.Println("mirror: cancelled (target has uncommitted changes)")
				return true, nil
			}
		}
		if err := mirror.CaptureGit(targetPath, slug); err != nil {
			return false, err
		}
	case mirror.ModeFreshEmpty:
		if err := mirror.CaptureEmpty(slug); err != nil {
			return false, err
		}
	case mirror.ModeFreshNonEmpty:
		q := fmt.Sprintf("target %q has files and is not a git repo; mirror without the ability to revert?", targetPath)
		if !isTerminal(os.Stdin) || !promptYesNo(q) {
			fmt.Println("mirror: cancelled (target is not a git repo; revert unavailable)")
			return true, nil
		}
		// Proceed with no snapshot — `mirror off` will leave the files in place.
	}
	return false, nil
}

func runMirrorOff(revert, skipRevert bool) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	// Find the (one) container with a mirror device. Tolerate not-found
	// across the registry — `mirror off` is idempotent.
	var stopped bool
	for i := range reg.Branches {
		b := &reg.Branches[i]
		if b.IncusName == "" {
			continue
		}
		has, err := incus.HasDevice(b.IncusName, mirrorDeviceName)
		if err != nil || !has {
			continue
		}
		fmt.Printf("→ stopping mirror on %s\n", b.IncusName)

		// Resolve the host target (stored already-expanded; Expand is
		// idempotent on an absolute path).
		targetPath := ""
		if repo := reg.FindRepo(b.Repo); repo != nil {
			targetPath = paths.Expand(repo.MacMirrorTarget)
		}

		// Disable+stop only matters when the container is running. If it
		// isn't, the unit can't be active, so removing the device is enough.
		// When it is running, confirm the daemon is fully inactive before we
		// touch the target — a live daemon would re-copy files mid-restore.
		status, _ := incus.ContainerStatus(b.IncusName)
		if strings.EqualFold(status, "Running") {
			if err := incus.SystemctlDisableNow(b.IncusName, mirrorUnit); err != nil {
				fmt.Fprintf(cobraOutErr(), "warn: disable %s on %s: %v\n", mirrorUnit, b.IncusName, err)
			}
			if err := waitMirrorInactive(b.IncusName); err != nil {
				return err
			}
		}

		// Remove the device BEFORE reverting. Once the disk device is gone, no
		// in-container write can reach the host target, so the restore runs on
		// a quiescent tree — this closes the narrow window where the unit reads
		// inactive but the daemon's last tempfile+rename is still in flight.
		if err := incus.RemoveDevice(b.IncusName, mirrorDeviceName); err != nil {
			return err
		}

		// Optionally restore the host target to its pre-mirror state.
		if targetPath != "" && decideRevert(revert, skipRevert, targetPath, b.Slug) {
			if err := mirror.Revert(targetPath, b.Slug); err != nil {
				return fmt.Errorf("revert %s: %w", targetPath, err)
			}
			fmt.Printf("mirror: reverted %s to its pre-mirror state\n", targetPath)
		}
		stopped = true
		alias := b.Slug
		if len(b.Aliases) > 0 {
			alias = b.Aliases[0]
		}
		refreshAhjoStateByName(b.IncusName, b.Slug, alias, "")
	}
	if !stopped {
		fmt.Println("mirror: inactive")
		return nil
	}
	fmt.Println("mirror: off")
	return nil
}

// waitMirrorInactive polls until the mirror unit reports inactive, so a revert
// never races a still-writing daemon. Bounded at ~10s (the EnsureSSHAgentProxy
// poll idiom); a daemon that won't stop aborts the revert with a clear error.
func waitMirrorInactive(containerName string) error {
	for attempt := 0; attempt < 50; attempt++ {
		active, err := incus.SystemctlIsActive(containerName, mirrorUnit)
		if err != nil {
			return fmt.Errorf("check %s on %s: %w", mirrorUnit, containerName, err)
		}
		if !active {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("mirror daemon on %s still active after stop; aborting to avoid racing a live writer", containerName)
}

// decideRevert resolves whether `mirror off` should restore the host target.
// --skip-revert always declines; --revert restores when a snapshot exists;
// otherwise it prompts, defaulting to no when nothing was captured or stdin is
// not a TTY (the restore is destructive to the mirrored files, so a non-TTY
// default of "no" is the safe choice).
func decideRevert(revert, skipRevert bool, target, slug string) bool {
	if skipRevert {
		return false
	}
	possible := mirror.RevertPossible(target, slug)
	if revert {
		if !possible {
			fmt.Printf("mirror: nothing to revert for %s (no pre-mirror snapshot)\n", target)
		}
		return possible
	}
	if !possible || !isTerminal(os.Stdin) {
		return false
	}
	return promptYesNo(fmt.Sprintf("revert %s to its pre-mirror state? (mirrored files removed; gitignored files like .env kept)", target))
}

func runMirrorStatus() error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	var any bool
	for i := range reg.Branches {
		b := &reg.Branches[i]
		if b.IncusName == "" {
			continue
		}
		has, err := incus.HasDevice(b.IncusName, mirrorDeviceName)
		if err != nil || !has {
			continue
		}
		any = true
		alias := primaryAlias(b)
		active, _ := incus.SystemctlIsActive(b.IncusName, mirrorUnit)
		state := "inactive"
		if active {
			state = "active"
		}
		repo := reg.FindRepo(b.Repo)
		target := ""
		if repo != nil {
			target = repo.MacMirrorTarget
		}
		fmt.Printf("mirror: %s\n", state)
		fmt.Printf("  alias:     %s\n", alias)
		fmt.Printf("  container: %s\n", b.IncusName)
		fmt.Printf("  source:    %s (in container)\n", mirrorRepoPath)
		fmt.Printf("  target:    %s (Mac)\n", target)
	}
	if !any {
		fmt.Println("mirror: inactive")
	}
	return nil
}

func runMirrorLogs(alias string) error {
	reg, err := registry.Load()
	if err != nil {
		return err
	}
	br := reg.FindBranchByAlias(alias)
	if br == nil || br.IncusName == "" {
		return fmt.Errorf("no branch %q (or it has no container)", alias)
	}
	// Pass through to journalctl -f. ExecAttach replaces our process so
	// the user gets normal Ctrl-C semantics + full TTY.
	return incus.ExecAttach(br.IncusName, 0, nil, "", "journalctl", "-u", mirrorUnit, "-n", "200", "--follow")
}

// validateMirrorTarget refuses paths that would point outside the Mac
// virtiofs writable mount (when running under Lima) or that nest inside
// ~/.ahjo/. Bare-metal Linux only enforces the second guard. Recovered
// from commit 139d758.
func validateMirrorTarget(p string) error {
	cleaned := filepath.Clean(p) + string(filepath.Separator)
	if strings.HasPrefix(cleaned, paths.AhjoDir()+string(filepath.Separator)) {
		return fmt.Errorf("target %q must not live under %s", p, paths.AhjoDir())
	}
	mac, ok := paths.MacHostHome()
	if !ok {
		return nil
	}
	if !strings.HasPrefix(cleaned, mac+string(filepath.Separator)) {
		return fmt.Errorf("target %q is not under the Mac home (%s); mirror can only write into the writable virtiofs mount", p, mac)
	}
	return nil
}

// reconcileDaemonAssets stop-pushes the embedded ahjo-mirror binary AND the
// systemd unit into the container when missing or stale. Stop-push-start
// avoids any ambiguity about replacing a running binary's text segment;
// cost is ~1s of mirror downtime during upgrades.
//
// The doc's "step 4" (designdocs/in-container-mirror.md) describes pushing
// the binary; we also push the unit because a container built before the
// v3 Feature change won't have the unit at all, and "incus exec systemctl
// enable" fails before the user has any chance to run `ahjo update`. The
// unit is ~500 bytes; pushing it unconditionally is cheap and removes a
// sharp edge in the migration story.
func reconcileDaemonAssets(containerName, expectedVersion string) error {
	// Probe the binary's version stamp via a quiet exec — the binary may be
	// missing on first activation, so a failure here is expected and we
	// don't want it polluting the user's terminal.
	got, _ := quietContainerExec(containerName, mirrorBinPath, "-version")
	binaryFresh := expectedVersion != "" && strings.TrimSpace(got) == expectedVersion

	_, unitErr := quietContainerExec(containerName, "test", "-f", mirrorUnitContainerPath)
	unitOK := unitErr == nil

	if binaryFresh && unitOK {
		return nil
	}

	// Stop the unit first so we're not replacing a running binary's text
	// segment. Tolerant of "not loaded" (when the unit doesn't yet exist).
	if err := incus.SystemctlStop(containerName, mirrorUnit); err != nil {
		return err
	}

	if !binaryFresh {
		if err := pushBinary(containerName); err != nil {
			return err
		}
	}
	if !unitOK {
		if err := pushUnit(containerName); err != nil {
			return err
		}
	}
	return nil
}

func pushBinary(containerName string) error {
	binary, err := embeddedDaemonBinary()
	if err != nil {
		return err
	}
	tmp, err := writeTempFile("ahjo-mirror-*", binary, 0o755)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := incus.FilePush(containerName, tmp, mirrorBinPath); err != nil {
		return fmt.Errorf("push %s: %w", mirrorBinPath, err)
	}
	if _, err := incus.Exec(containerName, "chmod", "0755", mirrorBinPath); err != nil {
		return err
	}
	return nil
}

func pushUnit(containerName string) error {
	body, err := ahjoruntime.FeatureFS.ReadFile("feature/ahjo-mirror.service")
	if err != nil {
		return fmt.Errorf("read embedded unit file: %w", err)
	}
	tmp, err := writeTempFile("ahjo-mirror-service-*", body, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := incus.FilePush(containerName, tmp, mirrorUnitContainerPath); err != nil {
		return fmt.Errorf("push %s: %w", mirrorUnitContainerPath, err)
	}
	return nil
}

func writeTempFile(pattern string, body []byte, mode os.FileMode) (string, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// embeddedDaemonBinary returns the host-arch-matched daemon binary from
// the FeatureFS embed (FeatureFS already includes both arches via the
// existing `//go:embed all:feature` pattern). The host arch determines
// the container arch (Lima VM matches host on macOS; bare-metal Linux is
// the same machine), so runtime.GOARCH is the right key.
func embeddedDaemonBinary() ([]byte, error) {
	name := "feature/ahjo-mirror.linux-" + runtime.GOARCH
	b, err := ahjoruntime.FeatureFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s (run `go generate ./...`): %w", name, err)
	}
	return b, nil
}

// reconcileNoSkiplistDropIn writes or removes the systemd drop-in based
// on the flag. The drop-in form keeps the base unit untouched and reverts
// cleanly on the next activation.
func reconcileNoSkiplistDropIn(containerName string, noSkiplist bool) error {
	if !noSkiplist {
		// Remove (tolerant of not-found).
		_, _ = incus.Exec(containerName, "rm", "-f", mirrorDropInPath)
		return nil
	}
	if _, err := incus.Exec(containerName, "mkdir", "-p", mirrorDropInDir); err != nil {
		return err
	}
	dropIn := "[Service]\nEnvironment=" + mirrorNoSkiplistFlag + "\n"
	tmp, err := os.CreateTemp("", "ahjo-mirror-flags-*.conf")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(dropIn); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := incus.FilePush(containerName, tmp.Name(), mirrorDropInPath); err != nil {
		return err
	}
	return nil
}

// skiplistPresence runs a single bounded `find` inside the container to
// surface any skiplisted directories that exist in /repo. The `-prune`
// keeps the find cheap on node_modules-heavy trees (we never descend
// inside the matched dirs).
func skiplistPresence(containerName string, noSkiplist bool) ([]string, error) {
	if noSkiplist {
		return nil, nil
	}
	// Build args: find /repo -maxdepth 4 -type d \( -name X -o -name Y … \) -prune -print
	args := []string{"find", mirrorRepoPath, "-maxdepth", "4", "-type", "d", "("}
	skipNames := []string{
		".git", "node_modules", ".next", ".nuxt", ".svelte-kit", ".turbo",
		"__pycache__", ".venv", "venv", ".pytest_cache", ".ruff_cache", ".mypy_cache",
	}
	for i, n := range skipNames {
		if i > 0 {
			args = append(args, "-o")
		}
		args = append(args, "-name", n)
	}
	args = append(args, ")", "-prune", "-print")
	out, err := incus.Exec(containerName, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var matched []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		matched = append(matched, l)
	}
	return matched, nil
}

// stopAndRemoveMirror is the pre-destroy hook: if `containerName` has a
// mirror device, disable+stop the unit (best-effort, ok if container is
// already stopped) and remove the device. Idempotent / tolerant of "no
// such device" — safe to call before every destroy. Per memory
// project_ahjo_mirror_lifecycle_coupling.md: destroy paths auto-stop the
// mirror so the user is never left with a stale device pointing at a
// gone container.
func stopAndRemoveMirror(containerName string) error {
	has, err := incus.HasDevice(containerName, mirrorDeviceName)
	if err != nil || !has {
		return nil
	}
	status, _ := incus.ContainerStatus(containerName)
	if strings.EqualFold(status, "Running") {
		_ = incus.SystemctlDisableNow(containerName, mirrorUnit)
	}
	if err := incus.RemoveDevice(containerName, mirrorDeviceName); err != nil {
		return err
	}
	// Best-effort: do NOT auto-revert here — this is a teardown path (rm /
	// recreate), often non-interactive, where a surprise destructive restore
	// would be wrong. Just tell the user a snapshot was kept so they can
	// restore it deliberately.
	hintMirrorSnapshotKept(containerName)
	return nil
}

// hintMirrorSnapshotKept prints a note when the just-removed mirror left a
// restorable pre-mirror snapshot. Looked up best-effort by IncusName; the git
// snapshot lives in the target's own .git, independent of this container, so it
// survives the destroy that follows.
func hintMirrorSnapshotKept(containerName string) {
	reg, err := registry.Load()
	if err != nil {
		return
	}
	for i := range reg.Branches {
		b := &reg.Branches[i]
		if b.IncusName != containerName {
			continue
		}
		repo := reg.FindRepo(b.Repo)
		if repo == nil || repo.MacMirrorTarget == "" {
			return
		}
		target := paths.Expand(repo.MacMirrorTarget)
		if mirror.RevertPossible(target, b.Slug) {
			fmt.Printf("note: a pre-mirror snapshot of %s was kept (refs %s%s/); the mirrored files were left in place.\n",
				target, "refs/ahjo/mirror-snapshot/", b.Slug)
		}
		return
	}
}

// quietContainerExec runs `incus exec <container> -- <argv…>` capturing
// both stdout and stderr; the stderr stays in the returned error rather
// than leaking to the user's terminal. Used by the binary/unit probes
// in reconcileDaemonAssets where a missing-file failure is expected on
// first activation.
func quietContainerExec(container string, argv ...string) (string, error) {
	args := append([]string{"exec", container, "--"}, argv...)
	cmd := exec.Command("incus", args...)
	out, err := cmd.Output()
	return string(out), err
}

func primaryAlias(b *registry.Branch) string {
	if len(b.Aliases) > 0 {
		return b.Aliases[0]
	}
	return b.Slug
}
