package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List all registered worktrees and their container state",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			reg, err := registry.Load()
			if err != nil {
				return err
			}
			if len(reg.Worktrees) == 0 {
				fmt.Println("no worktrees")
				return nil
			}
			tw := tabwriter.NewWriter(cobraOut(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ALIASES\tSLUG\tSSH PORT\tCONTAINER\tCREATED")
			for _, w := range reg.Worktrees {
				name := w.Slug + "-1"
				state := "missing"
				if exists, err := incus.ContainerExists(name); err == nil && exists {
					state = "present"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
					strings.Join(w.Aliases, ","), w.Slug, w.SSHPort, state,
					w.CreatedAt.Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
}
