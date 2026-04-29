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
	return &cobra.Command{
		Use:   "expose <repo> <branch> <container-port>",
		Short: "Add an Incus proxy device exposing a container port on 127.0.0.1",
		Args:  cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			cport, err := strconv.Atoi(args[2])
			if err != nil || cport <= 0 || cport > 65535 {
				return fmt.Errorf("invalid container port %q", args[2])
			}
			return runExpose(args[0], args[1], cport)
		},
	}
}

func runExpose(repoName, branch string, cport int) error {
	release, err := lockfile.Acquire()
	if err != nil {
		return err
	}
	defer release()

	reg, err := registry.Load()
	if err != nil {
		return err
	}
	w := reg.FindWorktree(repoName, branch)
	if w == nil {
		return fmt.Errorf("no worktree for %s/%s", repoName, branch)
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

	containerName := w.Slug + "-1"
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
