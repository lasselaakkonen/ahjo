// Package cli: ahjo mirror — a one-way push from /repo (in a branch
// container) to a host directory.
//
// Nothing of the host is mounted into the container. The host runs the
// writer (`ahjo _mirror-daemon`, see internal/mirror/hostdaemon.go), which
// spawns the in-container watcher through `incus exec` and applies its frame
// stream to the target under the checks in internal/mirror/receiver.go. The
// container is untrusted; the daemon is the only thing that writes the target.
//
// State: ~/.ahjo/mirror/state.json (written here, under the ahjo lockfile)
// means "a mirror is configured"; the daemon's flock means "it is alive".
package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
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
	mirrorRepoPath = "/repo"
	mirrorBinPath  = "/usr/local/bin/ahjo-mirror"
	mirrorBinName  = "ahjo-mirror"

	// Legacy (pre-host-writer) mirror: a writable disk device bind-mounting
	// the host target at /mirror plus an in-container systemd unit. These
	// names exist only so removeLegacyMirror can tear that setup down.
	legacyMirrorDeviceName = "mirror"
	legacyMirrorUnit       = "ahjo-mirror.service"
	legacyMirrorUnitPath   = "/etc/systemd/system/ahjo-mirror.service"
	legacyMirrorDropInDir  = "/etc/systemd/system/ahjo-mirror.service.d"

	mirrorStopTimeout = 10 * time.Second
)

