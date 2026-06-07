package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pipelines under $XDG_CONFIG_HOME/vamp/pipelines/.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			dir := vamp.PipelinesDir()
			entries, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(out, "no pipelines dir (drop YAML files in %s)\n", dir)
				return nil
			}
			if err != nil {
				return err
			}
			any := false
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
					continue
				}
				any = true
				path := filepath.Join(dir, e.Name())
				p, err := vamp.LoadPipeline(path)
				if err != nil {
					fmt.Fprintf(out, "%-30s  INVALID: %v\n", e.Name(), err)
					continue
				}
				desc := p.Description
				if desc == "" {
					desc = "(no description)"
				}
				fmt.Fprintf(out, "%-20s  %s\n", p.Name, desc)
			}
			if !any {
				fmt.Fprintf(out, "no pipelines in %s\n", dir)
			}
			return nil
		},
	}
}
