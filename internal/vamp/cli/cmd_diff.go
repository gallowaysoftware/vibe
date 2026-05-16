package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// diffCmd implements `vamp diff <run-a> <run-b>`: a side-by-side
// comparison of two pipeline runs surfacing what changed between them
// (pipeline yaml, inputs, per-stage status / duration / prompt / output,
// and overall timing). The implementation is a thin shim over
// vamp.Compare — every formatting / serialisation decision lives in the
// pure package so this command and the JSON output never drift.
//
// We deliberately accept the two arguments in either of two shapes:
//   - a run-id prefix (resolved against vamp.RunsDir() via FindRunByPrefix),
//   - a path (absolute or relative containing a path separator),
//
// matching the existing surface of `vamp runs show`.
func diffCmd() *cobra.Command {
	var (
		jsonFlag      bool
		noContentFlag bool
		stageFlag     string
	)
	cmd := &cobra.Command{
		Use:   "diff <id-or-path-a> <id-or-path-b>",
		Short: "Compare two pipeline runs and show what changed.",
		Long: "diff loads two run directories and reports the differences between\n" +
			"them: pipeline YAML, declared inputs, per-stage status / duration /\n" +
			"prompt / output, and total wall-clock. Each run argument may be a\n" +
			"basename-prefix (resolved against $XDG_STATE_HOME/vamp/runs/) or an\n" +
			"absolute / relative directory path. Honors NO_COLOR; ANSI colour\n" +
			"otherwise lights up when stdout is a tty.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pathA, err := resolveRunPath(cmd, args[0])
			if err != nil {
				return err
			}
			pathB, err := resolveRunPath(cmd, args[1])
			if err != nil {
				return err
			}
			opts := vamp.CompareOpts{
				NoContent:   noContentFlag,
				StageFilter: stageFlag,
			}
			rep, err := vamp.Compare(pathA, pathB, opts)
			if err != nil {
				return err
			}
			if jsonFlag {
				return rep.JSON(cmd.OutOrStdout())
			}
			return rep.Markdown(cmd.OutOrStdout(), wantColor())
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Emit machine-readable JSON instead of the human-readable view.")
	cmd.Flags().BoolVar(&noContentFlag, "no-content", false, "Skip reading stage output files (metadata only).")
	cmd.Flags().StringVar(&stageFlag, "stage", "", "Restrict the per-stage section to one stage id.")
	return cmd
}

// resolveRunPath turns a CLI argument (id prefix or path) into a
// concrete run-dir path. The lookup mirrors `vamp runs show`: paths
// containing a separator pass through verbatim; everything else is
// prefix-matched against vamp.RunsDir(). Ambiguous prefixes get the
// same "candidates:" message style as `runs show` so the user has one
// recognisable error to fix.
func resolveRunPath(cmd *cobra.Command, arg string) (string, error) {
	r, err := vamp.FindRunByPrefix(vamp.RunsDir(), arg)
	if err == nil {
		return r.Path, nil
	}
	var amb *vamp.AmbiguousError
	if errors.As(err, &amb) {
		fmt.Fprintf(cmd.ErrOrStderr(), "ambiguous prefix %q; candidates:\n", amb.ID)
		for _, c := range amb.Candidates {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", c)
		}
		return "", fmt.Errorf("ambiguous run prefix")
	}
	return "", err
}

// wantColor returns true when ANSI colour should be emitted: stdout is
// a tty AND NO_COLOR is unset. We honor the NO_COLOR convention
// (https://no-color.org) verbatim — any non-empty value disables.
func wantColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isatty.IsTerminal(os.Stdout.Fd())
}
