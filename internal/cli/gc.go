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
		Short: "Report (and optionally remove) stale worktrees",
		RunE: func(_ *cobra.Command, _ []string) error {
			reg, err := registry.Load()
			if err != nil {
				return err
			}
			cutoff := time.Now().Add(-olderThan)
			var stale []registry.Worktree
			for _, w := range reg.Worktrees {
				if olderThan > 0 && w.CreatedAt.After(cutoff) {
					continue
				}
				stale = append(stale, w)
			}
			if len(stale) == 0 {
				fmt.Println("no candidates")
				return nil
			}
			for _, w := range stale {
				fmt.Printf("%s/%s  (created %s)\n", w.Repo, w.Branch, w.CreatedAt.Format(time.RFC3339))
			}
			if dryRun || !prune {
				fmt.Println("(dry run; pass --prune to delete)")
				return nil
			}
			for _, w := range stale {
				if err := runRm(w.Repo, w.Branch); err != nil {
					fmt.Fprintf(cobraOutErr(), "warn: rm %s/%s: %v\n", w.Repo, w.Branch, err)
				}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 24*time.Hour, "only consider worktrees older than this")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report only")
	cmd.Flags().BoolVar(&prune, "prune", false, "actually remove stale worktrees")
	return cmd
}
