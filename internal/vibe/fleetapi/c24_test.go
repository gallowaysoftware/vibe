package fleetapi

// C24 coverage: the stop record — what a cell unit's own ExecStopPost
// hook writes, and the four things fleetd must refuse to do with it.
//
// The property under test throughout is one sentence: a stop record
// carries the WHEN and the WHAT, never the WHY. Every test here either
// proves the record cannot become a command, or proves that a surface
// which answers "why is this box down?" is exactly as loud as it was
// before the record existed.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// TestC24StopRecordIsNeverHandedBackAsACommand is the headline. A
// registry drained entry is handed to an announcing cell as
// desired_intent, and fleetannounce.reconcile answers it by RUNNING
// cell_cmds.drain. So the naive hook — POST drained, done — stops the
// serving stack of a box that has just come back, on the first
// heartbeat, through a path nothing in the hook can see.
func TestC24StopRecordIsNeverHandedBackAsACommand(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)

	// Control first: an ordinary declared drain IS handed back. Without
	// this line the test below passes just as well against a fleetd that
	// hands nothing back to anyone.
	if _, err := s.SetIntent("laptop", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	_, resp := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"serving","since":"2020-01-01T00:00:00Z"}}`)
	if resp.DesiredIntent == nil || resp.DesiredIntent.State != "drained" {
		t.Fatalf("a declared drain must still be handed back: desired = %+v", resp.DesiredIntent)
	}

	// The same shape, written by the unit's own stop hook. The clear
	// first is not incidental: a stop record does not overwrite a
	// declaration (see TestC24StopRecordNeverOverwritesADeclaration), so
	// the operator's drain has to be over before the unit's note applies.
	if _, err := s.SetIntent("laptop", "serving", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIntent("laptop", "drained", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	_, resp = postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":2,"intent":{"state":"serving","since":"2020-01-01T00:00:00Z"}}`)
	if resp.DesiredIntent != nil {
		t.Errorf("a stop record was handed back as %+v — the cell will run cell_cmds.drain on a box that just returned; a hook that records must not actuate", resp.DesiredIntent)
	}
	// And it is still in the store: not handing it back is not the same
	// as forgetting it.
	if in, ok := s.effectiveIntent("laptop"); !ok || in.Reason != StopIntentReason {
		t.Errorf("effective intent = %+v ok=%v, want the stop record kept", in, ok)
	}
}

// TestC24StopRecordLosesToTheCellsOwnDrain: a drained ECHO is only ever
// produced by a declared drain at the box (the daemon writes it after
// its own drain verb succeeds). The unit stop that drain performs fires
// the hook too, so without this rule every `vibe cell drain --reason
// gaming` on an announcing cell ends up displaying "stopped out of
// band" — the phase would make the axis less trustworthy, not more.
func TestC24StopRecordLosesToTheCellsOwnDrain(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)
	if _, err := s.SetIntent("laptop", "drained", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"drained","since":"2020-01-01T00:00:00Z"}}`)

	s.mu.Lock()
	_, still := s.intents["laptop"]
	s.mu.Unlock()
	if still {
		t.Error("the stop record survived the cell's own drained echo — the box you are standing at outranks a record written by the stop")
	}
	// The echo alone renders the drain, exactly as it did before C24.
	in, ok := s.effectiveIntent("laptop")
	if !ok || in.State != "drained" || in.Reason != "" {
		t.Errorf("effective intent = %+v ok=%v, want a bare drained echo with no invented reason", in, ok)
	}

	// The rule is scoped to stop records: a HUMAN's declared drain plus a
	// drained echo is the C6 complied-drain branch, which keeps the
	// reason and the ETA.
	if _, err := s.SetIntent("laptop", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":2,"intent":{"state":"drained","since":"`+
		time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)+`"}}`)
	if in, ok := s.effectiveIntent("laptop"); !ok || in.Reason != "gaming" || in.ETA != "23:00" {
		t.Errorf("a declared drain must survive its own ack: %+v ok=%v", in, ok)
	}
}

