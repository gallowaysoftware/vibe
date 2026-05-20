package cli

import (
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
			return ValidatePipeline(p, filepath.Dir(pipelinePath), skipCaps, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&skipCaps, "no-capability-check", false, "Skip loading capabilities.yaml and verifying every stage's capability is mapped. Useful when validating a pipeline authored for a different machine.")
	return cmd
}
