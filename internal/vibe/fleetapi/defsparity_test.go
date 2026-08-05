package fleetapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// versionFleetWithHost is versionFleet plus fleetd's own build facts, so
// the parity check can be exercised from both ends of the comparison it
// makes: the cells against each other, and fleetd against the cells.
func versionFleetWithHost(t *testing.T, host DoctorHost, versions map[string]*AnnounceVersions) *Server {
	t.Helper()
	dir := t.TempDir()
	cells := []Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}}
	names := make([]string, 0, len(versions))
	for n := range versions {
		names = append(names, n)
	}
	for _, n := range names {
		cells = append(cells, Cell{Name: n, URL: "http://127.0.0.1:1", Class: "always_on"})
	}
	s := New(cells, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{
		IntentPath:   filepath.Join(dir, "intent.json"),
		LastSeenPath: filepath.Join(dir, "last-seen.json"),
		DoctorHost:   func() DoctorHost { return host },
	})
	t.Cleanup(s.Close)
	for _, n := range names {
		s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: n, Seq: 1,
			Intent:   &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
			Versions: versions[n]})
	}
	return s
}

// TestDoctor_DefsParityDirtinessNeverDowngradesAKnownDivergence is the
// 2026-08-05 live-gate finding, reproduced from the fleet that produced
// it: alpha and charlie at one defs SHA, bravo at another, and doctor
// correctly reporting WARN. Making bravo's checkout DIRTY dropped it from
// the comparison entirely, leaving one SHA standing and flipping the same
// real divergence to OK — a diagnostic going quiet exactly as the
// situation gets worse. Dirty-and-diverged is strictly more alarming than
// clean-and-diverged, so dirtiness may only ever ADD concern.
func TestDoctor_DefsParityDirtinessNeverDowngradesAKnownDivergence(t *testing.T) {
	fleet := func(bravoDirty bool) *Server {
		return versionFleetWithHost(t, DoctorHost{Version: "v1", DefsSHA: "aaa111"}, map[string]*AnnounceVersions{
			"alpha":   {DefsSHA: "aaa111", Vibe: "v1"},
			"bravo":   {DefsSHA: "bbb222", Vibe: "v1", DefsDirty: bravoDirty},
			"charlie": {DefsSHA: "aaa111", Vibe: "v1"},
		})
	}

	cleanRep := mustCheck(t, fleet(false).Doctor(context.Background()), "defs.parity", "")
	if cleanRep.Level != LevelWarn {
		t.Fatalf("clean divergence → %s (%s), want warn — the fixture is wrong, not the fix", cleanRep.Level, cleanRep.Detail)
	}

	got := mustCheck(t, fleet(true).Doctor(context.Background()), "defs.parity", "")
	if got.Level != LevelWarn {
		t.Fatalf("bravo diverged AND dirty → %s (%s), want warn: dirtiness may only add concern", got.Level, got.Detail)
	}
	// The divergent SHA must still be NAMED. A WARN that has forgotten
	// which cell is on the other tree costs the operator the diagnosis.
	for _, want := range []string{"aaa111", "bbb222", "bravo"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got.Detail, want)
		}
	}
	if !strings.Contains(got.Detail, "dirty") {
		t.Errorf("detail = %q, want bravo's dirty tree still named — it is extra concern, not a reason to go quiet", got.Detail)
	}
}