// TestC24StopRecordIsNotAPendingRequest. pending means "requested of the
// cell, not yet acked". Nothing was requested, and the box whose stack
// is down cannot ack anything — left as pending it renders "requested,
// awaiting cell ack" in `vibe cell status` and turns `vibe fleet doctor`
// yellow every night the box is off (C14's permanent-WARN shape).
func TestC24StopRecordIsNotAPendingRequest(t *testing.T) {
	stop := Intent{State: "drained", Reason: StopIntentReason, Since: time.Now().UTC()}
	if _, _, pending := resolveIntent(stop, true, nil); pending {
		t.Error("stop record reported pending")
	}
	// Control: the same entry with a human's reason IS pending — the
	// cell has not echoed it yet.
	human := Intent{State: "drained", Reason: "gaming", Since: time.Now().UTC()}
	if _, _, pending := resolveIntent(human, true, nil); !pending {
		t.Error("a declared drain with no echo must still read as pending, or this test proves nothing")
	}
}

// TestC24UnitStartRetiresOnlyItsOwnRecord covers the ExecStartPost half.
// It must clear the stop record (or the record is stale for as long as
// the box is up, which is the "confident wrong answer" this phase
// exists to prevent), and it must do nothing else at all.
func TestC24UnitStartRetiresOnlyItsOwnRecord(t *testing.T) {
	s, _, _ := newFleetdServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"},
	})

	// Nothing recorded: a no-op, not an error and not a write.
	if got, err := s.SetIntent("gpu-cell", "serving", StopIntentReason, ""); err != nil || got != nil {
		t.Errorf("clear with no record = %+v, %v; want a silent no-op", got, err)
	}

	// Its own record: retired.
	if _, err := s.SetIntent("gpu-cell", "drained", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIntent("gpu-cell", "serving", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.effectiveIntent("gpu-cell"); ok {
		t.Error("the unit's start did not retire its own stop record")
	}

	// A human's declaration: untouched, and reported back so the hook can
	// say so in the journal. The operator is still gaming; the unit
	// merely started.
	if _, err := s.SetIntent("gpu-cell", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetIntent("gpu-cell", "serving", StopIntentReason, "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Reason != "gaming" {
		t.Errorf("clear returned %+v, want the untouched declaration handed back", got)
	}
	if in, ok := s.effectiveIntent("gpu-cell"); !ok || in.Reason != "gaming" {
		t.Errorf("a unit start cleared a human's declared drain: %+v ok=%v", in, ok)
	}
}

// TestC24UnitStartNeverBecomesAResumeRequest is the other half of "it
// does not actuate", from the start side: on an ANNOUNCING cell the
// ordinary serving path STORES a serving request, which is handed back
// as desired_intent, where reconcile runs cell_cmds.resume.
func TestC24UnitStartNeverBecomesAResumeRequest(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)
	postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"serving","since":"2020-01-01T00:00:00Z"}}`)
	if _, err := s.SetIntent("laptop", "drained", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIntent("laptop", "serving", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	entry, has := s.intents["laptop"]
	s.mu.Unlock()
	if has {
		t.Fatalf("a unit start left %+v in the store for an announcing cell — a serving entry is a REQUEST, and a request runs cell_cmds.resume", entry)
	}
	_, resp := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":2,"intent":{"state":"serving","since":"2020-01-01T00:00:00Z"}}`)
	if resp.DesiredIntent != nil {
		t.Errorf("desired = %+v after a unit start; the hook must hand the cell no command at all", resp.DesiredIntent)
	}

	// Control: the ordinary resume path (a human, no reserved reason) DOES
	// store the request for an announcing cell — C3's ride-along.
	if _, err := s.SetIntent("laptop", "serving", "", ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	_, has = s.intents["laptop"]
	s.mu.Unlock()
	if !has {
		t.Error("a human's resume must still ride desired_intent to an announcing cell, or this test proves nothing")
	}
}

