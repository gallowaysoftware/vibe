package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <pipeline.yaml>",
		Short: "Load and validate a pipeline file (does not run it).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := vamp.LoadPipeline(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("OK: %s — %d stages\n", p.Name, len(p.Stages))
			for i, s := range p.Stages {
				fmt.Printf("  %d. %s (capability: %s) -> %s\n", i+1, s.ID, s.Capability, s.Output)
			}
			return nil
		},
	}
}
