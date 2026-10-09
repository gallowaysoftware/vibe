package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/agentpool"
	"github.com/spf13/cobra"
)

func agentpoolCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "agentpool",
		Short: "Serve pooled model ids that hand each agent conversation to a free member (foreground).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := agentpool.LoadConfig(configPath)
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			srv := &http.Server{
				Addr:              cfg.Listen,
				Handler:           agentpool.NewServer(cfg, nil, log),
				ReadHeaderTimeout: 10 * time.Second,
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			errc := make(chan error, 1)
			go func() { errc <- srv.ListenAndServe() }()
			log.Info("agentpool: serving", "listen", cfg.Listen, "upstream", cfg.Upstream, "pools", len(cfg.Pools))
			select {
			case err := <-errc:
				return err
			case <-ctx.Done():
			}
			// In-flight completions are agent turns; give them time to finish
			// rather than cutting a stream mid-edit on a redeploy.
			shutdownCtx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
			defer stop()
			if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "/etc/vibe/agentpool.yaml", "agentpool config file")
	return cmd
}