// TestC24StopRecordDoesNotSilenceTheAlwaysOnAlarm. `systemctl stop` and
// a crash fire the same ExecStopPost. If the record counted as an
// explanation, C9 would stop paging for the one thing the always_on
// alarm exists for — and the notifier would be muted by the box's own
// dying words.
func TestC24StopRecordDoesNotSilenceTheAlwaysOnAlarm(t *testing.T) {
	stop := &Intent{State: "drained", Reason: StopIntentReason, Since: time.Now().UTC()}
	cases := []struct {
		name    string
		snap    CellSnapshot
		alarm   bool
		wantSub string
	}{
		{"stop record, host up", CellSnapshot{Name: "heavy", Class: string(fleetcfg.ClassAlwaysOn),
			Display: DisplayDrained, Intent: stop}, true, "nothing recorded why"},
		{"stop record, box gone", CellSnapshot{Name: "heavy", Class: string(fleetcfg.ClassAlwaysOn),
			Display: DisplayOff, Intent: stop}, true, "nothing recorded why"},
		{"declared drain", CellSnapshot{Name: "heavy", Class: string(fleetcfg.ClassAlwaysOn),
			Display: DisplayDrained, Intent: &Intent{State: "drained", Reason: "maintenance"}}, false, ""},
		{"no entry at all", CellSnapshot{Name: "heavy", Class: string(fleetcfg.ClassAlwaysOn),
			Display: DisplayDrainedQ}, true, "no declared intent"},
		{"opportunistic box, stop record", CellSnapshot{Name: "gpu-cell", Class: string(fleetcfg.ClassOpportunistic),
			Display: DisplayDrained, Intent: stop}, false, ""},
		{"serving", CellSnapshot{Name: "heavy", Class: string(fleetcfg.ClassAlwaysOn),
			Display: DisplayServing}, false, ""},
	}
	for _, tc := range cases {
		detail, got := absentAlarm(tc.snap)
		if got != tc.alarm {
			t.Errorf("%s: alarm = %v, want %v (detail %q)", tc.name, got, tc.alarm, detail)
		}
		if tc.wantSub != "" && !strings.Contains(detail, tc.wantSub) {
			t.Errorf("%s: detail = %q, want it to contain %q", tc.name, detail, tc.wantSub)
		}
	}
}

// TestC24DoctorStillCallsTheStopUndeclared: the record answers "when",
// so the display moves off DRAINED? — but nothing declared WHY, and the
// sit-down command must not get quieter because the box managed to say
// "I stopped" on the way down.
func TestC24DoctorStillCallsTheStopUndeclared(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"})
	if _, err := s.SetIntent("gpu-cell", "drained", StopIntentReason, ""); err != nil {
		t.Fatal(err)
	}
	// Older than the ack window: the age at which a request would be
	// called residue.
	s.mu.Lock()
	in := s.intents["gpu-cell"]
	in.Since = time.Now().UTC().Add(-2 * staleRequestAge)
	s.intents["gpu-cell"] = in
	s.mu.Unlock()

	got := mustCheck(t, s.Doctor(context.Background()), "intent.hygiene", "")
	if got.Level != LevelWarn {
		t.Fatalf("intent.hygiene = %s (%s), want warn: nothing declared why this box is down", got.Level, got.Detail)
	}
	if !strings.Contains(got.Detail, "undeclared state") || !strings.Contains(got.Detail, "gpu-cell") {
		t.Errorf("detail = %q, want the cell named as an undeclared stop", got.Detail)
	}
	if strings.Contains(got.Detail, "never echoed") {
		t.Errorf("detail = %q: a stop record is not a request and has no ack to wait for", got.Detail)
	}
	if !strings.Contains(got.Fix, "--until-exit") {
		t.Errorf("fix = %q, want it to point at the declared path", got.Fix)
	}
}

