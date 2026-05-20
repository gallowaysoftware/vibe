package cli

import (
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func vizCmd() *cobra.Command {
	var (
		outFlag        string
		formatFlag     string
		showInputsFlag bool
	)
	cmd := &cobra.Command{
		Use:   "viz <pipeline.yaml>",
		Short: "Emit a Mermaid flowchart of the pipeline's DAG.",
		Long: "viz loads a pipeline file and writes a Mermaid `flowchart TD` block " +
			"to stdout (or `--out <file>`), suitable for pasting into a markdown " +
			"document or the Mermaid live editor. Foreach stages are rendered with " +
			"a dotted edge from the upstream JSON source; nodes are coloured by " +
			"stage type via Mermaid class definitions.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := vamp.LoadPipeline(args[0])
			if err != nil {
				return err
			}
			return VizPipeline(p, VizOptions{OutFile: outFlag, Format: formatFlag, ShowInputs: showInputsFlag}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&outFlag, "out", "", "Write rendered Mermaid to this file instead of stdout.")
	cmd.Flags().StringVar(&formatFlag, "format", "mermaid", "Output format. Only \"mermaid\" is supported in Phase 1; \"dot\" is reserved for future use.")
	cmd.Flags().BoolVar(&showInputsFlag, "show-inputs", false, "Include the pipeline's declared inputs as a `subgraph inputs` block at the top of the diagram.")
	return cmd
}
