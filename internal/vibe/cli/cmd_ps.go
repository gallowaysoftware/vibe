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
			// `ps` is a read-only status query, so it must not spawn a
			// daemon as a side effect — that would make asking "is anything
			// running?" itself start a background process. If the daemon
			// isn't up, nothing is running, full stop.
			if err := pingDaemon(500 * time.Millisecond); err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "no active profile (daemon not running)")
				return nil
			}
			active, services, err := newClient().StatusFull(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			// Active profile section.
			if active == nil || !active.Running {
				fmt.Fprintln(out, "no active profile")
			} else {
				ready := "starting"
				if active.Ready {
					ready = "ready"
				}
				fmt.Fprintf(out, "active:   %s (%s)\n", active.Profile, ready)
				if active.StartedAt != nil {
					fmt.Fprintf(out, "  uptime: %s\n", time.Since(active.StartedAt.AsTime()).Round(time.Second))
				}
				if active.BackendAddr != "" {
					fmt.Fprintf(out, "  backend: %s\n", active.BackendAddr)
				}
				if active.ProxyAddr != "" {
					fmt.Fprintf(out, "  proxy:   %s\n", active.ProxyAddr)
				}
				if active.Pid != 0 {
					fmt.Fprintf(out, "  pid:     %d\n", active.Pid)
				}
			}

			// Services section. Empty in the legacy single-profile
			// case; one line per running sidecar otherwise. Backend
			// addr is the published port the caller actually hits
			// (services bypass the proxy).
			if len(services) > 0 {
				fmt.Fprintln(out)
				fmt.Fprintf(out, "services: %d running\n", len(services))
				for _, s := range services {
					ready := "starting"
					if s.Ready {
						ready = "ready"
					}
					uptime := ""
					if s.StartedAt != nil {
						uptime = " " + time.Since(s.StartedAt.AsTime()).Round(time.Second).String()
					}
					fmt.Fprintf(out, "  - %-24s %s%s  %s (pid %d)\n",
						s.Profile, ready, uptime, s.BackendAddr, s.Pid)
				}
			}
			return nil
		},
	}
}
