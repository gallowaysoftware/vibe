package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// jobsCmd is the parent of `vamp jobs {ls,show,cancel}`. Like `runs`, it
// is a help-only no-op when invoked without a sub-command; sub-commands
// are the verbs the user actually intends.
//
// `cancel` is also surfaced as a top-level `vamp cancel <id>` in root.go;
// having it under `jobs` is the discoverable form (it shows up in
// `vamp jobs --help`) and the top-level alias is the ergonomic shortcut
// the user asked for. Both routes call the same RunE.
func jobsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect and control vamp jobs spawned by `vamp run --detach`.",
	}
	cmd.AddCommand(
		jobsLsCmd(),
		jobsShowCmd(),
		jobsCancelCmd(),
	)
	return cmd
}

func jobsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List running + recently-finished vamp jobs, newest first.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobs, err := vamp.ListJobs(vamp.RunsDir())
			if err != nil {
				return err
			}
			if len(jobs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no jobs under %s\n", vamp.RunsDir())
				return nil
			}
			// tabwriter columns: STATE, TIMESTAMP, PIPELINE, STAGES,
			// DURATION, STATUS, PID, ID. STATE leads so a running job
			// jumps out visually; PID follows STATUS so a glance shows
			// "running, pid 12345". ID is last because the basename
			// varies and would otherwise push columns out of alignment.
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "STATE\tTIMESTAMP\tPIPELINE\tSTAGES\tDURATION\tSTATUS\tPID\tID")
			for _, j := range jobs {
				name := j.Name
				if name == "" {
					name = "-"
				}
				pidStr := "-"
				if j.PID > 0 {
					pidStr = fmt.Sprintf("%d", j.PID)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
					j.State,
					vamp.FormatTimestamp(j.Timestamp),
					name,
					j.NumStages,
					vamp.FormatDuration(j.Duration),
					nonEmptyOrDash(j.Status),
					pidStr,
					j.ID,
				)
			}
			return tw.Flush()
		},
	}
}

func jobsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id-or-prefix>",
		Short: "Print state, timing, and outputs index for one job.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			j, err := vamp.FindJobByPrefix(vamp.RunsDir(), args[0])
			if err != nil {
				return renderJobLookupErr(cmd, err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "=== job %s ===\n", j.ID)
			fmt.Fprintf(out, "state:     %s\n", j.State)
			fmt.Fprintf(out, "path:      %s\n", j.Path)
			fmt.Fprintf(out, "pipeline:  %s\n", nonEmptyOrDash(j.Name))
			fmt.Fprintf(out, "started:   %s\n", vamp.FormatTimestamp(j.Timestamp))
			fmt.Fprintf(out, "duration:  %s\n", vamp.FormatDuration(j.Duration))
			fmt.Fprintf(out, "stages:    %d\n", j.NumStages)
			fmt.Fprintf(out, "size:      %s\n", vamp.FormatSize(j.SizeBytes))
			fmt.Fprintf(out, "status:    %s\n", nonEmptyOrDash(j.Status))
			pidStr := "-"
			if j.PID > 0 {
				pidStr = fmt.Sprintf("%d", j.PID)
			}
			fmt.Fprintf(out, "pid:       %s\n", pidStr)
			fmt.Fprintf(out, "log:       %s\n", j.LogPath)
			fmt.Fprintln(out)

			dumpFile(out, "pipeline.json", filepath.Join(j.Path, "pipeline.json"))

			// Outputs index: reuse the same layout as `runs show` so
			// users who switch between the two commands aren't surprised.
			fmt.Fprintln(out, "--- outputs ---")
			any := false
			_ = filepath.Walk(j.Path, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if info.IsDir() {
					return nil
				}
				rel, rerr := filepath.Rel(j.Path, p)
				if rerr != nil {
					return nil
				}
				switch rel {
				case "pipeline.yaml.snapshot", "inputs.json", "pipeline.json", vamp.LogFileName, vamp.PidFileName, "pipeline_timing.json":
					return nil
				}
				fmt.Fprintf(out, "%s\t%s\n", vamp.FormatSize(info.Size()), rel)
				any = true
				return nil
			})
			if !any {
				fmt.Fprintln(out, "(no output files)")
			}
			return nil
		},
	}
}

func jobsCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id-or-prefix>",
		Short: "Send SIGTERM to a running vamp job (under jobs).",
		Args:  cobra.ExactArgs(1),
		RunE:  runCancel,
	}
}

// cancelCmd is the top-level `vamp cancel <id>` alias for
// `vamp jobs cancel <id>`. Cancellation is common enough that paying for
// the verb-noun pattern every time is friction; the explicit `jobs cancel`
// form stays available for users who prefer to keep the namespace tidy.
func cancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id-or-prefix>",
		Short: "Send SIGTERM to a running vamp job (alias of `vamp jobs cancel`).",
		Args:  cobra.ExactArgs(1),
		RunE:  runCancel,
	}
}

// runCancel implements `vamp cancel` / `vamp jobs cancel`. The flow is:
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
		return renderJobLookupErr(cmd, err)
	}
	if j.State != vamp.JobStateRunning {
		return fmt.Errorf("job %s is %s, not running", j.ID, j.State)
	}
	if err := vamp.CancelJob(j); err != nil {
		if errors.Is(err, vamp.ErrJobNotRunning) {
			return fmt.Errorf("job %s is no longer running", j.ID)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "sent SIGTERM to job %s (pid %d)\n", j.ID, j.PID)
	return nil
}

// renderJobLookupErr unwraps AmbiguousError into a friendly multi-line
// message and returns NotFoundError verbatim. Mirrors the rendering
// `runs show` does for the run-prefix lookup so the two listings feel
// consistent.
func renderJobLookupErr(cmd *cobra.Command, err error) error {
	var amb *vamp.AmbiguousError
	if errors.As(err, &amb) {
		fmt.Fprintf(cmd.ErrOrStderr(), "ambiguous prefix %q; candidates:\n", amb.ID)
		for _, c := range amb.Candidates {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", c)
		}
		return fmt.Errorf("ambiguous job prefix")
	}
	return err
}
