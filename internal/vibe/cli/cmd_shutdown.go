package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func shutdownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Stop the active profile (if any) and shut down the daemon.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := pingDaemon(200 * time.Millisecond); err != nil {
				// Already-down is the desired end state for a teardown
				// command; report it and exit 0 so `vibe shutdown` is
				// idempotent (no `|| true` needed in scripts).
				fmt.Fprintln(cmd.OutOrStdout(), "daemon not running")
				return nil
			}
			if err := newClient().Shutdown(ctx); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon shutting down")
			return nil
		},
	}
}
