package vamp

import (
	"path/filepath"
	"testing"
)

// TestPaths_AreAbsoluteWithNoResolvableHome.
//
// paths.go discarded os.UserHomeDir()'s error, so with $HOME unset the
// helpers returned RELATIVE paths: RunsDir() == ".local/state/vamp/runs",
// IsAbs == false. Nothing logged and nothing errored — vamp simply
// relocated its whole config and state tree onto whatever the process
// CWD happened to be, and the first symptom is a capability lookup that
// reads "this capability is not configured" instead of "I cannot find my
// configuration".
//
// A HOME-less unit is not hypothetical here: it is the shape of an
// SSH+systemd remote, which is exactly how the fleet execs vamp.
//
// StateHome, PipelinesDir and RunsDir were all at 0.0% coverage; this is
// their first test.
func TestPaths_AreAbsoluteWithNoResolvableHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	for name, got := range map[string]string{
		"ConfigHome":       ConfigHome(),
		"StateHome":        StateHome(),
		"PipelinesDir":     PipelinesDir(),
		"CapabilitiesFile": CapabilitiesFile(),
		"RunsDir":          RunsDir(),
	} {
		if got == "" {
			t.Errorf("%s() = \"\"", name)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s() = %q, which is RELATIVE — the state tree follows the CWD", name, got)
		}
	}
}

// TestPaths_XDGStillWinsWithNoHome keeps the fallback narrow. An operator
// who set XDG_CONFIG_HOME / XDG_STATE_HOME has already answered the
// question, and the home-dir fallback must not overrule them.
func TestPaths_XDGStillWinsWithNoHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	if got, want := ConfigHome(), filepath.Join(dir, "cfg", appName); got != want {
		t.Errorf("ConfigHome() = %q, want %q", got, want)
	}
	if got, want := RunsDir(), filepath.Join(dir, "state", appName, "runs"); got != want {
		t.Errorf("RunsDir() = %q, want %q", got, want)
	}
}

// TestPaths_HomeIsHonouredWhenItResolves is the other side of the same
// guard: the fallback must be unreachable in the normal case, or it would
// silently move everyone's config to TempDir.
func TestPaths_HomeIsHonouredWhenItResolves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	if got, want := ConfigHome(), filepath.Join(home, ".config", appName); got != want {
		t.Errorf("ConfigHome() = %q, want %q", got, want)
	}
	if got, want := StateHome(), filepath.Join(home, ".local", "state", appName); got != want {
		t.Errorf("StateHome() = %q, want %q", got, want)
	}
}
