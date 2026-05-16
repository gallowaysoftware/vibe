// Package cli implements the `vamp` command-line interface.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
)

func Execute() error { return rootCmd().Execute() }

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vamp",
		Short:         "Multi-stage AI pipeline orchestrator (drives vibe).",
		Long:          "vamp runs a YAML pipeline against vibe, swapping profiles per stage based on a capability → profile mapping.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		runCmd(),
		validateCmd(),
		listCmd(),
		capabilitiesCmd(),
		runsCmd(),
		jobsCmd(),
		logsCmd(),
		cancelCmd(),
		confirmCmd(),
		vizCmd(),
		schemaCmd(),
		diffCmd(),
		cacheCmd(),
	)
	return root
}
