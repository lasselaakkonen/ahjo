package cli

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/mirror"
)

const mirrorDisabledMsg = "mirror: temporarily disabled in this build (Phase 1 of no-more-worktrees). Phase 2 restores it via storage-pool internal paths; tracking issue / design: designdocs/no-more-worktrees.md."

func newMirrorCmd() *cobra.Command {
	var target string
	var force bool
	var daemonMode bool

	cmd := &cobra.Command{
		Use:   "mirror <alias|off|status>",
		Short: "Mirror a branch onto the Mac so you can run the app natively (DISABLED in Phase 1)",
		Long: `Phase 1 of the no-more-worktrees redesign disables ` + "`ahjo mirror`" + `: the
old VM-resident worktree path is gone, and Phase 2's storage-pool-internal-path
replacement is not yet wired. ` + "`status`" + ` and ` + "`off`" + ` continue to work so an
already-running daemon (left over from a previous build) can be stopped.`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(_ *cobra.Command, args []string) error {
			if daemonMode {
				return fmt.Errorf(mirrorDisabledMsg)
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
				return fmt.Errorf(mirrorDisabledMsg)
			}
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "Mac path to mirror into (Phase 1: ignored)")
	cmd.Flags().BoolVar(&force, "force", false, "Phase 1: ignored")
	cmd.Flags().BoolVar(&daemonMode, "daemon", false, "internal: run as the watcher daemon (Phase 1: refused)")
	_ = cmd.Flags().MarkHidden("daemon")
	return cmd
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
	fmt.Printf("  alias:     %s\n", st.Alias)
	fmt.Printf("  container: %s\n", st.Container)
	fmt.Printf("  target:    %s\n", st.Target)
	fmt.Printf("  pid:       %d\n", st.PID)
	fmt.Printf("  started:   %s\n", st.StartedAt.Format(time.RFC3339))
	if !alive {
		// Per the plan: stale PIDs lie. Clean up so the next status call
		// reports inactive, and the user's per-repo target stays in registry.
		_ = mirror.Clear()
	}
	return nil
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
