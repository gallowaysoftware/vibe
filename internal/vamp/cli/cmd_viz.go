package cli

import (
	"fmt"
	"os"

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
			// --format is stubbed for Phase 1: only mermaid is supported.
			// Reject anything else with a clear error rather than silently
			// emitting Mermaid so users can't be surprised when a future
			// version actually adds DOT support.
			switch formatFlag {
			case "", "mermaid":
				// ok
			case "dot":
				return fmt.Errorf("--format=dot is not implemented yet (Phase 1 supports only mermaid)")
			default:
				return fmt.Errorf("--format %q is not supported (allowed: mermaid)", formatFlag)
			}
			p, err := vamp.LoadPipeline(args[0])
			if err != nil {
				return err
			}
			rendered := vamp.RenderMermaid(p, vamp.VizOptions{ShowInputs: showInputsFlag})
			if outFlag == "" {
				_, err := fmt.Fprint(cmd.OutOrStdout(), rendered)
				return err
			}
			// 0644 so the file is readable but not executable; the renderer
			// output is plain text the user typically commits next to the
			// pipeline yaml.
			if err := os.WriteFile(outFlag, []byte(rendered), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", outFlag, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outFlag, "out", "", "Write rendered Mermaid to this file instead of stdout.")
	cmd.Flags().StringVar(&formatFlag, "format", "mermaid", "Output format. Only \"mermaid\" is supported in Phase 1; \"dot\" is reserved for future use.")
	cmd.Flags().BoolVar(&showInputsFlag, "show-inputs", false, "Include the pipeline's declared inputs as a `subgraph inputs` block at the top of the diagram.")
	return cmd
}
