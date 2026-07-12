package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// confirmCmd writes the `accepted` / `rejected` response file a
// `type: confirm` stage is polling. The blocked executor sees the file
// on its next 500ms tick and either returns ok (accept) or returns
// errStageRejected (reject) — which the run aggregator treats the same
// way as any other stage error.
//
// We deliberately *don't* require the pending marker file to exist before
// writing the response: an operator who knows the exact stage id may want
// to pre-stage a decision (e.g. for scripted runs) before the worker
// reaches that stage. The executor only reads the response file once it
// has written its own marker, so a pre-stage write either fires
// immediately on first poll (good) or sits idle until the stage arrives
// (also good).
func confirmCmd() *cobra.Command {
	var reject bool
	cmd := &cobra.Command{
		Use:   "confirm <id-or-prefix> <stage-id>",
		Short: "Accept (default) or reject (--reject) a `type: confirm` stage in a run.",
		Long: "Clears a `type: confirm` stage gate by writing its response file. " +
			"Use this from outside the run (typically when the run was started with --detach) " +
			"to approve or reject a stage that's blocking on operator input.\n\n" +
			"The run is named by id-or-prefix (the id `vamp run --detach` printed, " +
			"resolved against $XDG_STATE_HOME/vamp/runs/) or a directory path — the " +
			"same addressing as `vamp runs show` / `vamp logs`.",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeConfirmArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stageID := args[1]
			r, err := vamp.FindRunByPrefix(vamp.RunsDir(), args[0])
			if err != nil {
				return renderLookupErr(cmd, err)
			}
			path := vamp.ResponseFilePath(r.Path, stageID)
			body := "accepted"
			if reject {
				body = "rejected"
			}
			// O_EXCL would catch a double-confirm (two operators
			// racing), but the executor is the only consumer and
			// happily idempotent on either keyword — making the second
			// confirm idempotently overwrite is the friendlier ergonomic
			// for an operator who wants to flip from accept to reject
			// before the stage's next poll. WriteFile is plenty.
			if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
				return fmt.Errorf("write response file %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stage %s %s (run %s)\n", stageID, body, r.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&reject, "reject", false, "Reject the stage instead of accepting.")
	return cmd
}
