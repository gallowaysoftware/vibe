// Package cli implements the `vibe` command-line interface. It speaks to the
// vibe daemon over a unix socket and auto-spawns the daemon on first use.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
)

// noVRAMCheckUsage is the shared help string for the --no-vram-check flag,
// which `start` and `run` both expose. Defined once so the two can't drift.
const noVRAMCheckUsage = "skip the daemon's pre-flight VRAM check against the profile's estimated_vram_gb"

func Execute() error {
	return rootCmd().Execute()
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "vibe",
		Short:         "Task-oriented launcher for local AI inference.",
		Long:          "vibe brings up a model server (llama.cpp, tabbyAPI, ComfyUI, or any HTTP inference server) + frontend stack from a single YAML profile.",
		Version:       buildinfo.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
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
		backendCmd(),
		routerCmd(),
		tokenCmd(),
		cellCmd(),
	)
	return root
}
