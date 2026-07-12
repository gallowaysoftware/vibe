package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func stopCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop [profile]",
		Short: "Stop the active profile, a named service, or everything (--all).",
		Long: `Stop a running profile.

  vibe stop              # stop the active profile (the default)
  vibe stop <name>       # stop a specific service-mode profile by name
  vibe stop --all        # stop the active profile AND every service

Service-mode profiles run as concurrent sidecars (searxng, embedding
servers, TTS engines). Each must be addressed by name since multiple
can be running at once — there's no single "active service".`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProfileNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			var name string
			if len(args) == 1 {
				name = args[0]
			}
			// `vibe stop all` is the legacy spelling of `vibe stop --all`.
			// It's kept working but nudged toward the flag, which removes the
			// collision with a service-mode profile literally named "all".
			if name == "all" {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: `vibe stop all` is deprecated; use `vibe stop --all`")
			}
			if all {
				if name != "" && name != "all" {
					return fmt.Errorf("--all conflicts with the profile argument %q", name)
				}
				name = "all"
			}
			if _, err := newClient().StopByName(ctx, name); err != nil {
				return err
			}
			switch name {
			case "":
				fmt.Fprintln(cmd.OutOrStdout(), "stopped (active profile)")
			case "all":
				fmt.Fprintln(cmd.OutOrStdout(), "stopped (everything)")
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "stopped %s\n", name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop the active profile and every running service")
	return cmd
}
