// Package vamp is the pipeline orchestrator that drives vibe. It reads a
// pipeline YAML, maps each stage's capability to a vibe profile, calls vibe
// to activate that profile, and dispatches the stage's prompt to vibe's
// OpenAI-compatible inference proxy.
package vamp

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const appName = "vamp"

// homeWarnOnce keeps the fallback notice below to one line per process.
// Every path helper funnels through resolveHome and several are called
// per run; a warning repeated forty times is a warning nobody reads.
var homeWarnOnce sync.Once

// resolveHome returns the user's home directory, and NEVER the empty
// string.
//
// The discarded error was the defect. `home, _ := os.UserHomeDir()`
// leaves home == "" when $HOME is unset or empty, and
// filepath.Join("", ".config", "vamp") is the RELATIVE path
// ".config/vamp" — so every derived path (PipelinesDir,
// CapabilitiesFile, StateHome, RunsDir) silently re-roots onto whatever
// the process CWD happens to be, with nothing logged and nothing
// returning an error. The downstream shape is the class this package
// keeps producing: capabilities.yaml is not found at
// ./.config/vamp/capabilities.yaml, so a capability lookup reads as
// "this capability is not configured" rather than "I cannot find my
// configuration".
//
// The trigger is not exotic. A systemd unit or container entrypoint
// that does not set HOME is exactly the remote-exec shape the fleet's
// SSH+systemd remotes use, and `su`/`env -i` wrappers reach it too.
//
// Fallback rather than a propagated error, deliberately: the four
// exported helpers return a bare string and are called from template
// funcs and CLI wiring that have no error channel, so threading one
// through would be a signature change across every caller for a case
// that must not be silent — which is the actual requirement. TempDir is
// absolute (so the result is never CWD-relative), and the one-shot
// stderr line names both the cause and the two env vars that fix it.
func resolveHome() string {
	home, err := os.UserHomeDir()
	if home != "" && err == nil {
		return home
	}
	return fallbackHome(err)
}

// fallbackHome is the visible relocation: an absolute directory, plus a
// one-shot line on stderr naming the cause and the two env vars that
// answer it. Split out from resolveHome so the guard above is one
// mutable expression rather than a branch tangled with the warning.
func fallbackHome(cause error) string {
	fallback := os.TempDir()
	homeWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"vamp: cannot resolve a home directory (%v); config and state fall back under %s. Set HOME, or XDG_CONFIG_HOME/XDG_STATE_HOME, to place them deliberately.\n",
			cause, fallback)
	})
	return fallback
}

// ConfigHome is $XDG_CONFIG_HOME/vamp (default ~/.config/vamp).
func ConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, appName)
	}
	return filepath.Join(resolveHome(), ".config", appName)
}

// StateHome is $XDG_STATE_HOME/vamp (default ~/.local/state/vamp).
func StateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, appName)
	}
	return filepath.Join(resolveHome(), ".local", "state", appName)
}

func PipelinesDir() string     { return filepath.Join(ConfigHome(), "pipelines") }
func CapabilitiesFile() string { return filepath.Join(ConfigHome(), "capabilities.yaml") }
func RunsDir() string          { return filepath.Join(StateHome(), "runs") }
