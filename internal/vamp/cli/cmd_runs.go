package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// runsCmd is the parent of `vamp runs {ls,show,cleanup}`. It is a no-op
// when invoked without a sub-command (cobra prints the help text), which
// matches the pattern used by `vibe profile` and avoids surprising the
// user with `runs` deleting or listing without an explicit verb.
func runsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "Inspect and manage vamp run history (under $XDG_STATE_HOME/vamp/runs/).",
	}
	cmd.AddCommand(
		runsLsCmd(),
		runsShowCmd(),
		runsCleanupCmd(),
	)
	return cmd
}

func runsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List past vamp runs, newest first.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			runs, err := vamp.ListRuns(vamp.RunsDir())
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no runs under %s\n", vamp.RunsDir())
				return nil
			}
			// tabwriter columns: TIMESTAMP, PIPELINE, STAGES, DURATION,
			// SIZE, STATUS, ID. We put the long ID last because it
			// otherwise pushes the other columns out of alignment when
			// the basename varies in length.
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TIMESTAMP\tPIPELINE\tSTAGES\tDURATION\tSIZE\tSTATUS\tID")
			for _, r := range runs {
				name := r.Name
				if name == "" {
					name = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
					vamp.FormatTimestamp(r.Timestamp),
					name,
					r.NumStages,
					vamp.FormatDuration(r.Duration),
					vamp.FormatSize(r.SizeBytes),
					r.Status,
					r.ID,
				)
			}
			return tw.Flush()
		},
	}
}

func runsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id-or-path>",
		Short: "Print pipeline snapshot, inputs, and outputs for a run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := vamp.FindRunByPrefix(vamp.RunsDir(), args[0])
			if err != nil {
				// Friendly rendering of the two structured error types;
				// fall back to the default Error() for anything else.
				var amb *vamp.AmbiguousError
				if errors.As(err, &amb) {
					fmt.Fprintf(cmd.ErrOrStderr(), "ambiguous prefix %q; candidates:\n", amb.ID)
					for _, c := range amb.Candidates {
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", c)
					}
					return fmt.Errorf("ambiguous run prefix")
				}
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "=== run %s ===\n", r.ID)
			fmt.Fprintf(out, "path:      %s\n", r.Path)
			fmt.Fprintf(out, "pipeline:  %s\n", nonEmptyOrDash(r.Name))
			fmt.Fprintf(out, "started:   %s\n", vamp.FormatTimestamp(r.Timestamp))
			fmt.Fprintf(out, "duration:  %s\n", vamp.FormatDuration(r.Duration))
			fmt.Fprintf(out, "stages:    %d\n", r.NumStages)
			fmt.Fprintf(out, "size:      %s\n", vamp.FormatSize(r.SizeBytes))
			fmt.Fprintf(out, "status:    %s\n", r.Status)
			fmt.Fprintln(out)

			dumpFile(out, "pipeline.yaml.snapshot", filepath.Join(r.Path, "pipeline.yaml.snapshot"))
			dumpFile(out, "inputs.json", filepath.Join(r.Path, "inputs.json"))
			dumpFile(out, "pipeline.json", filepath.Join(r.Path, "pipeline.json"))

			// Outputs index: every regular file under the run dir EXCEPT
			// the three metadata files we already printed. We list paths
			// relative to the run dir so the output is portable across
			// runs / machines.
			fmt.Fprintln(out, "--- outputs ---")
			any := false
			_ = filepath.Walk(r.Path, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if info.IsDir() {
					return nil
				}
				rel, rerr := filepath.Rel(r.Path, p)
				if rerr != nil {
					return nil
				}
				switch rel {
				case "pipeline.yaml.snapshot", "inputs.json", "pipeline.json":
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

func runsCleanupCmd() *cobra.Command {
	var (
		olderThanFlag string
		dryRunFlag    bool
	)
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Delete run dirs older than --older-than (default safety: --dry-run shows what would go).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if olderThanFlag == "" {
				return fmt.Errorf("--older-than is required (e.g. --older-than 7d)")
			}
			d, err := parseDurationSpec(olderThanFlag)
			if err != nil {
				return fmt.Errorf("--older-than %q: %w", olderThanFlag, err)
			}
			res, err := vamp.CleanupRuns(vamp.RunsDir(), d, dryRunFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if dryRunFlag {
				fmt.Fprintf(out, "DRY RUN: %d run(s) would be removed, %d kept\n", len(res.Removed), len(res.Skipped))
			} else {
				fmt.Fprintf(out, "removed %d run(s), kept %d\n", len(res.Removed), len(res.Skipped))
			}
			for _, p := range res.Removed {
				fmt.Fprintf(out, "  %s %s\n", actionVerb(dryRunFlag), p)
			}
			if len(res.Errors) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "%d cleanup error(s):\n", len(res.Errors))
				for _, e := range res.Errors {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %v\n", e.Path, e.Err)
				}
				return fmt.Errorf("cleanup completed with errors")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThanFlag, "older-than", "", "Age threshold (e.g. 7d, 24h, 30m). Required.")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "Show what would be removed without deleting anything.")
	return cmd
}

// durationSpecRE matches a positive number followed by a unit suffix.
// Recognized suffixes layer on top of time.ParseDuration by adding d
// (days) and w (weeks); both are computed as fixed-size multiples since
// vamp's run-dir age is wall-clock-meaningful for the cleanup use case
// (a "7-day-old" run is one whose timestamp is 7*24h in the past).
var durationSpecRE = regexp.MustCompile(`^(\d+)([smhdw])$`)

// parseDurationSpec extends time.ParseDuration with "d" (24h) and "w"
// (168h) suffixes. The grammar is intentionally narrow — a single
// "<int><unit>" token — because mixing with time.ParseDuration's float
// + multi-unit grammar invites confusing inputs ("7d30m"). When the
// input is exactly one of the extended units we compute the duration
// directly; everything else is delegated to time.ParseDuration so
// "24h" and "30m" still work without surprises.
func parseDurationSpec(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if m := durationSpecRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		switch m[2] {
		case "s":
			return time.Duration(n) * time.Second, nil
		case "m":
			return time.Duration(n) * time.Minute, nil
		case "h":
			return time.Duration(n) * time.Hour, nil
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil
		case "w":
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

// actionVerb returns "would remove" under --dry-run, "removed" otherwise.
// Centralized so both messages stay grammatically consistent if a future
// formatter prepends an ANSI color code or similar.
func actionVerb(dryRun bool) string {
	if dryRun {
		return "would remove"
	}
	return "removed"
}

// nonEmptyOrDash returns "-" when s is empty so the show command's fixed
// header rows always render a value rather than leaving a column blank.
func nonEmptyOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// dumpFile writes a "--- <label> ---" header followed by the file's
// contents (or an "(missing)" / "(error: …)" placeholder when unreadable).
// We deliberately don't size-limit the body: pipeline.yaml.snapshot and
// inputs.json are both small by construction; pipeline.json is at most
// a few KB even for very long pipelines. If a user wants a paged view
// they can pipe through `less`.
func dumpFile(out interface{ Write(p []byte) (int, error) }, label, path string) {
	fmt.Fprintf(out, "--- %s ---\n", label)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "(missing)")
		} else {
			fmt.Fprintf(out, "(error: %v)\n", err)
		}
		fmt.Fprintln(out)
		return
	}
	_, _ = out.Write(data)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)
}
