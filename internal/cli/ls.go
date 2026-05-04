package cli

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/incus"
	"github.com/lasselaakkonen/ahjo/internal/ports"
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
			pp, err := ports.Load()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cobraOut(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ALIASES\tSLUG\tSSH PORT\tCONTAINER\tEXPOSED\tCREATED")
			for _, w := range reg.Worktrees {
				name := w.Slug + "-1"
				if w.IncusName != "" {
					name = w.IncusName
				}
				state := "missing"
				if exists, err := incus.ContainerExists(name); err == nil && exists {
					state = "present"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
					strings.Join(w.Aliases, ","), w.Slug, w.SSHPort, state,
					formatExposed(pp.AllocationsForSlug(w.Slug)),
					w.CreatedAt.Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
}

// formatExposed renders the worktree's expose-/auto- allocations as a
// comma-separated list of `<container>->127.0.0.1:<host>` entries, sorted by
// container port. Returns "-" when there are no exposes.
func formatExposed(allocs []ports.Allocation) string {
	type row struct{ cport, hport int }
	var rows []row
	for _, a := range allocs {
		var prefix string
		switch {
		case strings.HasPrefix(a.Purpose, ports.AutoExposePrefix):
			prefix = ports.AutoExposePrefix
		case strings.HasPrefix(a.Purpose, ports.ExposePrefix):
			prefix = ports.ExposePrefix
		default:
			continue
		}
		var cport int
		if _, err := fmt.Sscanf(strings.TrimPrefix(a.Purpose, prefix), "%d", &cport); err != nil {
			continue
		}
		rows = append(rows, row{cport: cport, hport: a.Port})
	}
	if len(rows) == 0 {
		return "-"
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cport < rows[j].cport })
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf(":%d->127.0.0.1:%d", r.cport, r.hport))
	}
	return strings.Join(parts, ",")
}
