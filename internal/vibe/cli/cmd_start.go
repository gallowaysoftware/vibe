package cli

import (
	"context"
	"fmt"

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
			r, err := newClient().Start(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("started %s\n", r.Status.Profile)
			fmt.Printf("  backend: %s\n", r.Status.BackendAddr)
			fmt.Printf("  proxy:   %s\n", r.Status.ProxyAddr)
			if r.Frontend != nil {
				fmt.Printf("  frontend: %s\n", r.Frontend.App)
				fmt.Printf("  wrote:    %s\n", r.Frontend.WroteFile)
				if r.Frontend.RestartRequired {
					fmt.Printf("  WARNING: %s does not hot-reload its config — restart it to pick up the new endpoint\n", r.Frontend.App)
				}
			}
			return nil
		},
	}
}
