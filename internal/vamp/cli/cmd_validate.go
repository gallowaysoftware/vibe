package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func validateCmd() *cobra.Command {
	var skipCaps bool
	cmd := &cobra.Command{
		Use:               "validate <pipeline.yaml>",
		Short:             "Load and validate a pipeline file (does not run it).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePipelineFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			pipelinePath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			p, err := vamp.LoadPipeline(pipelinePath)
			if err != nil {
				return err
			}
			// Load capabilities unless the user opted out. Missing
			// capabilities.yaml is not fatal on its own — the user may be
			// editing a pipeline before they've set up their config — but a
			// stage that references an unmapped capability IS surfaced.
			// --no-capability-check skips loading entirely so a pipeline
			// authored against a different machine's config can still be
			// shape-validated here.
			var caps *vamp.Capabilities
			if !skipCaps {
				caps, err = vamp.LoadCapabilities()
				if err != nil {
					return fmt.Errorf("validate: %w (use --no-capability-check to skip)", err)
				}
			}
			if err := vamp.CheckRunnable(p, filepath.Dir(pipelinePath), caps); err != nil {
				return err
			}
			stageWord := "stages"
			if len(p.Stages) == 1 {
				stageWord = "stage"
			}
			fmt.Printf("OK: %s — %d %s\n", p.Name, len(p.Stages), stageWord)
			for i, s := range p.Stages {
				fmt.Printf("  %d. %s (capability: %s) -> %s\n", i+1, s.ID, s.Capability, s.Output)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipCaps, "no-capability-check", false, "Skip loading capabilities.yaml and verifying every stage's capability is mapped. Useful when validating a pipeline authored for a different machine.")
	return cmd
}
