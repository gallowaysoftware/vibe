package cli

import (
	"context"
	"fmt"

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
			if err := pullProfile(ctx, args[0]); err != nil {
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
			fmt.Printf("started %s\n", r.Status.Profile)
			fmt.Printf("  backend: %s\n", r.Status.BackendAddr)
			fmt.Printf("  proxy:   %s\n", r.Status.ProxyAddr)
			if r.Frontend != nil {
				// external = config rendered, user launches the binary;
				// managed / docker-compose = vibe also launched the binary.
				// The label + trailing note differ accordingly.
				external := r.Frontend.Kind == "external"
				if external {
					fmt.Printf("  frontend: %s (external — launch yourself)\n", r.Frontend.App)
				} else {
					fmt.Printf("  frontend: %s (running)\n", r.Frontend.App)
				}
				if r.Frontend.Url != "" && !external {
					fmt.Printf("  browser: %s\n", r.Frontend.Url)
				}
				if r.Frontend.WroteFile != "" {
					fmt.Printf("  wrote:    %s\n", r.Frontend.WroteFile)
				}
				if len(r.Frontend.EnvVars) > 0 {
					if external {
						fmt.Println("  to use:")
					} else {
						fmt.Println("  env (already applied to the running frontend):")
					}
					for k, v := range r.Frontend.EnvVars {
						fmt.Printf("    export %s=%q\n", k, v)
					}
				}
				if external && r.Frontend.RestartRequired {
					fmt.Printf("  note: %s does not hot-reload — start (or relaunch) it with the env above\n", r.Frontend.App)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false,
		"Skip the daemon's pre-flight VRAM check against the profile's estimated_vram_gb.")
	return cmd
}
