package cli

import (
	"context"
	"fmt"

	"github.com/gallowaysoftware/vibe/internal/ipc"
	"github.com/spf13/cobra"
)

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the active profile.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			var resp ipc.StopResponse
			if err := postJSON(ctx, "/stop", nil, &resp); err != nil {
				return err
			}
			fmt.Println("stopped")
			return nil
		},
	}
}
