package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

func startCmd() *cobra.Command {
	var noVRAMCheck bool
	cmd := &cobra.Command{
		Use:               "start <profile>",
		Short:             "Start a profile (auto-spawns the daemon if needed).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeProfileNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			// Idempotent: PHASE_DONE immediately if the profile has no HF block
			// or the model is already cached at the right size.
			if err := pullProfile(ctx, cmd.OutOrStdout(), args[0]); err != nil {
				return err
			}
			// Tail the daemon log during the Start RPC so the user sees
			// vram check, model load, backend-ready, compose-up, etc.
			// land in real time instead of staring at silence.
			cancelTail := startProgressTail(cmd.OutOrStdout())
			defer cancelTail()
			r, err := newClient().StartWithOptions(ctx, args[0], vibeclient.StartOptions{
				NoVRAMCheck: noVRAMCheck,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "started %s\n", r.Status.Profile)
			fmt.Fprintf(out, "  backend: %s\n", r.Status.BackendAddr)
			// Service-mode profiles don't go through the proxy — the
			// daemon returns ProxyAddr empty in that case. Skip the
			// line to avoid surfacing a misleading "proxy: " line for
			// a sidecar that callers reach via BackendAddr directly.
			if r.Status.ProxyAddr != "" {
				fmt.Fprintf(out, "  proxy:   %s\n", r.Status.ProxyAddr)
			}
			if r.Frontend != nil {
				// external = config rendered, user launches the binary;
				// managed / docker-compose = vibe also launched the binary.
				// The label + trailing note differ accordingly. The profile
				// name doubles as the human-readable frontend label here —
				// the historical `frontend.app` cosmetic field was dropped.
				external := r.Frontend.Kind == "external"
				if external {
					fmt.Fprintf(out, "  frontend: %s (external — launch yourself)\n", r.Status.Profile)
				} else {
					fmt.Fprintf(out, "  frontend: %s (running)\n", r.Status.Profile)
				}
				if r.Frontend.Url != "" && !external {
					fmt.Fprintf(out, "  browser: %s\n", r.Frontend.Url)
				}
				if r.Frontend.WroteFile != "" {
					fmt.Fprintf(out, "  wrote:    %s\n", r.Frontend.WroteFile)
				}
				if len(r.Frontend.EnvVars) > 0 {
					if external {
						fmt.Fprintln(out, "  to use:")
					} else {
						fmt.Fprintln(out, "  env (already applied to the running frontend):")
					}
					keys := make([]string, 0, len(r.Frontend.EnvVars))
					for k := range r.Frontend.EnvVars {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						fmt.Fprintf(out, "    export %s=%q\n", k, r.Frontend.EnvVars[k])
					}
				}
				if external && r.Frontend.RestartRequired {
					fmt.Fprintf(out, "  note: %s does not hot-reload — start (or relaunch) it with the env above\n", r.Status.Profile)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false, noVRAMCheckUsage)
	return cmd
}
