package cli

import (
	"context"
	"fmt"

	"github.com/gallowaysoftware/vibe/internal/ipc"
	"github.com/spf13/cobra"
)

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <profile>",
		Short: "Start a profile (auto-spawns the daemon if needed).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			var resp ipc.StartResponse
			if err := postJSON(ctx, "/start", ipc.StartRequest{Profile: args[0]}, &resp); err != nil {
				return err
			}
			fmt.Printf("started %s\n", resp.Status.Profile)
			fmt.Printf("  backend: %s\n", resp.Status.BackendAddr)
			fmt.Printf("  proxy:   %s\n", resp.Status.ProxyAddr)
			if resp.Frontend != nil {
				fmt.Printf("  frontend: %s\n", resp.Frontend.App)
				fmt.Printf("  wrote:    %s\n", resp.Frontend.WroteFile)
				if resp.Frontend.RestartRequired {
					fmt.Printf("  WARNING: %s does not hot-reload its config — restart it to pick up the new endpoint\n", resp.Frontend.App)
				}
			}
			return nil
		},
	}
}
