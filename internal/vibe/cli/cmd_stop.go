package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [profile]",
		Short: "Stop the active profile, a named service, or everything.",
		Long: `Stop a running profile.

  vibe stop              # stop the active profile (the legacy default)
  vibe stop <name>       # stop a specific service-mode profile by name
  vibe stop all          # stop the active profile AND every service

Service-mode profiles run as concurrent sidecars (searxng, embedding
servers, TTS engines). Each must be addressed by name since multiple
can be running at once — there's no single "active service".`,
		Args: cobra.MaximumNArgs(1),
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
			if _, err := newClient().StopByName(ctx, name); err != nil {
				return err
			}
			switch {
			case name == "":
				fmt.Println("stopped (active profile)")
			case name == "all":
				fmt.Println("stopped (everything)")
			default:
				fmt.Printf("stopped %s\n", name)
			}
			return nil
		},
	}
}
