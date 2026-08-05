package cli

// C14 coverage, CLI side: `vibe cell suspend` asks the cell to prove it
// is idle by default and only skips that proof when the operator says
// so — the verb that takes a box off the fleet does not get a quiet
// default.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestCellSuspendRemoteAsksForTheIdleProofByDefault(t *testing.T) {
	sink := newCLIIntentSink(t)
	fake := &cellVerbFake{}
	cellSrv := serveCellVerbsTCP(t, fake)
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "%s"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s" }
`, sink.srv.URL, cellSrv.URL))

	var out bytes.Buffer
	if err := suspendCell(t.Context(), &out, "gpu-cell", "done for the night", false); err != nil {
		t.Fatal(err)
	}
	if fake.suspends != 1 {
		t.Fatalf("suspends = %d, want 1", fake.suspends)
	}
	if !fake.lastRequire {
		t.Fatal("require_idle = false by default: the box would go down mid-generation")
	}
	if fake.lastReason != "done for the night" {
		t.Fatalf("reason = %q", fake.lastReason)
	}
	got := sink.calls()
	if len(got) != 1 || got[0]["state"] != "drained" || got[0]["reason"] != "done for the night" {
		t.Fatalf("intent posts = %v, want the suspend recorded as declared intent", got)
	}
	if !strings.Contains(out.String(), "wake it with") {
		t.Fatalf("output = %q, want it to name the way back", out.String())
	}
}

func TestCellSuspendForceSkipsTheProof(t *testing.T) {
	fake := &cellVerbFake{}
	cellSrv := serveCellVerbsTCP(t, fake)
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "http://127.0.0.1:1"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s" }
`, cellSrv.URL))

	var out bytes.Buffer
	if err := suspendCell(t.Context(), &out, "gpu-cell", "", true); err != nil {
		t.Fatal(err)
	}
	if fake.lastRequire {
		t.Fatal("--force still asked the cell to prove it was idle")
	}
	if !strings.Contains(out.String(), "idle proof was NOT required") {
		t.Fatalf("output = %q, want the skipped proof stated", out.String())
	}
}

func TestCellSuspendReportsTheCellsRefusal(t *testing.T) {
	fake := &cellVerbFake{failSuspend: true}
	cellSrv := serveCellVerbsTCP(t, fake)
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "http://127.0.0.1:1"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on }
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s" }
`, cellSrv.URL))

	var out bytes.Buffer
	err := suspendCell(t.Context(), &out, "gpu-cell", "", false)
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("err = %v, want the cell's own reason carried to the operator", err)
	}
}
