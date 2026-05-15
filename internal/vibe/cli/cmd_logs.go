package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs",
		Short: "Show recent llama-server logs.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			lines, err := newClient().Logs(ctx)
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
