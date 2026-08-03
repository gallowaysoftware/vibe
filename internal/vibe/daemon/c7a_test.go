package daemon

// C7a: the declared fleet timezone. It is what the usage ledger buckets
// days in and what the C4 warm schedule evaluates cron fields in;
// fleetd runs containerized with TZ=UTC, so leaving it to the process
// zone splits an evening session across two days and fires an 06:30
// warm at 01:30 local.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir := filepath.Join(tmp, "vibe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFleetLocation_ResolvesTheDeclaredZone(t *testing.T) {
	writeConfig(t, "fleet:\n  cell: gpu\n  timezone: America/Toronto\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Fleet.Timezone != "America/Toronto" {
		t.Fatalf("Fleet.Timezone = %q", cfg.Fleet.Timezone)
	}
	loc := cfg.FleetLocation()
	if loc == nil {
		t.Fatal("FleetLocation() = nil")
	}
	if _, err := time.LoadLocation("America/Toronto"); err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	if loc.String() != "America/Toronto" {
		t.Errorf("FleetLocation() = %q, want America/Toronto", loc)
	}
}

// An unparseable zone must not cost the fleet its registry: warn and
// keep serving in the process zone.
func TestFleetLocation_FallsBackToTheProcessZoneOnGarbage(t *testing.T) {
	writeConfig(t, "fleet:\n  timezone: Mars/Olympus_Mons\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.FleetLocation(); got != time.Local {
		t.Errorf("FleetLocation() = %q, want the process zone", got)
	}
}

func TestFleetLocation_UnsetKeepsThePreC7aBehavior(t *testing.T) {
	writeConfig(t, "fleet:\n  cell: gpu\n")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.FleetLocation(); got != time.Local {
		t.Errorf("FleetLocation() = %q, want the process zone when unset", got)
	}
}
