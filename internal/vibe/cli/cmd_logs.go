package cli

import (
	"context"
	"io"
	"net/http"
	"os"

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
			req, err := http.NewRequestWithContext(ctx, "GET", "http://unix/logs", nil)
			if err != nil {
				return err
			}
			resp, err := newHTTPClient().Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, err = io.Copy(os.Stdout, resp.Body)
			return err
		},
	}
}