// TestDoctor_DefsParityFleetdDirtinessNeverDowngradesEither pins the same
// trap one level down: fleetd on a different commit from the cells is a
// WARN because the render it writes comes from another tree, and the
// first cut suppressed that WARN outright when fleetd's own checkout was
// dirty — the strictly worse case.
func TestDoctor_DefsParityFleetdDirtinessNeverDowngradesEither(t *testing.T) {
	cells := map[string]*AnnounceVersions{
		"alpha": {DefsSHA: "aaa111", Vibe: "v1"},
		"bravo": {DefsSHA: "aaa111", Vibe: "v1"},
	}
	clean := mustCheck(t, versionFleetWithHost(t, DoctorHost{Version: "v1", DefsSHA: "zzz999"}, cells).Doctor(context.Background()), "defs.parity", "")
	if clean.Level != LevelWarn {
		t.Fatalf("fleetd on another commit → %s (%s), want warn", clean.Level, clean.Detail)
	}

	got := mustCheck(t, versionFleetWithHost(t, DoctorHost{Version: "v1", DefsSHA: "zzz999", DefsDirty: true}, cells).Doctor(context.Background()), "defs.parity", "")
	if got.Level != LevelWarn {
		t.Fatalf("fleetd on another commit AND dirty → %s (%s), want warn", got.Level, got.Detail)
	}
	if !strings.Contains(got.Detail, "zzz999") || !strings.Contains(got.Detail, "dirty") {
		t.Errorf("detail = %q, want fleetd's SHA and its dirty tree both named", got.Detail)
	}
}

// TestDoctor_DefsParityCannotCompareSaysSoDistinctly pins C13's own rule
// on this check: UNKNOWN is not OK, and the two ways parity has nothing
// to compare read as different sentences. "Nobody reports a SHA" and
// "everybody reports the same SHA and not one of them can vouch for it"
// are different problems.
func TestDoctor_DefsParityCannotCompareSaysSoDistinctly(t *testing.T) {
	t.Run("nothing reported", func(t *testing.T) {
		got := mustCheck(t, versionFleetWithHost(t, DoctorHost{}, map[string]*AnnounceVersions{
			"alpha": nil, "bravo": nil,
		}).Doctor(context.Background()), "defs.parity", "")
		if got.Level != LevelUnknown {
			t.Fatalf("level = %s, want unknown", got.Level)
		}
		if strings.Contains(got.Detail, "dirty") {
			t.Errorf("detail = %q blames dirtiness where there is none", got.Detail)
		}
	})
	t.Run("every reporter dirty on one SHA", func(t *testing.T) {
		got := mustCheck(t, versionFleetWithHost(t, DoctorHost{}, map[string]*AnnounceVersions{
			"alpha": {DefsSHA: "aaa111", DefsDirty: true},
			"bravo": {DefsSHA: "aaa111", DefsDirty: true},
		}).Doctor(context.Background()), "defs.parity", "")
		if got.Level != LevelUnknown {
			t.Fatalf("level = %s, want unknown — matching base commits prove nothing when no tree is clean", got.Level)
		}
		if !strings.Contains(got.Detail, "dirty") {
			t.Errorf("detail = %q, want the reason named", got.Detail)
		}
	})
}

// TestDoctor_DefsParityDirtyAgreementStaysOKAndSaysWhichCell: a fleet
// where the comparable cells agree and one cell is mid-edit is a working
// fleet, so the level stays OK (C13: a permanent WARN on a healthy fleet
// teaches the operator to ignore the level) — but the cell whose tree is
// not any committed version is named, because that is what the operator
// asks next.
func TestDoctor_DefsParityDirtyAgreementStaysOKAndSaysWhichCell(t *testing.T) {
	got := mustCheck(t, versionFleetWithHost(t, DoctorHost{Version: "v1", DefsSHA: "aaa111"}, map[string]*AnnounceVersions{
		"alpha": {DefsSHA: "aaa111", Vibe: "v1"},
		"bravo": {DefsSHA: "aaa111", Vibe: "v1", DefsDirty: true},
	}).Doctor(context.Background()), "defs.parity", "")
	if got.Level != LevelOK {
		t.Fatalf("one dirty cell on the agreed SHA → %s (%s), want ok", got.Level, got.Detail)
	}
	if !strings.Contains(got.Detail, "bravo") || !strings.Contains(got.Detail, "dirty") {
		t.Errorf("detail = %q, want bravo's dirty tree named", got.Detail)
	}
}
