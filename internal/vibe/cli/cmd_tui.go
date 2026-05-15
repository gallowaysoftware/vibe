package cli

import (
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/tui"
)

// tuiCmd is the operator's real-time dashboard. Unlike most other subcommands
// it deliberately does NOT auto-spawn the daemon — the TUI is for observing
// what's already running, so side effects at boot would surprise users (and
// would be especially annoying over SSH where stderr can be far away).
func tuiCmd() *cobra.Command {
	var apiURL string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the real-time vibe dashboard.",
		Long: "tui opens a Bubbletea-based dashboard that polls the vibe daemon " +
			"for status and logs once a second, and lets you start/stop profiles " +
			"interactively. Works over SSH; honors NO_COLOR.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(apiURL)
		},
	}
	cmd.Flags().StringVar(&apiURL, "api", "", "vibe daemon control-plane URL (default: unix socket, then $VIBE_API, then "+defaultAPIHint+")")
	return cmd
}

// defaultAPIHint is just the static default surfaced in --help; we don't
// import vibeclient.DefaultBaseURL here to keep the help string concise.
const defaultAPIHint = "http://127.0.0.1:9001"
