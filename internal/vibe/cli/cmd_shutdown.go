package cli

import (
	"context"
	"errors"
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
				return errors.New("daemon is not running")
			}
			if err := newClient().Shutdown(ctx); err != nil {
				return err
			}
			fmt.Println("daemon shutting down")
			return nil
		},
	}
}
