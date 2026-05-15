package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/spf13/cobra"
)

func daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the vibe daemon (foreground). Auto-spawned by other commands when not running.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			cfg, err := daemon.LoadConfig()
			if err != nil {
				return err
			}
			closer, err := daemon.SetupLogging()
			if err != nil {
				return err
			}
			defer closer.Close()
			return daemon.New(cfg).Run(ctx)
		},
	}
}
