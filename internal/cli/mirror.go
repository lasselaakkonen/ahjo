package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/mirror"
	"github.com/lasselaakkonen/ahjo/internal/paths"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newMirrorCmd() *cobra.Command {
	var target string
	var force bool
	var daemonMode bool

	cmd := &cobra.Command{
		Use:   "mirror <alias|off|status>",
		Short: "Mirror a worktree onto the Mac so you can run the app natively",
		Long: `Activate a one-way live mirror from a worktree to a Mac directory.

  ahjo mirror <alias> [--target <path>]   activate (one active at a time)
  ahjo mirror off                         stop the active mirror
  ahjo mirror status                      show the current mirror (if any)

The watcher runs on the Lima VM. Container writes show up in the worktree via
the existing read-write bind-mount, so VM-side fsnotify catches them with no
in-container daemon.

The target path is remembered per-repo after the first activation; subsequent
calls without --target reuse it. The first sync runs ` + "`rsync --delete-during`" + `,
so a target dir with uncommitted git changes is rejected unless --force is
given. Sync respects the worktree's .gitignore at every level, so Mac-side
build artifacts in gitignored paths (node_modules/, dist/, etc.) are not
clobbered.`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(_ *cobra.Command, args []string) error {
			if daemonMode {
				return runMirrorDaemon()
			}
			if len(args) == 0 {
				return runMirrorStatus()
			}
			switch args[0] {
			case "off":
				return runMirrorOff()
			case "status":
				return runMirrorStatus()
			default:
				return runMirrorActivate(args[0], target, force)
			}
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Mac path to mirror into (defaults to the per-repo remembered value)")
	cmd.Flags().BoolVar(&force, "force", false, "activate even if the target dir has uncommitted git changes")
	cmd.Flags().BoolVar(&daemonMode, "daemon", false, "internal: run as the watcher daemon")
	_ = cmd.Flags().MarkHidden("daemon")
	return cmd
}

func runMirrorActivate(alias, target string, force bool) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	w := reg.FindWorktreeByAlias(alias)
	if w == nil {
		return fmt.Errorf("no worktree with alias %q; create with `ahjo new`", alias)
	}
	repo := reg.FindRepo(w.Repo)
	if repo == nil {
		return fmt.Errorf("internal: worktree %q references missing repo %q", alias, w.Repo)
	}

	targetPath := strings.TrimSpace(target)
	if targetPath == "" {
		targetPath = repo.MacMirrorTarget
	}
	if targetPath == "" {
		return fmt.Errorf("no target dir set for repo %q; pass --target </absolute/path>", repo.Name)
	}
	targetPath = expandMirrorTarget(targetPath)
	if !filepath.IsAbs(targetPath) {
		return fmt.Errorf("--target must be absolute (got %q)", targetPath)
	}
	if err := validateMirrorTarget(targetPath); err != nil {
		return err
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		return fmt.Errorf("rsync not on PATH inside the VM: install with `sudo apt-get install -y rsync` and re-run")
	}
	if err := os.MkdirAll(targetPath, 0o755); err != nil {
		return fmt.Errorf("mkdir target: %w", err)
	}
	if !force {
		if err := requireCleanTargetGit(targetPath); err != nil {
			return err
		}
	}

	if err := stopActiveMirror(); err != nil {
		fmt.Fprintf(cobraOutErr(), "warn: stop existing mirror: %v\n", err)
	}

	if repo.MacMirrorTarget != targetPath {
		repo.MacMirrorTarget = targetPath
		if err := reg.Save(); err != nil {
			return err
		}
	}

	fmt.Printf("→ initial sync %s → %s\n", w.WorktreePath, targetPath)
	if err := mirror.Bootstrap(w.WorktreePath, targetPath, cobraOut()); err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	st := &mirror.State{
		Alias:        alias,
		Slug:         w.Slug,
		WorktreePath: w.WorktreePath,
		Target:       targetPath,
		StartedAt:    time.Now(),
	}
	if err := st.Save(); err != nil {
		return err
	}
	pid, err := spawnMirrorDaemon()
	if err != nil {
		_ = mirror.Clear()
		return fmt.Errorf("launch daemon: %w", err)
	}
	st.PID = pid
	if err := st.Save(); err != nil {
		return err
	}
	fmt.Printf("mirror: %s → %s (pid %d, log %s)\n", alias, targetPath, pid, mirror.LogPath())
	return nil
}

func runMirrorOff() error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()
	if err := stopActiveMirror(); err != nil {
		return err
	}
	if err := mirror.Clear(); err != nil {
		return err
	}
	fmt.Println("mirror: off")
	return nil
}

