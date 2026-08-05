package cli

// C14 coverage, CLI side: `vibe cell suspend` asks the cell to prove it
// is idle by default and only skips that proof when the operator says
// so — the verb that takes a box off the fleet does not get a quiet
// default — and it holds the guard's STRUCTURAL refusals, which --force
// never bypasses.

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
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s", wake: { mac: "aa:bb:cc:dd:ee:ff" } }
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
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s", wake: { mac: "aa:bb:cc:dd:ee:ff" } }
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
  gpu-cell: { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%s", wake: { mac: "aa:bb:cc:dd:ee:ff" } }
`, cellSrv.URL))

	var out bytes.Buffer
	err := suspendCell(t.Context(), &out, "gpu-cell", "", false)
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("err = %v, want the cell's own reason carried to the operator", err)
	}
}

// TestCellSuspendHoldsTheStructuralRefusals (review REV2-2). The CLI
// dials the cell daemon directly, so it never reaches
// fleetapi.SuspendGuard — and before this fix it applied NONE of the
// refusals the phase doc calls structural. `vibe cell suspend front`
// took the data plane and the control plane down with it; `vibe cell
// suspend laptop` put a roaming box to sleep that no packet on this LAN
// can wake; and a cell with no `wake:` block went down with no way back.
// --force is about tonight's conditions and bypasses none of them: a
// force that suspends the front is not an override, it is a bug.
func TestCellSuspendHoldsTheStructuralRefusals(t *testing.T) {
	fake := &cellVerbFake{}
	cellSrv := serveCellVerbsTCP(t, fake)
	writeHosts(t, fmt.Sprintf(`
fleetd_url: "http://127.0.0.1:1"
cells:
  front:    { url: "http://127.0.0.1:1", class: always_on, daemon_url: "%[1]s" }
  laptop:   { url: "http://127.0.0.1:1", class: roaming, daemon_url: "%[1]s", wake: { mac: "aa:bb:cc:dd:ee:ff" } }
  no-wake:  { url: "http://127.0.0.1:1", class: opportunistic, daemon_url: "%[1]s" }
`, cellSrv.URL))

	cases := []struct {
		cell, want string
		force      bool
	}{
		{cell: "front", want: "total fleet outage"},
		{cell: "front", want: "total fleet outage", force: true},
		{cell: "laptop", want: "class roaming"},
		{cell: "laptop", want: "class roaming", force: true},
		{cell: "no-wake", want: "nothing could bring it back"},
	}
	for _, tc := range cases {
		name := tc.cell
		if tc.force {
			name += "/force"
		}
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := suspendCell(t.Context(), &out, tc.cell, "", tc.force)
			if err == nil {
				t.Fatalf("suspended %s with no refusal", tc.cell)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
	if fake.suspends != 0 {
		t.Fatalf("%d suspend RPCs reached a cell: every one of these is a refusal that happens BEFORE the wire", fake.suspends)
	}

	// The one that is not structural: --force is exactly what a walk to
	// the basement looks like, so it does override the missing wake path.
	var out bytes.Buffer
	if err := suspendCell(t.Context(), &out, "no-wake", "", true); err != nil {
		t.Fatalf("--force refused a cell with no wake path: %v", err)
	}
	if fake.suspends != 1 {
		t.Fatalf("suspends = %d, want the forced one through", fake.suspends)
	}
}
