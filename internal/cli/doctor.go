package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lasselaakkonen/ahjo/internal/preflight"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Read-only host environment check (use `ahjo init` for setup)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ps := preflight.Run()
			for _, p := range ps {
				fmt.Println(preflight.Format(p))
			}
			if preflight.Worst(ps) >= preflight.Fail {
				return fmt.Errorf("doctor found failures (run `ahjo init` to fix)")
			}
			return nil
		},
	}
}
