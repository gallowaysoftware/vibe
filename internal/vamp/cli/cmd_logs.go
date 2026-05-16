package cli

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// logsCmd implements `vamp logs <id> [-f]`. The non-follow form is a
// straight `cat` of vamp.log; the follow form polls the file for new
// bytes (200ms cadence) and watches the pid file so we exit cleanly once
// the worker is gone instead of spinning forever on a dead job.
//
// We don't reuse `runs show` for this: the artifact we care about is
// vamp.log specifically, not the pipeline.yaml.snapshot / inputs.json
// metadata bundle.
func logsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <id-or-prefix>",
		Short: "Print the vamp.log for a job (use -f to follow live).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			j, err := vamp.FindJobByPrefix(vamp.RunsDir(), args[0])
			if err != nil {
				return renderJobLookupErr(cmd, err)
			}
			// Set up ctx with signal handling so ctrl-C while
			// following exits cleanly (and we don't leak the file
			// descriptor). The plain `cat` path also benefits — a
			// huge log file shouldn't hang on a sluggish terminal
			// without the user being able to abort.
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			pid := j.PID
			stopWhen := func() bool {
				// Once the recorded pid is gone the worker has
				// exited; one more drain and we're done. This is
				// what lets `vamp logs <id> -f` finish without the
				// user having to ctrl-C even after the job ends.
				if pid <= 0 {
					return true
				}
				return !vamp.IsAlive(pid)
			}
			err = vamp.TailLogs(ctx, cmd.OutOrStdout(), j.LogPath, follow, 200*time.Millisecond, stopWhen)
			// A signal-cancelled tail isn't really an error — the
			// user asked us to stop. Don't muddy the CLI's exit
			// code by propagating it.
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow the log file as new content is appended (exits when the job's pid is gone).")
	return cmd
}