func newMirrorCmd() *cobra.Command {
	var target string
	var noSkiplist bool
	var noRevert bool

	cmd := &cobra.Command{
		Use:   "mirror [<alias> | off | revert <target> | status | logs <alias>]",
		Short: "Mirror /repo to a host path (host-side writer, in-container watcher)",
		Long: `Activate a one-way push from /repo (inside the branch container) to a
host directory you choose with --target. A watcher inside the container
reports changes; a host-side daemon is the only thing that writes the target.
No host directory is mounted into the container, so the container can neither
read the target nor write anything the daemon does not accept. Bootstrap and
live event handling share a single git-faithful gitignore matcher (go-git), so
.gitignore'd files never reach the host.

  ahjo mirror <alias> --target /Users/me/mirrors/foo
                                                — start; --target sticky per-repo
  ahjo mirror <alias> --no-skiplist             — also mirror node_modules etc
  ahjo mirror off                               — stop the active mirror and revert the target to its pre-mirror state
  ahjo mirror off --no-revert                   — stop but leave the mirrored files in place
  ahjo mirror revert <target>                   — restore a target from its kept snapshot (no container needed)
  ahjo mirror status                            — show the configured mirror
  ahjo mirror logs <alias>                      — tail the host daemon's log

Mirror is one-way: container → host. Host-side edits to mirrored files are
clobbered on the next event. Deletions are not replicated. A path with a .git
component is never written (not even with --no-skiplist), nor is anything that
would make a directory look like a git repository; symlinks that leave the
target are refused, and files over 512 MiB are skipped.

The mirrored files are authored inside the container: review them before
running them on the host.

When the target is a git work tree (or empty), 'mirror off' reverts it to its
exact pre-mirror state by default (pass --no-revert to keep the files): tracked
files restored, mirror-added files removed, and gitignored files like .env
kept. Files added under --no-skiplist (e.g. node_modules) are not
garbage-collected by the revert. Activating a mirror while another one is
configured takes over — the old mirror is stopped and reverted first.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			ver := c.Root().Version
			switch {
			case len(args) == 0:
				return runMirrorStatus()
			case args[0] == "status":
				return runMirrorStatus()
			case args[0] == "off":
				return runMirrorOff(noRevert)
			case args[0] == "revert":
				if len(args) < 2 || len(args) > 3 {
					return fmt.Errorf("usage: ahjo mirror revert <target> [<slug>]")
				}
				slugArg := ""
				if len(args) == 3 {
					slugArg = args[2]
				}
				return runMirrorRevert(args[1], slugArg)
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
	cmd.Flags().StringVar(&target, "target", "", "host path to mirror into (sticky per-repo after first activation)")
	cmd.Flags().BoolVar(&noSkiplist, "no-skiplist", false, "skip the static skiplist (still honors .gitignore; .git is never mirrored)")
	cmd.Flags().BoolVar(&noRevert, "no-revert", false, "with off: leave the mirrored files in place instead of reverting the target to its pre-mirror state")
	return cmd
}

// newMirrorDaemonCmd is the hidden host-side writer. `ahjo mirror <alias>`
// spawns it detached; it serves the mirror described by the state file until
// that state is cleared, tombstoned or replaced.
func newMirrorDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    mirror.DaemonCommandName,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			version := c.Root().Version
			logger, err := mirror.OpenFileLogger(paths.MirrorLogPath())
			if err != nil {
				return err
			}
			defer func() { _ = logger.Close() }()
			ctx, cancel := mirror.InstallSignalHandler()
			defer cancel()
			return mirror.RunDaemon(ctx, mirror.DaemonOptions{
				Spawn:           mirrorSpawnFunc(version),
				Version:         version,
				Logf:            logger.Logf,
				ContainerExists: incus.ContainerExists,
			})
		},
	}
}

// mirrorSpawnFunc starts the in-container watcher for the daemon. It
// reconciles the watcher binary before every start, so a container whose
// binary is stale (or, with forcePush, suspect) gets the embedded one.
func mirrorSpawnFunc(version string) mirror.SpawnFunc {
	return func(ctx context.Context, st *mirror.State, forcePush bool) (*mirror.Session, error) {
		status, err := incus.ContainerStatus(st.Container)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(status, "Running") {
			return nil, fmt.Errorf("container %s is %q", st.Container, status)
		}
		if err := reconcileWatcherBinary(st.Container, version, forcePush); err != nil {
			return nil, err
		}
		argv := []string{mirrorBinPath, "watch"}
		if st.NoSkiplist {
			argv = append(argv, "--no-skiplist")
		}
		argv = append(argv, mirrorRepoPath)

		sctx, cancel := context.WithCancel(ctx)
		cmd := incus.ExecStreamCommand(sctx, st.Container, 1000, argv...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			cancel()
			return nil, fmt.Errorf("incus exec %s: %w", st.Container, err)
		}
		var once sync.Once
		return &mirror.Session{
			Stdout: stdout,
			Stderr: stderr,
			Stop: func() {
				once.Do(func() {
					cancel() // kills the incus client; the watcher exits on its next write
					_ = cmd.Wait()
				})
			},
		}, nil
	}
}

// spawnMirrorDaemon starts the host daemon detached (own session, no stdio),
// mirroring spawnRefreshBase. The environment is scrubbed: the Mac relay puts
// tokens such as GH_TOKEN into the CLI's env, and a long-lived process that
// handles container-supplied data must not carry them.
func spawnMirrorDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve ahjo binary: %w", err)
	}
	cmd := exec.Command(exe, mirror.DaemonCommandName)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME", k == "PATH", k == "USER", k == "LOGNAME", k == "LANG",
			strings.HasPrefix(k, "XDG_"), strings.HasPrefix(k, "INCUS_"):
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn mirror daemon: %w", err)
	}
	return cmd.Process.Release()
}

// mirrorPreRun is the root PersistentPreRun hook. On the first ahjo command
// after an upgrade it removes mirrors of the old bind-mount design; on every
// command it revives the host daemon when a mirror is configured but the
// daemon is gone (VM/host reboot, crash) or runs an older ahjo version.
// Best-effort and quiet: it must never fail or slow the user's command.
func mirrorPreRun(c *cobra.Command, _ []string) {
	if runtime.GOOS != "linux" {
		return // the CLI proper only runs on Linux (in the VM or bare-metal)
	}
	top := c
	for top.HasParent() && top.Parent().HasParent() {
		top = top.Parent()
	}
	switch top.Name() {
	case "mirror", mirror.DaemonCommandName, "nuke", "help", "completion", "__complete":
		return
	}
	if top.Hidden {
		// Machine-read RPCs (the Mac TUI polls `top-state`): never print,
		// never wait — only bring a dead daemon back.
		reviveMirrorDaemon("")
		return
	}
	sweepLegacyMirrors()
	reviveMirrorDaemon(c.Root().Version)
}

// reviveMirrorDaemon needs no ahjo lock: the Stopping tombstone and the
// daemon's own state re-checks make a concurrent stop safe. With a non-empty
// version it also restarts a daemon left over from an older ahjo binary.
func reviveMirrorDaemon(version string) {
	st, err := mirror.LoadState()
	if err != nil || st == nil || st.Stopping {
		return
	}
	if mirror.DaemonAlive() {
		status := mirror.LoadStatus()
		if version == "" || status == nil || status.Version == version {
			return
		}
		// The ahjo binary was replaced; the running daemon still carries the
		// old validation code.
		if err := mirror.StopDaemon(mirrorStopTimeout); err != nil {
			return
		}
	}
	_ = spawnMirrorDaemon()
}

// sweepLegacyMirrors runs once per install (marker file): an upgraded host
// can still have an old mirror running with its writable /mirror mount while
// every status surface of this version reports "inactive".
func sweepLegacyMirrors() {
	if _, err := os.Stat(paths.MirrorLegacySweptPath()); err == nil {
		return
	}
	release, err := lockfile.AcquireWithTimeout(2 * time.Second)
	if err != nil {
		return
	}
	defer release()
	reg, err := registry.Load()
	if err != nil {
		return
	}
	clean := true
	for i := range reg.Branches {
		if _, err := removeLegacyMirror(reg, &reg.Branches[i]); err != nil {
			clean = false
		}
	}
	if clean {
		if err := os.MkdirAll(paths.MirrorRunDir(), 0o755); err == nil {
			_ = os.WriteFile(paths.MirrorLegacySweptPath(), nil, 0o600)
		}
	}
}

// removeLegacyMirror tears down a pre-host-writer mirror on branch b: stop
// and delete the in-container unit, remove the writable disk device, and
// revert the host target the way `mirror off` used to. A container that no
// longer exists counts as clean.
func removeLegacyMirror(reg *registry.Registry, b *registry.Branch) (removed bool, err error) {
	name := b.IncusName
	if name == "" {
		return false, nil
	}
	has, err := incus.HasDevice(name, legacyMirrorDeviceName)
	if err != nil {
		if exists, eerr := incus.ContainerExists(name); eerr == nil && !exists {
			return false, nil
		}
		return false, err
	}
	if !has {
		return false, nil
	}
	out := cobraOutErr() // stderr: this can run ahead of any command
	fmt.Fprintf(out, "→ removing legacy bind-mount mirror on %s\n", name)
	if status, _ := incus.ContainerStatus(name); strings.EqualFold(status, "Running") {
		if err := incus.SystemctlDisableNow(name, legacyMirrorUnit); err != nil {
			fmt.Fprintf(cobraOutErr(), "warn: disable %s on %s: %v\n", legacyMirrorUnit, name, err)
		}
		for attempt := 0; attempt < 50; attempt++ {
			if active, err := incus.SystemctlIsActive(name, legacyMirrorUnit); err != nil || !active {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, _ = quietContainerExec(name, "rm", "-rf", legacyMirrorUnitPath, legacyMirrorDropInDir)
		_, _ = quietContainerExec(name, "systemctl", "daemon-reload")
	}
	if err := incus.RemoveDevice(name, legacyMirrorDeviceName); err != nil {
		return false, err
	}
	if repo := reg.FindRepo(b.Repo); repo != nil && repo.MacMirrorTarget != "" {
		target := paths.Expand(repo.MacMirrorTarget)
		if mirror.RevertPossible(target, b.Slug) {
			if err := mirror.Revert(target, b.Slug); err != nil {
				fmt.Fprintf(cobraOutErr(), "warn: revert %s: %v\n", target, err)
			} else {
				fmt.Fprintf(out, "mirror: reverted %s to its pre-mirror state\n", target)
			}
		}
	}
	fmt.Fprintf(out, "mirror: removed the old bind-mount mirror on %s (upgrade); run `ahjo mirror %s` to resume.\n", name, primaryAlias(b))
	fmt.Fprintf(out, "  note: a process inside %s that still holds a file open under /mirror may keep it until the container restarts.\n", name)
	return true, nil
}

// runMirrorOn activates a mirror. Wrapped in lockfile.Acquire so two
// concurrent `ahjo mirror …` invocations don't race on the state file.
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
	targetPath = filepath.Clean(targetPath)
	if err := validateMirrorTarget(targetPath); err != nil {
		return fmt.Errorf("%w (pass a different --target)", err)
	}

	// Refuse if the container is stopped — activation must not become a
	// hidden way to start containers.
	status, err := incus.ContainerStatus(containerName)
	if err != nil {
		return err
	}
	if !strings.EqualFold(status, "Running") {
		return fmt.Errorf("container %s is %q; run `ahjo shell %s` first", containerName, status, alias)
	}

	// Upgrade path: no container may keep a writable host mount.
	for i := range reg.Branches {
		if _, err := removeLegacyMirror(reg, &reg.Branches[i]); err != nil {
			return fmt.Errorf("remove legacy mirror on %s: %w", reg.Branches[i].IncusName, err)
		}
	}

	cur, err := mirror.LoadState()
	if err != nil {
		return err
	}
	reconfigure := cur != nil && cur.Container == containerName && cur.Target == targetPath

	// Single active mirror: anything else that is configured is taken over —
	// stopped and reverted — before this target is snapshotted. This is what
	// lets the TUI switch the mirror between containers in one gesture.
	if cur != nil && !reconfigure {
		if _, err := stopActiveMirror(false); err != nil {
			return err
		}
	}

	// Snapshot the pre-mirror state of the host target so `mirror off` can
	// restore it. A cancel (or an error) here leaves everything as it was —
	// including, on a reconfigure, the mirror that is already running. A
	// cancel returns nil: the user declined, not a failure.
	if cancelled, err := captureMirrorSnapshot(targetPath, br.Slug); err != nil {
		return err
	} else if cancelled {
		return nil
	}

	if err := reconcileWatcherBinary(containerName, version, false); err != nil {
		return err
	}

	// mkdir -p the target dir as the user (in the VM this writes through
	// virtiofs to the Mac with the user's uid).
	if err := os.MkdirAll(targetPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", targetPath, err)
	}

	if reconfigure {
		// Same container, same target: restart the daemon with the new
		// settings, without a revert. Done last, after everything that can
		// fail, so an error above leaves the running mirror untouched rather
		// than tombstoned.
		cur.Stopping = true
		if err := mirror.SaveState(cur); err != nil {
			return err
		}
		if err := mirror.StopDaemon(mirrorStopTimeout); err != nil {
			return err
		}
	}

	st := &mirror.State{
		Session:    mirror.NewSessionID(),
		Container:  containerName,
		Slug:       br.Slug,
		Alias:      alias,
		Target:     targetPath,
		NoSkiplist: noSkiplist,
		StartedAt:  time.Now(),
	}
	if err := mirror.SaveState(st); err != nil {
		return fmt.Errorf("write mirror state: %w", err)
	}
	if err := spawnMirrorDaemon(); err != nil {
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
	if !waitMirrorStarted(st.Session, 15*time.Second) {
		fmt.Fprintf(cobraOutErr(), "warn: the mirror daemon has not reported a running watcher yet; check `ahjo mirror logs %s`\n", alias)
	}
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

// waitMirrorStarted polls the daemon's status file until it reports a
// connected watcher for this session.
func waitMirrorStarted(session string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := mirror.LoadStatus(); s != nil && s.Session == session &&
			(s.Phase == mirror.PhaseBootstrapping || s.Phase == mirror.PhaseLive) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
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
			q := fmt.Sprintf("target %q has uncommitted changes (%s) that the mirror will clobber (restorable via 'ahjo mirror off'); continue?", targetPath, summary)
			if !isTerminal(os.Stdin) || !promptYesNo(q) {
				fmt.Println("mirror: cancelled (target has uncommitted changes)")
				return true, nil
			}
		}
		if err := mirror.CaptureGit(targetPath, slug); err != nil {
			return false, err
		}
	case mirror.ModeFreshEmpty:
		if err := mirror.CaptureEmpty(targetPath, slug); err != nil {
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

func runMirrorOff(noRevert bool) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	stopped := false
	if reg, err := registry.Load(); err == nil {
		for i := range reg.Branches {
			removed, err := removeLegacyMirror(reg, &reg.Branches[i])
			if err != nil {
				fmt.Fprintf(cobraOutErr(), "warn: remove legacy mirror on %s: %v\n", reg.Branches[i].IncusName, err)
			}
			stopped = stopped || removed
		}
	}
	active, err := stopActiveMirror(noRevert)
	if err != nil {
		return err
	}
	if !stopped && !active {
		fmt.Println("mirror: inactive")
		return nil
	}
	fmt.Println("mirror: off")
	return nil
}

// stopActiveMirror stops the configured mirror (if any) and, unless noRevert,
// restores its host target to the pre-mirror state. Callers hold the ahjo
// lockfile.
//
// Order matters. The state is tombstoned first so no concurrent ahjo command
// revives the daemon and a daemon that is just starting exits before writing;
// only then is the daemon stopped, and only once its lock is free does the
// revert run. The quiescence the revert needs is therefore purely host-side:
// with the daemon gone nothing can write the target, whatever the container
// does.
func stopActiveMirror(noRevert bool) (stopped bool, err error) {
	st, err := mirror.LoadState()
	if err != nil {
		return false, err
	}
	if st == nil {
		return false, nil
	}
	fmt.Printf("→ stopping mirror on %s\n", st.Container)
	st.Stopping = true
	if err := mirror.SaveState(st); err != nil {
		return false, err
	}
	if err := mirror.StopDaemon(mirrorStopTimeout); err != nil {
		return false, err
	}
	if err := mirror.ClearState(); err != nil {
		return false, err
	}
	if st.Target != "" && decideRevert(noRevert, st.Target, st.Slug) {
		if err := mirror.Revert(st.Target, st.Slug); err != nil {
			return true, fmt.Errorf("revert %s: %w", st.Target, err)
		}
		fmt.Printf("mirror: reverted %s to its pre-mirror state\n", st.Target)
	}
	if status, _ := incus.ContainerStatus(st.Container); strings.EqualFold(status, "Running") {
		refreshAhjoStateByName(st.Container, st.Slug, st.Alias, "")
	}
	return true, nil
}

// runMirrorRevert restores a host target from a kept pre-mirror snapshot when no
// container/branch remains to drive `ahjo mirror off`. It works purely against
// the target's own .git (where the snapshot refs live), so it stays usable after
// the mirror container has been deleted. Refuses while a mirror is still
// configured for the target — that case must go through `ahjo mirror off` so
// the daemon stops first and the restore runs on a quiescent tree.
func runMirrorRevert(target, slugArg string) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	target = paths.Expand(strings.TrimSpace(target))
	if !filepath.IsAbs(target) {
		return fmt.Errorf("target must be absolute (got %q)", target)
	}
	target = filepath.Clean(target)

	if st, err := mirror.LoadState(); err != nil {
		return err
	} else if st != nil && sameMirrorTarget(st.Target, target) {
		return fmt.Errorf("mirror still configured on %s for %s; run `ahjo mirror off` instead", st.Container, target)
	}

	slug := strings.TrimSpace(slugArg)
	if slug == "" {
		slugs, err := mirror.ListSnapshots(target)
		if err != nil {
			return err
		}
		switch len(slugs) {
		case 0:
			fmt.Printf("mirror: nothing to revert for %s (no pre-mirror snapshot)\n", target)
			return nil
		case 1:
			slug = slugs[0]
		default:
			return fmt.Errorf("multiple snapshots at %s (%s); pass one: ahjo mirror revert %s <slug>",
				target, strings.Join(slugs, ", "), target)
		}
	}

	if !mirror.RevertPossible(target, slug) {
		fmt.Printf("mirror: nothing to revert for %s (no pre-mirror snapshot for %q)\n", target, slug)
		return nil
	}
	if err := mirror.Revert(target, slug); err != nil {
		return fmt.Errorf("revert %s: %w", target, err)
	}
	fmt.Printf("mirror: reverted %s to its pre-mirror state\n", target)
	return nil
}

// sameMirrorTarget compares two target paths, tolerating symlink differences.
func sameMirrorTarget(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// decideRevert resolves whether `mirror off` should restore the host target.
// Revert is the default — tearing a mirror down undoes its footprint — so the
// only opt-out is --no-revert, which leaves the mirrored files in place.
// Reverting needs a captured snapshot; when none exists we say so and decline
// rather than silently no-op.
func decideRevert(noRevert bool, target, slug string) bool {
	if noRevert {
		return false
	}
	if !mirror.RevertPossible(target, slug) {
		fmt.Printf("mirror: nothing to revert for %s (no pre-mirror snapshot)\n", target)
		return false
	}
	return true
}

func runMirrorStatus() error {
	st, err := mirror.LoadState()
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Println("mirror: inactive")
		return nil
	}
	alive := mirror.DaemonAlive() && !st.Stopping
	if alive {
		fmt.Println("mirror: active")
	} else {
		fmt.Println("mirror: inactive")
	}
	phase := "daemon not running (any ahjo command restarts it; `ahjo mirror off` removes it)"
	if st.Stopping {
		phase = "stopping (finish with `ahjo mirror off`)"
	} else if alive {
		phase = mirror.PhaseStarting
		if s := mirror.LoadStatus(); s != nil && s.Session == st.Session {
			phase = s.Phase
		}
	}
	fmt.Printf("  phase:     %s\n", phase)
	fmt.Printf("  alias:     %s\n", st.Alias)
	fmt.Printf("  container: %s\n", st.Container)
	fmt.Printf("  source:    %s (in container)\n", mirrorRepoPath)
	fmt.Printf("  target:    %s (host)\n", st.Target)
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
	if st, _ := mirror.LoadState(); st != nil && st.Container != br.IncusName {
		fmt.Fprintf(cobraOutErr(), "note: the configured mirror is on %s, not %s; showing the daemon log anyway\n", st.Container, br.IncusName)
	}
	logPath := paths.MirrorLogPath()
	if _, err := os.Stat(logPath); err != nil {
		return fmt.Errorf("no mirror log yet (%s); start one with `ahjo mirror %s`", logPath, alias)
	}
	// Replace our process so the user gets normal Ctrl-C semantics.
	bin, err := exec.LookPath("tail")
	if err != nil {
		return fmt.Errorf("tail not on PATH: %w", err)
	}
	return syscall.Exec(bin, []string{"tail", "-n", "200", "-F", logPath}, os.Environ())
}

// resolveDeepest resolves symlinks in p, tolerating a tail that does not
// exist yet (the target may be created by `mirror on`).
func resolveDeepest(p string) (string, error) {
	p = filepath.Clean(p)
	cur, rest := p, ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// pathWithin reports whether p is base or lies below it, returning p's
// slash-free relative components.
func pathWithin(p, base string) (rel string, ok bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// validateMirrorTarget refuses targets the mirror must never write into. The
// path is resolved first (symlinks followed, a not-yet-existing tail kept), so
// a link cannot smuggle the mirror into a refused location. Rules:
//   - not a home directory itself, nor an ancestor of one;
//   - not inside a dot-directory directly under a home (~/.ssh, ~/.ahjo,
//     ~/.claude, ~/.config, …) nor inside ~/Library;
//   - under Lima: inside the Mac home (the writable virtiofs mount);
//   - otherwise: inside $HOME.
//
// `ahjo mirror revert` deliberately skips this — it must keep working for
// snapshots taken under older rules.
func validateMirrorTarget(p string) error {
	resolved, err := resolveDeepest(p)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", p, err)
	}
	var homes []string
	addHome := func(h string) {
		if h == "" {
			return
		}
		if r, err := filepath.EvalSymlinks(h); err == nil {
			h = r
		}
		homes = append(homes, filepath.Clean(h))
	}
	if h, err := os.UserHomeDir(); err == nil {
		addHome(h)
	}
	mac, underLima := paths.MacHostHome()
	if underLima {
		addHome(mac)
	}
	for _, h := range homes {
		if _, ok := pathWithin(h, resolved); ok {
			return fmt.Errorf("target %q is (or contains) the home directory %s; pick a project directory inside it", p, h)
		}
		rel, ok := pathWithin(resolved, h)
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		if strings.HasPrefix(first, ".") || first == "Library" {
			return fmt.Errorf("target %q resolves into %s, which holds credentials or configuration; mirror targets may not live under dot-directories of the home directory or ~/Library",
				p, filepath.Join(h, first))
		}
	}
	if len(homes) == 0 {
		return fmt.Errorf("cannot determine the home directory to validate target %q", p)
	}
	required := homes[0]
	what := "the home directory"
	if underLima {
		required = homes[len(homes)-1]
		what = "the Mac home (the only host directory the VM can write)"
	}
	if _, ok := pathWithin(resolved, required); !ok {
		return fmt.Errorf("target %q is not under %s (%s)", p, what, required)
	}
	return nil
}

// reconcileWatcherBinary pushes the embedded ahjo-mirror watcher into the
// container when its version stamp differs from this CLI's (or force is set —
// the daemon asks for that when a session never produced a valid hello, which
// also covers dev builds whose stamps compare equal across incompatible
// binaries).
func reconcileWatcherBinary(containerName, expectedVersion string, force bool) error {
	if !force {
		// The binary may be missing on first activation, so a failure here is
		// expected and must not pollute the user's terminal.
		got, _ := quietContainerExec(containerName, mirrorBinPath, "-version")
		if expectedVersion != "" && strings.TrimSpace(got) == expectedVersion {
			return nil
		}
	}
	return pushBinary(containerName)
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
	// A watcher from an earlier session may still be running for a few
	// seconds; stop it and unlink the old file so the push never writes into
	// a running binary's text segment.
	_, _ = quietContainerExec(containerName, "pkill", "-x", mirrorBinName)
	_, _ = quietContainerExec(containerName, "rm", "-f", mirrorBinPath)
	if _, err := incus.FilePush(containerName, tmp, mirrorBinPath); err != nil {
		return fmt.Errorf("push %s: %w", mirrorBinPath, err)
	}
	if _, err := quietContainerExec(containerName, "chmod", "0755", mirrorBinPath); err != nil {
		return fmt.Errorf("chmod %s: %w", mirrorBinPath, err)
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

// embeddedDaemonBinary returns the host-arch-matched watcher binary from
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
		"node_modules", ".next", ".nuxt", ".svelte-kit", ".turbo",
		"__pycache__", ".venv", "venv", ".pytest_cache", ".ruff_cache", ".mypy_cache",
	}
	for i, n := range skipNames {
		if i > 0 {
			args = append(args, "-o")
		}
		args = append(args, "-name", n)
	}
	args = append(args, ")", "-prune", "-print")
	out, err := quietContainerExec(containerName, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
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

// stopAndRemoveMirror is the pre-destroy hook: if the configured mirror is on
// containerName, stop it and revert the host target to its pre-mirror state.
// Idempotent — safe to call before every destroy — so the user is never left
// with a daemon retrying against a container that is gone. It also clears a
// legacy bind-mount mirror on the container.
//
// Revert (not just snapshot-and-hint) so `ahjo rm` / `shell --update` don't
// leave the mirror's files dirtying the host dir: mirrored content originates
// in the container, so nothing is permanently lost — the lone exception is
// manual host-side edits in the target, which are discarded, matching the
// no-confirmation revert policy of `mirror off`.
func stopAndRemoveMirror(containerName string) error {
	if reg, err := registry.Load(); err == nil {
		for i := range reg.Branches {
			if reg.Branches[i].IncusName == containerName {
				if _, err := removeLegacyMirror(reg, &reg.Branches[i]); err != nil {
					fmt.Fprintf(cobraOutErr(), "warn: remove legacy mirror on %s: %v\n", containerName, err)
				}
			}
		}
	}
	st, err := mirror.LoadState()
	if err != nil || st == nil || st.Container != containerName {
		return err
	}
	_, err = stopActiveMirror(false)
	return err
}

// quietContainerExec runs `incus exec <container> -- <argv…>` capturing
// stdout; stderr is dropped rather than leaking to the user's terminal (a
// missing-file failure is expected on first activation). What runs is inside
// the container and therefore untrusted — the host daemon calls this before
// every watcher start — so it is bounded in both time and output size: a
// replaced binary can neither hang the caller nor balloon its memory.
func quietContainerExec(container string, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), containerProbeTimeout)
	defer cancel()
	args := append([]string{"exec", container, "-T", "--"}, argv...)
	cmd := exec.CommandContext(ctx, "incus", args...)
	out := &cappedBuffer{max: containerProbeMaxOutput}
	cmd.Stdout = out
	err := cmd.Run()
	if err == nil && out.overflow {
		err = fmt.Errorf("incus exec %s %s: output exceeds %d bytes", container, argv[0], containerProbeMaxOutput)
	}
	return out.b.String(), err
}

const (
	containerProbeTimeout   = 30 * time.Second
	containerProbeMaxOutput = 1 << 20
)

// cappedBuffer keeps the first max bytes written and discards the rest
// (reporting success, so the writer is not blocked or failed).
type cappedBuffer struct {
	b        strings.Builder
	max      int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	if c.b.Len() >= c.max {
		c.overflow = true
	}
	return len(p), nil
}

func primaryAlias(b *registry.Branch) string {
	if len(b.Aliases) > 0 {
		return b.Aliases[0]
	}
	return b.Slug
}
