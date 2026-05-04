package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/lockfile"
	"github.com/lasselaakkonen/ahjo/internal/ports"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newExposeCmd() *cobra.Command {
	var sync bool
	cmd := &cobra.Command{
		Use:   "expose <alias> [<container-port>]",
		Short: "Add an Incus proxy device exposing a container port on 127.0.0.1",
		Long: `Manually expose <container-port> on 127.0.0.1 (Mac-side via Lima auto-forward),
or with --sync, reconcile auto-expose proxy devices to match the container's
current set of TCP loopback listeners (ports >= [auto_expose].min_port).

--sync is what you run after starting docker-compose / a dev server inside the
container so newly-bound ports surface to the host without restarting the
shell. Manual expose entries are never touched by --sync.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if sync {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if sync {
				return runExposeSync(args[0])
			}
			cport, err := strconv.Atoi(args[1])
			if err != nil || cport <= 0 || cport > 65535 {
				return fmt.Errorf("invalid container port %q", args[1])
			}
			return runExpose(args[0], cport)
		},
	}
	cmd.Flags().BoolVar(&sync, "sync", false, "reconcile auto-expose proxy devices to the container's current listeners")
	return cmd
}

func runExposeSync(alias string) error {
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
		return fmt.Errorf("no worktree with alias %q", alias)
	}
	return reconcileAutoExpose(cobraOut(), w)
}

func runExpose(alias string, cport int) error {
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
		return fmt.Errorf("no worktree with alias %q", alias)
	}

	containerName, err := resolveContainerName(w)
	if err != nil {
		return err
	}

	pp, err := ports.Load()
	if err != nil {
		return err
	}
	purpose := fmt.Sprintf("%s%d", ports.ExposePrefix, cport)
	hostPort, err := pp.Allocate(w.Slug, purpose)
	if err != nil {
		return err
	}
	if err := pp.Save(); err != nil {
		return err
	}

	deviceName := fmt.Sprintf("ahjo-expose-%d", cport)
	if err := incus.AddProxyDevice(
		containerName, deviceName,
		fmt.Sprintf("tcp:127.0.0.1:%d", hostPort),
		fmt.Sprintf("tcp:127.0.0.1:%d", cport),
	); err != nil {
		return err
	}
	fmt.Printf("container :%d -> 127.0.0.1:%d\n", cport, hostPort)
	return nil
}
