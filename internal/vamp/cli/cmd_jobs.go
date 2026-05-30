package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// jobsCmd is a hidden deprecated alias for `vamp runs`. The two used to be
// separate nouns — `runs` for history, `jobs` for detached-run control —
// which meant a detached run showed up in both with different columns. They
// are now one noun; `jobs {ls,show,cancel}` delegates to the identical
// `runs` verbs so old scripts and muscle memory keep working.
func jobsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:        "jobs",
		Short:      "Deprecated alias for `vamp runs`.",
		Deprecated: "use `vamp runs` instead.",
		Hidden:     true,
	}
	cmd.AddCommand(
		runsLsCmd(),
		runsShowCmd(),
		runsCancelCmd(),
	)
	return cmd
}

// cancelCmd is the top-level `vamp cancel <id>` alias for
// `vamp runs cancel <id>`. Cancellation is common enough that paying for
// the verb-noun pattern every time is friction; the explicit
// `runs cancel` form stays available for users who prefer to keep the
// namespace tidy.
func cancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "cancel <id-or-prefix>",
		Short:             "Send SIGTERM to a running detached run (alias of `vamp runs cancel`).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs,
		RunE:              runCancel,
	}
}

// runCancel implements `vamp cancel` / `vamp runs cancel`. The flow is:
//  1. Resolve the prefix to exactly one job (FindJobByPrefix already
//     handles zero/many-match error rendering).
//  2. If the job isn't in JobStateRunning, return a friendly message
//     naming the actual state so the user doesn't think we silently
//     no-op'd.
//  3. Otherwise send SIGTERM. The worker's signal.NotifyContext picks
//     that up and cancels the executor's ctx; writePipelineJSON renders
//     that as status="canceled" in pipeline.json on its way out.
func runCancel(cmd *cobra.Command, args []string) error {
	j, err := vamp.FindJobByPrefix(vamp.RunsDir(), args[0])
	if err != nil {
		return renderLookupErr(cmd, err)
	}
	if j.State != vamp.JobStateRunning {
		return fmt.Errorf("run %s is %s, not running", j.ID, j.State)
	}
	if err := vamp.CancelJob(j); err != nil {
		if errors.Is(err, vamp.ErrJobNotRunning) {
			return fmt.Errorf("run %s is no longer running", j.ID)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "sent SIGTERM to run %s (pid %d)\n", j.ID, j.PID)
	return nil
}
