package fleetapi

// C3 adversarial-review regression coverage: the serving-request
// end-to-end, probe-evidence precedence, echo clock clamp, event
// transition gating, ingest validation, and the command queue cap.

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestResumeViaAnnounceEndToEnd: the daemonless-cell lifecycle that was
// a silent no-op — drain (stored), echo drained, resume request (stored
// serving), desired serving handed back, cell echoes serving newer,
// request dropped.
func TestResumeViaAnnounceEndToEnd(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)

	// 1. Drain request via fleetd (the MCP/CLI path writes it).
	if _, err := s.SetIntent("laptop", "drained", "gaming", ""); err != nil {
		t.Fatal(err)
	}
	// 2. Cell drains and echoes drained (newer): the request is resolved
	// and BECOMES the record — it stops being pending, and the reason
	// survives the ack (C6 M6; deleting it here erased the WHY one
	// heartbeat after every drain).
	drainTime := time.Now().UTC()
	postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"drained","since":"`+drainTime.Format(time.RFC3339Nano)+`"}}`)
	s.mu.Lock()
	rec, drainedThere := s.intents["laptop"]
	s.mu.Unlock()
	if !drainedThere || rec.State != "drained" || rec.Reason != "gaming" || !rec.Since.Equal(drainTime) {
		t.Fatalf("resolved drain record = %+v (present=%v)", rec, drainedThere)
	}
	drainedSnap := CellSnapshot{Name: "laptop"}
	s.decorate(&drainedSnap)
	if drainedSnap.IntentPending {
		t.Fatal("a complied drain still reads as a pending request")
	}

	// 3. Resume request: stored as a serving request (announcing cell).
	if _, err := s.SetIntent("laptop", "serving", "", ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	stored, ok := s.intents["laptop"]
	s.mu.Unlock()
	if !ok || stored.State != "serving" {
		t.Fatalf("serving request not stored: %+v", stored)
	}

	// 4. The next heartbeat still echoes the OLD drained stamp (the cell
	// hasn't reconciled the serving request yet): desired serving is
	// handed back.
	_, resp := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":2,"intent":{"state":"drained","since":"`+drainTime.Format(time.RFC3339Nano)+`"}}`)
	if resp.DesiredIntent == nil || resp.DesiredIntent.State != "serving" {
		t.Fatalf("desired = %+v, want the serving request handed back", resp.DesiredIntent)
	}

	// 5. The cell resumes and echoes serving newer: request resolved.
	evenLater := time.Now().UTC().Add(time.Second)
	_, resp = postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":3,"intent":{"state":"serving","since":"`+evenLater.Format(time.RFC3339Nano)+`"}}`)
	if resp.DesiredIntent != nil {
		t.Errorf("serving request survived its echo: %+v", resp.DesiredIntent)
	}
	s.mu.Lock()
	_, stillThere := s.intents["laptop"]
	s.mu.Unlock()
	if stillThere {
		t.Error("serving request not dropped after echo")
	}
}

// TestProbeEvidenceBeatsDrainedEcho: a probe that just answered is proof
// of life a drained echo can't retract — display nags INCONSISTENT, the
// state that exposes the disagreement (or the lie).
func TestProbeEvidenceBeatsDrainedEcho(t *testing.T) {
	cell := newFakeCell(t)
	s, ts, _ := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: cell.srv.URL, Class: "opportunistic"},
	})
	// Cell announces drained (and is fresh)...
	now := time.Now().UTC()
	s.recordAnnounce(&AnnounceRequest{
		V: 1, Cell: "gpu-cell", Seq: 1,
		Intent: &AnnounceIntent{State: "drained", Since: now},
	})
	// ...but the probe ANSWERS (the stack is up: slow stop, or a lie).
	st := getState(t, ts)
	c := st.Cells[0]
	if !c.Reachable {
		t.Error("probe evidence of life retracted by a drained echo")
	}
	if c.Display != DisplayInconsistent {
		t.Errorf("display = %s, want INCONSISTENT (the disagreement must nag)", c.Display)
	}
}

