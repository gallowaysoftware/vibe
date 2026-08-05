package fleetmcp

// C14 coverage, agent side: suspend_cell shares the schedule's guard,
// force overrides tonight's conditions and never the structural
// refusals, and a cell with no way back is refused before it is taken
// away.

import (
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

func suspendCells(daemonURL string) map[string]fleetcfg.Cell {
	return map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic,
			DaemonURL: daemonURL, Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
		"heavy": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn,
			DaemonURL: daemonURL, Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
		"no-wake": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic, DaemonURL: daemonURL},
		"no-daemon": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassOpportunistic,
			Wake: &fleetcfg.Wake{MAC: "aa:bb:cc:dd:ee:ff"}},
	}
}

// TestSuspendCell_ForceNeverBypassesAStructuralRefusal is the phase's
// safety line at the agent surface: an operator override is about
// tonight's conditions, and suspending the front or an always_on cell is
// not a condition, it is a configuration mistake.
func TestSuspendCell_ForceNeverBypassesAStructuralRefusal(t *testing.T) {
	fake := newFakeCellDaemon(t)
	s, _ := newTestFacade(t, suspendCells(fake.srv.URL), nil)
	for _, cell := range []string{fleetcfg.FrontCell, "heavy", "typo"} {
		for _, force := range []bool{false, true} {
			if _, err := s.toolSuspendCell(t.Context(), cell, "goodnight", force); err == nil {
				t.Fatalf("%s (force=%v) was suspended", cell, force)
			}
		}
	}
	if fake.suspendCalls != 0 {
		t.Fatalf("suspend calls = %d, want none", fake.suspendCalls)
	}
}

// TestSuspendCell_RefusesTonightsConditionsWithTheReason proves the
// shared guard is actually consulted (an absent cell here) and that
// force is what overrides it.
func TestSuspendCell_RefusesTonightsConditionsWithTheReason(t *testing.T) {
	fake := newFakeCellDaemon(t)
	s, _ := newTestFacade(t, suspendCells(fake.srv.URL), nil)

	_, err := s.toolSuspendCell(t.Context(), "gpu-cell", "goodnight", false)
	if err == nil || !strings.Contains(err.Error(), "already absent") {
		t.Fatalf("err = %v, want the guard's own reason", err)
	}
	if fake.suspendCalls != 0 {
		t.Fatalf("suspend calls = %d before the override", fake.suspendCalls)
	}

	out, err := s.toolSuspendCell(t.Context(), "gpu-cell", "goodnight", true)
	if err != nil {
		t.Fatal(err)
	}
	if fake.suspendCalls != 1 {
		t.Fatalf("suspend calls = %d, want 1", fake.suspendCalls)
	}
	if fake.lastRequire {
		t.Fatal("force still demanded the cell's idle proof")
	}
	if !strings.Contains(out, "wake_cell") || !strings.Contains(out, "NOT required") {
		t.Fatalf("reply = %q, want the way back and the skipped proof", out)
	}
	for _, c := range s.fleet.Snapshot(t.Context()).Cells {
		if c.Name != "gpu-cell" {
			continue
		}
		if c.Intent == nil || c.Intent.Reason != "goodnight" {
			t.Fatalf("intent on gpu-cell = %+v, want the reason recorded (a box off the fleet with no stated why is the DRAINED? residue)", c.Intent)
		}
	}
}

// TestSuspendCell_RefusesACellWithNoWayBack: nothing else in this fleet
// can bring a suspended box back, so the absence of a wake path is
// checked BEFORE the box goes away, not after.
func TestSuspendCell_RefusesACellWithNoWayBack(t *testing.T) {
	fake := newFakeCellDaemon(t)
	s, _ := newTestFacade(t, suspendCells(fake.srv.URL), nil)
	_, err := s.toolSuspendCell(t.Context(), "no-wake", "goodnight", false)
	if err == nil || !strings.Contains(err.Error(), "no wake") {
		t.Fatalf("err = %v, want a refusal naming the missing wake path", err)
	}
	if fake.suspendCalls != 0 {
		t.Fatalf("suspend calls = %d", fake.suspendCalls)
	}
}

// TestSuspendCell_HasNoAnnounceFallback pins the delivery decision: a
// piggybacked suspend is redelivered after the cell's next reboot, which
// is a box that puts itself back to sleep the morning after.
func TestSuspendCell_HasNoAnnounceFallback(t *testing.T) {
	fake := newFakeCellDaemon(t)
	s, _ := newTestFacade(t, suspendCells(fake.srv.URL), nil)
	_, err := s.toolSuspendCell(t.Context(), "no-daemon", "goodnight", true)
	if err == nil || !strings.Contains(err.Error(), "no announce fallback") {
		t.Fatalf("err = %v, want the missing daemon_url to be an error, not a queued command", err)
	}
}
