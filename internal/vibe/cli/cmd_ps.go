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
		Short: "Show the active profile and every running service.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			active, services, err := newClient().StatusFull(ctx)
			if err != nil {
				return err
			}

			// Active profile section.
			if active == nil || !active.Running {
				fmt.Println("no active profile")
			} else {
				ready := "starting"
				if active.Ready {
					ready = "ready"
				}
				fmt.Printf("active:   %s (%s)\n", active.Profile, ready)
				if active.StartedAt != nil {
					fmt.Printf("  uptime: %s\n", time.Since(active.StartedAt.AsTime()).Round(time.Second))
				}
				if active.BackendAddr != "" {
					fmt.Printf("  backend: %s\n", active.BackendAddr)
				}
				if active.ProxyAddr != "" {
					fmt.Printf("  proxy:   %s\n", active.ProxyAddr)
				}
				if active.Pid != 0 {
					fmt.Printf("  pid:     %d\n", active.Pid)
				}
			}

			// Services section. Empty in the legacy single-profile
			// case; one line per running sidecar otherwise. Backend
			// addr is the published port the caller actually hits
			// (services bypass the proxy).
			if len(services) > 0 {
				fmt.Println()
				fmt.Printf("services: %d running\n", len(services))
				for _, s := range services {
					ready := "starting"
					if s.Ready {
						ready = "ready"
					}
					uptime := ""
					if s.StartedAt != nil {
						uptime = " " + time.Since(s.StartedAt.AsTime()).Round(time.Second).String()
					}
					fmt.Printf("  - %-24s %s%s  %s (pid %d)\n",
						s.Profile, ready, uptime, s.BackendAddr, s.Pid)
				}
			}
			return nil
		},
	}
}
