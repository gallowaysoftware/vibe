// Package cli implements the `vibe` command-line interface. It speaks to the
// vibe daemon over a unix socket and auto-spawns the daemon on first use.
package cli

import "github.com/spf13/cobra"

func Execute() error {
	return rootCmd().Execute()
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vibe",
		Short:         "Task-oriented launcher for local AI inference.",
		Long:          "vibe brings up a llama-server + frontend stack from a single YAML profile.",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		startCmd(),
		stopCmd(),
		psCmd(),
		listCmd(),
		logsCmd(),
		daemonCmd(),
		shutdownCmd(),
		envCmd(),
		pullCmd(),
	)
	return root
}