// TestC24VersionedVerbsAreTheWireContract: the hook posts a STATE the
// endpoint either understands or refuses. fleetd stamps the reserved
// reason; the hook never sends one, so it cannot dress a stop up as a
// declaration.
func TestC24VersionedVerbsAreTheWireContract(t *testing.T) {
	s, ts, _ := newFleetdServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"},
	})
	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/api/fleet/intent", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if code := post(`{"cell":"gpu-cell","state":"unit_stopped","reason":"nice try","eta":"23:00"}`); code != http.StatusOK {
		t.Fatalf("unit_stopped: HTTP %d", code)
	}
	in, ok := s.effectiveIntent("gpu-cell")
	if !ok || !IsStopRecord(&in) {
		t.Fatalf("intent = %+v ok=%v, want the reserved reason stamped by fleetd", in, ok)
	}
	if in.ETA != "" {
		t.Errorf("eta = %q: the unit knows that it stopped and when, and nothing else — a wire ETA must not survive", in.ETA)
	}

	if code := post(`{"cell":"gpu-cell","state":"unit_started"}`); code != http.StatusOK {
		t.Fatalf("unit_started: HTTP %d", code)
	}
	if _, ok := s.effectiveIntent("gpu-cell"); ok {
		t.Error("unit_started did not retire the stop record")
	}

	// The old vocabulary is untouched, and an unknown state is still a
	// 400 — which is what makes an OLD fleetd safe against a NEW hook.
	if code := post(`{"cell":"gpu-cell","state":"drained","reason":"gaming"}`); code != http.StatusOK {
		t.Errorf("drained: HTTP %d", code)
	}
	if code := post(`{"cell":"gpu-cell","state":"unit_exploded"}`); code != http.StatusBadRequest {
		t.Errorf("unknown state: HTTP %d, want 400", code)
	}
}

// TestC24StopRecordNeverOverwritesADeclaration. The forcing case is
// C14's: a scheduled suspend records {drained, "asleep per
// sleep_schedule", eta 07:15} and THEN takes the box down through the
// same unit stop that fires this hook. A stop record that overwrote it
// would make the fleet forget it had put the box to sleep — and the
// wake's clear-first ordering would have a different record to clear
// than the one it wrote.
func TestC24StopRecordNeverOverwritesADeclaration(t *testing.T) {
	s, _, _ := newFleetdServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"},
	})
	for _, declared := range []Intent{
		{State: "drained", Reason: SleepIntentReason, ETA: "07:15"},
		{State: "drained", Reason: "gaming", ETA: "23:00"},
	} {
		if _, err := s.SetIntent("gpu-cell", "drained", declared.Reason, declared.ETA); err != nil {
			t.Fatal(err)
		}
		got, err := s.SetIntent("gpu-cell", "drained", StopIntentReason, "")
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Reason != declared.Reason || got.ETA != declared.ETA {
			t.Errorf("the stop record overwrote %q: now %+v", declared.Reason, got)
		}
		in, ok := s.effectiveIntent("gpu-cell")
		if !ok || in.Reason != declared.Reason || in.ETA != declared.ETA {
			t.Errorf("stored intent = %+v ok=%v, want %q kept", in, ok, declared.Reason)
		}
	}

	// It may still replace its OWN note: that is the unit updating the
	// one entry it owns, and the timestamp is the point of the record.
	if _, err := s.SetIntent("gpu-cell", "serving", "", ""); err != nil {
		t.Fatal(err)
	}
	first, err := s.SetIntent("gpu-cell", "drained", StopIntentReason, "")
	if err != nil || first == nil {
		t.Fatalf("first stop: %+v %v", first, err)
	}
	later, err := s.SetIntentAt("gpu-cell", "drained", StopIntentReason, "", time.Now().UTC().Add(time.Hour))
	if err != nil || later == nil || !later.Since.After(first.Since) {
		t.Errorf("a later stop must refresh the unit's own record: %+v -> %+v (%v)", first, later, err)
	}
}
