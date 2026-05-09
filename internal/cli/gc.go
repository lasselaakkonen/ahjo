package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/registry"
)

func newGCCmd() *cobra.Command {
	var olderThan time.Duration
	var dryRun bool
	var prune bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Report (and optionally remove) stale branches",
		RunE: func(_ *cobra.Command, _ []string) error {
			reg, err := registry.Load()
			if err != nil {
				return err
			}
			cutoff := time.Now().Add(-olderThan)
			var stale []registry.Branch
			for _, br := range reg.Branches {
				if br.IsDefault {
					// Never garbage-collect the default container; it's the
					// COW source for every other branch in the repo.
					continue
				}
				if olderThan > 0 && br.CreatedAt.After(cutoff) {
					continue
				}
				stale = append(stale, br)
			}
			if len(stale) == 0 {
				fmt.Println("no candidates")
				return nil
			}
			for _, br := range stale {
				fmt.Printf("%s  (created %s)\n", br.Aliases[0], br.CreatedAt.Format(time.RFC3339))
			}
			if dryRun || !prune {
				fmt.Println("(dry run; pass --prune to delete)")
				return nil
			}
			for _, br := range stale {
				alias := br.Aliases[0]
				if err := runRm(alias, false); err != nil {
					fmt.Fprintf(cobraOutErr(), "warn: rm %s: %v\n", alias, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 24*time.Hour, "only consider branches older than this")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report only")
	cmd.Flags().BoolVar(&prune, "prune", false, "actually remove stale branches")
	return cmd
}
