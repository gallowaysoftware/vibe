package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs [profile]",
		Short: "Show recent logs from a running profile's supervisor.",
		Long: `Tails the most recent backend log lines from a supervised
process. With no argument, returns the active profile's logs
(legacy behavior). With a profile name, returns that service-mode
profile's logs — handy for diagnosing a docker-managed sidecar
that wedged itself mid-pipeline.

Examples:

  vibe logs                # active profile
  vibe logs searxng        # the searxng service
  vibe logs embed_bge_large
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			lines, err := newClient().LogsForProfile(ctx, name)
			if err != nil {
				return err
			}
			for _, l := range lines {
				fmt.Println(l)
			}
			return nil
		},
	}
}
