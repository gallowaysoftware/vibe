package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

func logsCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs [profile]",
		Short: "Show recent logs from a running profile's supervisor.",
		Long: `Tails the most recent backend log lines from a supervised
process. With no argument, returns the active profile's logs
(legacy behavior). With a profile name, returns that service-mode
profile's logs — handy for diagnosing a docker-managed sidecar
that wedged itself mid-pipeline.

Examples:

  vibe logs                # active profile
  vibe logs searxng        # the searxng service
  vibe logs embed_bge_large
  vibe logs -f searxng     # follow live until the service stops
`,
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
			client := newClient()
			window, err := client.LogsForProfile(ctx, name)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			start := 0
			if lines > 0 && len(window) > lines {
				start = len(window) - lines
			}
			for _, l := range window[start:] {
				fmt.Fprintln(out, l)
			}
			if !follow {
				return nil
			}
			// Signal-aware context so ctrl-C ends the follow cleanly
			// instead of surfacing as an RPC error.
			ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			err = followLogs(ctx, out, client, name, window)
			// A signal-cancelled follow isn't an error — the user asked
			// us to stop. Keep the exit code clean, mirroring vamp logs.
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow the supervisor log as new lines arrive (exits when the profile stops).")
	cmd.Flags().IntVarP(&lines, "lines", "n", 0, "Limit the initial dump to the last N buffered lines (0 = all).")
	return cmd
}

// followLogs polls the daemon's log window and prints only lines not
// already printed. The supervisor stores logs in a fixed-capacity ring
// buffer with no cursor, so once full, successive windows have identical
// length with shifted content — dedupe must be by content overlap, not
// line count. Since the ring only appends, the previous window's suffix
// reappears as the new window's prefix; everything after the longest such
// overlap is fresh. No overlap means the ring rolled past everything we
// printed, so the whole window is fresh.
func followLogs(ctx context.Context, out io.Writer, client *vibeclient.Client, name string, prev []string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		// Sample liveness before fetching so the fetch below still drains
		// lines written between the previous tick and process exit.
		exited := profileExited(ctx, client, name)
		window, err := client.LogsForProfile(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A named service that stops mid-follow vanishes from the
			// daemon's service table; that's the end of the stream, not
			// a failure.
			if vibeclient.IsNotFound(err) {
				return nil
			}
			return err
		}
		for _, l := range window[logOverlap(prev, window):] {
			fmt.Fprintln(out, l)
		}
		prev = window
		if exited {
			return nil
		}
	}
}

// logOverlap returns the length of the longest suffix of prev equal to a
// prefix of next. Longest-match order matters: a repeated log line could
// otherwise satisfy a shorter overlap and re-print lines already shown.
func logOverlap(prev, next []string) int {
	for l := min(len(prev), len(next)); l > 0; l-- {
		if slices.Equal(prev[len(prev)-l:], next[:l]) {
			return l
		}
	}
	return 0
}

// profileExited reports whether the followed profile is no longer running,
// so -f terminates without ctrl-C once the supervisor is done. Empty (or
// "active") targets the active profile; any other name is a service, which
// simply disappears from the status list when it stops. Status errors count
// as "still running" — better to keep polling than truncate a live tail on
// a transient RPC hiccup.
func profileExited(ctx context.Context, client *vibeclient.Client, name string) bool {
	active, services, err := client.StatusFull(ctx)
	if err != nil {
		return false
	}
	if name == "" || name == "active" {
		return active == nil || !active.GetRunning()
	}
	for _, s := range services {
		if s.GetProfile() == name {
			return false
		}
	}
	return true
}
