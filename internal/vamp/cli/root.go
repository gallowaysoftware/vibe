// Package cli implements the `vamp` command-line interface.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
)

// inputFlagUsage is the shared help string for the repeatable `--input`
// flag, which run / render (and their in-memory variants) all expose.
// Defined once so the wording can't drift across the four definitions.
const inputFlagUsage = "pipeline input as KEY=VALUE; can repeat (commas inside the value are NOT split)"

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
		renderCmd(),
		validateCmd(),
		lintCmd(),
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
