package cli

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

func envCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Print export lines for the active profile's frontend env vars.",
		Long:  "Suitable for `eval \"$(vibe env)\"` in your shell. Prints nothing if no profile is active or the profile defined no env vars.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Read-only: never spawn a daemon just to print env vars. With
			// no daemon there's no active profile, so there's nothing to
			// print — exactly the documented "prints nothing" behavior.
			if err := pingDaemon(500 * time.Millisecond); err != nil {
				return nil
			}
			s, err := newClient().Status(ctx)
			if err != nil {
				return err
			}
			if !s.Running || len(s.FrontendEnv) == 0 {
				return nil
			}
			keys := make([]string, 0, len(s.FrontendEnv))
			for k := range s.FrontendEnv {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out := cmd.OutOrStdout()
			for _, k := range keys {
				fmt.Fprintf(out, "export %s=%q\n", k, s.FrontendEnv[k])
			}
			return nil
		},
	}
}
