package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// schemaCmd exposes `vamp schema`, a stdout-by-default renderer for the
// pipeline JSON Schema. Editor extensions that speak the
// yaml-language-server protocol (VS Code's RedHat YAML, Helix, vim's coc,
// IntelliJ) consume the file via a `# yaml-language-server: $schema=...`
// directive at the top of pipeline.yaml; we keep the command stdout-first
// so the canonical "regenerate the checked-in schema" recipe is a single
// shell redirect (`vamp schema > vamp.schema.json`) without baking write
// behavior into the default path.
func schemaCmd() *cobra.Command {
	var outFlag string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Emit the vamp pipeline JSON Schema (draft-07).",
		Long: "schema prints a JSON Schema document describing pipeline.yaml " +
			"to stdout (or --out <file>). The schema covers every Stage field, " +
			"every StageType, RetryPolicy, ForeachSpec, and InputSpec — point " +
			"yaml-language-server at the rendered file with a " +
			"`# yaml-language-server: $schema=./vamp.schema.json` directive at " +
			"the top of your pipeline YAML to get editor validation + " +
			"autocomplete.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := vamp.SchemaJSON()
			if err != nil {
				return err
			}
			if outFlag == "" {
				_, err := cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(outFlag, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", outFlag, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outFlag, "out", "", "Write the schema to this file instead of stdout.")
	return cmd
}
