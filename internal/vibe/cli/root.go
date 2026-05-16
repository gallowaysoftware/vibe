// Package cli implements the `vibe` command-line interface. It speaks to the
// vibe daemon over a unix socket and auto-spawns the daemon on first use.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
)

func Execute() error {
	return rootCmd().Execute()
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vibe",
		Short:         "Task-oriented launcher for local AI inference.",
		Long:          "vibe brings up a llama-server + frontend stack from a single YAML profile.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	// Print just the version string (no "vibe version " prefix) so the
	// install script can parse `vibe --version` cheaply.
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		startCmd(),
		runCmd(),
		stopCmd(),
		psCmd(),
		listCmd(),
		logsCmd(),
		daemonCmd(),
		shutdownCmd(),
		envCmd(),
		pullCmd(),
		doctorCmd(),
		tuiCmd(),
		profileCmd(),
		tokenCmd(),
	)
	return root
}
