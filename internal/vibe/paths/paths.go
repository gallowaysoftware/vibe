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

func Socket() string      { return filepath.Join(RuntimeDir(), "vibe.sock") }
func PIDFile() string     { return filepath.Join(StateHome(), "vibe.pid") }
func LogDir() string      { return filepath.Join(StateHome(), "logs") }
func ProfilesDir() string { return filepath.Join(ConfigHome(), "profiles") }
func BackendsDir() string { return filepath.Join(ConfigHome(), "backends") }
func MCPDir() string      { return filepath.Join(ConfigHome(), "mcp") }
func ConfigFile() string  { return filepath.Join(ConfigHome(), "config.yaml") }

// FrontendStateRoot is the parent dir that holds per-profile frontend
// state (opencode config files, Open WebUI bind-mount data, etc.).
// Each profile gets a subdirectory keyed on its name.
func FrontendStateRoot() string { return filepath.Join(StateHome(), "frontend") }

// FrontendStateDir is the per-profile state dir under FrontendStateRoot.
// Vibe ensures this exists at profile-activate time so docker-compose
// bind mounts pointing inside it don't fail when the user has never
// activated the profile before.
func FrontendStateDir(profile string) string {
	return filepath.Join(FrontendStateRoot(), profile)
}

// StartHistoryFile persists the fleet API's per-model start-duration
// history. Under StateHome (not ConfigHome) because it's daemon-generated
// runtime data, same as the PID file and token.
func StartHistoryFile() string { return filepath.Join(StateHome(), "fleet", "start-history.json") }

// HostsFile is the fleet membership registry ($XDG_CONFIG_HOME/vibe/
// hosts.yaml): the cells map every fleet-aware component reads — fleetd's
// multi-cell registry, the CLI's degraded fallback, and the front render's
// peer addresses (C2). Config, not state: humans author it, it changes
// rarely, through git.
func HostsFile() string { return filepath.Join(ConfigHome(), "hosts.yaml") }

// IntentFile persists declared cell intent ("drained for gaming, eta 23:00")
// for the fleetd role. Absence of an entry means serving — the file is
// empty almost always.
func IntentFile() string { return filepath.Join(StateHome(), "fleet", "intent.json") }

// LastSeenFile persists per-cell last-seen timestamps so a fleetd restart
// doesn't forget when an absent cell was last sighted.
func LastSeenFile() string { return filepath.Join(StateHome(), "fleet", "last-seen.json") }

// CellIntentFile persists the cell's own intent state ({state, since})
// for the announce client's conflict rule (fleet-control C3): the
// human-at-the-box timestamp that newer-registry-requests must beat.
func CellIntentFile() string { return filepath.Join(StateHome(), "fleet", "cell-intent.json") }

// CellUsageFile persists the cell-side usage collector's cursor and
// cumulative per-model counters (fleet-control C7a §1). Cumulative, so a
// missed heartbeat loses nothing: the next successful announce carries
// the arrears.
func CellUsageFile() string { return filepath.Join(StateHome(), "fleet", "cell-usage.json") }

// CellProbeFile persists the cell-side throughput prober's rolling
// baselines and budget counters (fleet-control C8 §4). It lives on the
// CELL because the verdict has to be computable there: C3's cardinal
// rule is that an unreachable registry never affects the box, and a cell
// whose fleetd is down must still know its model is degraded and say so
// on the next successful heartbeat.
func CellProbeFile() string { return filepath.Join(StateHome(), "fleet", "model-probe.json") }

// FrontCloudUsageFile persists fleetd's cursor and cumulative counters
// for the FRONT's cloud_peer traffic (fleet-control C7b §6) — actual
// spend, reconstructed from the front's own activity log. Separate from
// CellUsageFile because it is a different collector against a different
// llama-swap with a different filter.
func FrontCloudUsageFile() string {
	return filepath.Join(StateHome(), "fleet", "front-cloud-usage.json")
}

// UsageLedgerFile is fleetd's append-only per-(day, cell, model, basis)
// token ledger (fleet-control C7a §6). Raw counts only — never dollars;
// storing counts is what lets a price table change re-price the whole
// history.
func UsageLedgerFile() string { return filepath.Join(StateHome(), "fleet", "usage.jsonl") }

// LeasesFile persists advisory consumer leases ("batch-ingest holds
// embed-large: mid-batch, 2.1M rows left"). Advisory only — they appear
// in the pre-drain report and fleet_status, never block anything.
func LeasesFile() string { return filepath.Join(StateHome(), "fleet", "leases.json") }

// NotifyScopeFile persists the declared away/home fleet notification
// scope (fleet-control C9 §5). Its own file rather than a key in
// intent.json: every reader of that store treats a key as a cell that
// announces and echoes, and this declares something about the operator,
// not about a cell.
func NotifyScopeFile() string { return filepath.Join(StateHome(), "fleet", "notify-scope.json") }

// TokenFile is the path to the daemon's bearer-token file. The token lives in
// $XDG_STATE_HOME/vibe rather than $XDG_CONFIG_HOME because it's a generated
// runtime secret, not user-authored configuration — same reasoning that puts
// the PID file under StateHome.
func TokenFile() string { return filepath.Join(StateHome(), "token") }

// EnsureDirs creates the directories vibe needs at runtime.
func EnsureDirs() error {
	for _, d := range []string{ConfigHome(), StateHome(), RuntimeDir(), LogDir(), ProfilesDir(), BackendsDir(), MCPDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
