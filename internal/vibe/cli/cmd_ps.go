package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func psCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "Show the active profile (if any).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			s, err := newClient().Status(ctx)
			if err != nil {
				return err
			}
			if !s.Running {
				fmt.Println("no active profile")
				return nil
			}
			ready := "starting"
			if s.Ready {
				ready = "ready"
			}
			fmt.Printf("profile:  %s (%s)\n", s.Profile, ready)
			if s.StartedAt != nil {
				fmt.Printf("uptime:   %s\n", time.Since(s.StartedAt.AsTime()).Round(time.Second))
			}
			fmt.Printf("backend:  %s\n", s.BackendAddr)
			fmt.Printf("proxy:    %s\n", s.ProxyAddr)
			if s.Pid != 0 {
				fmt.Printf("pid:      %d\n", s.Pid)
			}
			return nil
		},
	}
}
