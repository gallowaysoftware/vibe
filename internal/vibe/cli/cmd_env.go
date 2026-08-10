package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

func envCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Print export lines for the active profile's frontend env vars.",
		Long: "Suitable for `eval \"$(vibe env)\"` in your shell. Prints nothing if no profile is active or the profile defined no env vars.\n" +
			"Exits non-zero, printing nothing, if the daemon does not answer in time — that is not the same as no profile being active, and stdout stays empty because your shell executes it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// Read-only: never spawn a daemon just to print env vars. With
			// no daemon there's no active profile, so there's nothing to
			// print — exactly the documented "prints nothing" behavior.
			//
			// That argument covers ABSENCE and nothing else, and this
			// command used to apply it to every ping failure. What that
			// costs is specific: `eval "$(vibe env)"` against a daemon that
			// is merely slow to answer exports NOTHING, so the frontend
			// falls back to its built-in vendor endpoint and bills the
			// operator for tokens the local front was sitting there ready
			// to serve — with no diagnostic anywhere, because silence is
			// also what success looks like when no profile is active. See
			// daemonAbsent in client.go.
			//
			// The two channels are chosen by what `eval` does with them.
			// STDOUT is EXECUTED by the user's shell, so a diagnostic
			// printed there would be a command; it goes to stderr, where
			// eval leaves it alone and the operator still sees it. The exit
			// status is the honest machine-readable half: `vibe env` now
			// exits non-zero when it does not know, which is what
			// distinguishes "no profile" from "no answer" for a script
			// that checks.
			if err := pingDaemon(pingBudget); err != nil {
				if !daemonAbsent(err) {
					return fmt.Errorf("cannot read the active profile's environment: the daemon did not answer within %s (%w). "+
						"Nothing was exported — this is not evidence that no profile is active; re-run to retry", pingBudget, err)
				}
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