func runMirrorStatus() error {
	st, err := mirror.Load()
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Println("mirror: inactive")
		return nil
	}
	alive := mirror.PIDAlive(st.PID)
	if alive {
		fmt.Printf("mirror: active\n")
	} else {
		fmt.Printf("mirror: stale (daemon not running)\n")
	}
	fmt.Printf("  alias:    %s\n", st.Alias)
	fmt.Printf("  worktree: %s\n", st.WorktreePath)
	fmt.Printf("  target:   %s\n", st.Target)
	fmt.Printf("  pid:      %d\n", st.PID)
	fmt.Printf("  started:  %s\n", st.StartedAt.Format(time.RFC3339))
	if !alive {
		// Per the plan: stale PIDs lie. Clean up so the next status call
		// reports inactive, and the user's per-repo target stays in registry.
		_ = mirror.Clear()
	}
	return nil
}

func runMirrorDaemon() error {
	st, err := mirror.Load()
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("no mirror state; run `ahjo mirror <alias>` first")
	}
	logF, err := os.OpenFile(mirror.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logF.Close()
	ctx, cancel := mirror.InstallSignalHandler()
	defer cancel()
	return mirror.RunDaemon(ctx, st.WorktreePath, st.Target, logF)
}

// stopActiveMirror SIGTERMs the recorded daemon PID (if alive) and waits up
// to 3s for exit, then SIGKILLs as a last resort. Idempotent.
func stopActiveMirror() error {
	st, err := mirror.Load()
	if err != nil {
		return err
	}
	if st == nil || !mirror.PIDAlive(st.PID) {
		return nil
	}
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGTERM pid %d: %w", st.PID, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !mirror.PIDAlive(st.PID) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(st.PID, syscall.SIGKILL)
	return nil
}

// expandMirrorTarget expands a leading `~` against the Mac host home (as
// seen via virtiofs from inside the Lima VM) when running under Lima, and
// against the regular home on bare-metal Linux. Without this, `~/code/foo`
// inside the VM would resolve to /home/<linux-user>/code/foo, NOT the user's
// Mac path.
func expandMirrorTarget(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if mac, ok := paths.MacHostHome(); ok {
		switch {
		case p == "~":
			return mac
		case strings.HasPrefix(p, "~/"):
			return filepath.Join(mac, p[2:])
		}
		return p
	}
	return paths.Expand(p)
}

// validateMirrorTarget refuses paths that would point outside the Mac
// virtiofs writable mount (when running under Lima) or that nest inside
// ~/.ahjo/. Bare-metal Linux only enforces the second guard.
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

// requireCleanTargetGit blocks first-activation when target is a git working
// tree with uncommitted changes — `rsync --delete-during` would clobber them.
// Best-effort: if git is missing or fails, we don't block (force is the
// escape hatch).
func requireCleanTargetGit(p string) error {
	if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
		return nil
	}
	out, err := exec.Command("git", "-C", p, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil
	}
	return fmt.Errorf("target %q has uncommitted changes; commit/stash first or pass --force", p)
}

// spawnMirrorDaemon detaches `ahjo mirror --daemon` into its own session
// so it survives the limactl shell that started it. stdout/stderr go to the
// mirror log file; stdin is closed.
func spawnMirrorDaemon() (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("os.Executable: %w", err)
	}
	logF, err := os.OpenFile(mirror.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer logF.Close()
	cmd := exec.Command(self, "mirror", "--daemon")
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Reap the child if/when it exits so it doesn't linger as a zombie under
	// this short-lived parent. The PID is recorded before this returns, so a
	// later kill -0 / SIGTERM still works for the lifetime of the daemon.
	go func() { _ = cmd.Wait() }()
	// Settle window: surface immediate launch failures (rsync missing,
	// state file races) before we tell the user it's running.
	time.Sleep(200 * time.Millisecond)
	if !mirror.PIDAlive(cmd.Process.Pid) {
		return 0, fmt.Errorf("daemon exited immediately; see %s", mirror.LogPath())
	}
	return cmd.Process.Pid, nil
}
