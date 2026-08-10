package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func shutdownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Stop the active profile (if any) and shut down the daemon.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := pingDaemon(pingBudget); err != nil {
				// Already-down is the desired end state for a teardown
				// command; report it and exit 0 so `vibe shutdown` is
				// idempotent (no `|| true` needed in scripts). That is the
				// right answer to an ABSENT daemon and only to an absent
				// one.
				//
				// It used to be the answer to every ping failure, on a
				// 200ms budget — the shortest in the CLI, on the one
				// command whose entire job is to talk to a daemon that is
				// by construction busy: it is holding a model and it is
				// about to be asked to tear the whole stack down. So
				// `vibe shutdown && <next step>` printed "daemon not
				// running", exited 0, and ran the next step against a
				// daemon that was still up with the GPU still occupied.
				// The budget is now the same 500ms `ps` uses (see
				// pingBudget), and a spent budget is reported as what it
				// is. See daemonAbsent in client.go.
				if !daemonAbsent(err) {
					return fmt.Errorf("cannot tell whether the daemon is running: it did not answer within %s (%w). "+
						"Nothing was shut down — this is not evidence that it is already stopped; re-run to retry", pingBudget, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "daemon not running")
				return nil
			}
			if err := newClient().Shutdown(ctx); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "daemon shutting down")
			return nil
		},
	}
}
