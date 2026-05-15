// Package vamp is the pipeline orchestrator that drives vibe. It reads a
// pipeline YAML, maps each stage's capability to a vibe profile, calls vibe
// to activate that profile, and dispatches the stage's prompt to vibe's
// OpenAI-compatible inference proxy.
package vamp

import (
	"os"
	"path/filepath"
)

const appName = "vamp"

// ConfigHome is $XDG_CONFIG_HOME/vamp (default ~/.config/vamp).
func ConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, appName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", appName)
}

// StateHome is $XDG_STATE_HOME/vamp (default ~/.local/state/vamp).
func StateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, appName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", appName)
}

func PipelinesDir() string     { return filepath.Join(ConfigHome(), "pipelines") }
func CapabilitiesFile() string { return filepath.Join(ConfigHome(), "capabilities.yaml") }
func RunsDir() string          { return filepath.Join(StateHome(), "runs") }