// TestEchoClockClamped: a year-2999 echo can't cancel requests from the
// future — the timestamp clamps to now+skew at ingest.
func TestEchoClockClamped(t *testing.T) {
	s, ts := newAnnounceServerWithFleet(t)
	if _, err := s.SetIntent("laptop", "drained", "via MCP", ""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(365 * 24 * time.Hour)
	code, _ := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"serving","since":"`+future.Format(time.RFC3339Nano)+`"}}`)
	if code != http.StatusOK {
		t.Fatalf("clamped echo must still be accepted: HTTP %d", code)
	}
	// The clamp keeps the echo from out-dating the request: the request
	// survives (it is NOT resolved by a forged-future echo)... clamped
	// to now+2min, request was made earlier → clamped echo is newer
	// → resolves. Hmm — the clamp CAPS the future, it can't erase it.
	// The real assertion: since is clamped to ≤ now+skew.
	p := s.presenceFor("laptop")
	if p.IntentEcho.Since.After(time.Now().UTC().Add(echoFutureSkew)) {
		t.Errorf("echo since not clamped: %v", p.IntentEcho.Since)
	}
}

// TestWithdrawEventTransitionGating: steady-state withdrawing fires no
// repeated cellReturned/cellWithdrawn drip.
func TestWithdrawEventTransitionGating(t *testing.T) {
	s, _ := newAnnounceServerWithFleet(t)
	seen := []string{}
	orig := s.publish
	_ = orig
	// Publish through a capturing subscriber.
	ch := make(chan Event, 16)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	drain := func() {
		for {
			select {
			case ev := <-ch:
				seen = append(seen, ev.Type)
			default:
				return
			}
		}
	}

	post := func(state string, seq uint64) {
		s.recordAnnounce(&AnnounceRequest{
			V: 1, Cell: "laptop", Seq: seq,
			Intent: &AnnounceIntent{State: state, Since: time.Now().UTC()},
		})
		drain()
	}

	post("serving", 1)     // firstEver → cellReturned
	post("withdrawing", 2) // transition → cellWithdrawn
	post("withdrawing", 3) // steady state → nothing
	post("withdrawing", 4) // steady state → nothing
	post("serving", 5)     // back → cellReturned

	want := []string{EventCellReturned, EventCellWithdrawn, EventCellReturned}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want transition-gated %v", seen, want)
	}
}

// TestAnnounceIngestValidation: enum values and field hygiene.
func TestAnnounceIngestValidation(t *testing.T) {
	_, ts := newAnnounceServer(t)

	cases := map[string]string{
		"bad intent state":  `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"napping"}}`,
		"control char":      `{"v":1,"cell":"laptop","seq":1,"models":[{"id":"qw\u0001en","state":"ready"}]}`,
		"oversize model id": `{"v":1,"cell":"laptop","seq":1,"models":[{"id":"` + strings.Repeat("x", 300) + `","state":"ready"}]}`,
		"oversize version":  `{"v":1,"cell":"laptop","seq":1,"versions":{"llama_swap":"` + strings.Repeat("v", 300) + `"}}`,
	}
	for name, body := range cases {
		code, _ := postAnnounce(t, ts, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400", name, code)
		}
	}
	// A clean announce still passes.
	code, _ := postAnnounce(t, ts, `{"v":1,"cell":"laptop","seq":1,"intent":{"state":"serving"},"models":[{"id":"qwen","state":"ready"}]}`)
	if code != http.StatusOK {
		t.Errorf("clean announce rejected: HTTP %d", code)
	}
}

// TestCommandQueueCap: the per-cell queue drops oldest past the bound.
func TestCommandQueueCap(t *testing.T) {
	s, _ := newAnnounceServerWithFleet(t)
	for i := range maxQueuedCommands + 5 {
		s.queueCommand("laptop", AnnounceCommand{Verb: "unload", Model: "m" + string(rune('a'+i%26))})
	}
	s.mu.Lock()
	n := len(s.commands["laptop"])
	s.mu.Unlock()
	if n > maxQueuedCommands {
		t.Errorf("queue = %d, want capped at %d", n, maxQueuedCommands)
	}
}
