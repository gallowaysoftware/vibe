// Package paths resolves XDG-style locations for vibe's runtime artifacts.
package paths

import (
	"os"
	"path/filepath"
)

const appName = "vibe"

// ConfigHome is $XDG_CONFIG_HOME/vibe (default ~/.config/vibe).
func ConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, appName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", appName)
}

// StateHome is $XDG_STATE_HOME/vibe (default ~/.local/state/vibe).
func StateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, appName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", appName)
}

// RuntimeDir is $XDG_RUNTIME_DIR/vibe; falls back to StateHome when unset.
func RuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, appName)
	}
	return StateHome()
}

func Socket() string       { return filepath.Join(RuntimeDir(), "vibe.sock") }
func PIDFile() string      { return filepath.Join(StateHome(), "vibe.pid") }
func LogDir() string       { return filepath.Join(StateHome(), "logs") }
func ProfilesDir() string  { return filepath.Join(ConfigHome(), "profiles") }
func ConfigFile() string   { return filepath.Join(ConfigHome(), "config.yaml") }

// EnsureDirs creates the directories vibe needs at runtime.
func EnsureDirs() error {
	for _, d := range []string{ConfigHome(), StateHome(), RuntimeDir(), LogDir(), ProfilesDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
