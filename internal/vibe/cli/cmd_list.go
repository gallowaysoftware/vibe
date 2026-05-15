package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available profiles.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			profs, err := newClient().ListProfiles(ctx)
			if err != nil {
				return err
			}
			if len(profs) == 0 {
				fmt.Println("no profiles (drop YAML files in ~/.config/vibe/profiles/)")
				return nil
			}
			for _, p := range profs {
				if p.Description != "" {
					fmt.Printf("%-20s %s\n", p.Name, p.Description)
				} else {
					fmt.Println(p.Name)
				}
			}
			return nil
		},
	}
}
