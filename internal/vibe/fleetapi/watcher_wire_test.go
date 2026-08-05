package fleetapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
)

// The in-flight fold is the input to eight busy guards — drain, suspend,
// probe, both warm loops, and the pre-drain report — and llama-swap changed
// its shape between v239 (what the fleet runs) and v240+ (what the next
// `go install` on any cell delivers). These tests are the wire table.
//
// The rule they enforce is not "parse both shapes". It is: a shape this
// build does not recognise must degrade to UNREPORTED, never to a reported
// zero. Every guard already renders unreported as a refusal; a reported
// zero is a positive claim of idleness that no guard can question.

func wireServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, t.TempDir()+"/history.json", func() DaemonInfo { return DaemonInfo{} }, Options{})
	t.Cleanup(s.Close)
	return s
}

// frame builds a real llama-swap inflight payload: the SSE envelope's data
// member is a JSON STRING containing JSON. A test handing trackInFlight a
// bare object hits the first Unmarshal's early return and passes vacuously.
func wireFrame(inner string) json.RawMessage {
	b, err := json.Marshal(inner)
	if err != nil {
		panic(err)
	}
	return b
}

func TestInFlightWireTable(t *testing.T) {
	const (
		req1 = `{"id":"93445","timestamp":"2026-08-05T10:48:49.012790876-03:00","model":"qwen3.6-27b","req_path":"/v1/chat/completions","method":"POST"}`
		req2 = `{"id":"93446","timestamp":"2026-08-05T10:48:50.42678514-03:00","model":"gemma-4-31b","req_path":"/v1/chat/completions","method":"POST"}`
	)
	cases := []struct {
		name     string
		frames   []string
		wantN    int
		wantRep  bool
		wantSeen []string // models that must carry an activity stamp
	}{{
		// Recorded verbatim off llama-swap v239 on 2026-08-05.
		name:     "v239 full list",
		frames:   []string{`{"requests":[` + req1 + `]}`},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b"},
	}, {
		name:     "v239 two requests then one",
		frames:   []string{`{"requests":[` + req1 + `,` + req2 + `]}`, `{"requests":[` + req2 + `]}`},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b", "gemma-4-31b"},
	}, {
		name:     "v239 empty list is a reported zero",
		frames:   []string{`{"requests":[]}`},
		wantN:    0,
		wantRep:  true,
		wantSeen: nil,
	}, {
		name:     "v247 connect snapshot",
		frames:   []string{`{"operation":"snapshot","requests":[` + req1 + `]}`},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b"},
	}, {
		// THE REGRESSION. Before the set-based fold this answered
		// (0, reported): `requests` is omitempty upstream and absent on a
		// delta, so len() read a request STARTING as an idle cell.
		name:     "v247 upsert is one in flight, not zero",
		frames:   []string{`{"operation":"upsert","request":` + req1 + `}`},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b"},
	}, {
		name: "v247 upsert twice then remove one",
		frames: []string{
			`{"operation":"upsert","request":` + req1 + `}`,
			`{"operation":"upsert","request":` + req2 + `}`,
			`{"operation":"remove","id":"93445"}`,
		},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b", "gemma-4-31b"},
	}, {
		name: "v247 full lifecycle returns to zero",
		frames: []string{
			`{"operation":"upsert","request":` + req1 + `}`,
			`{"operation":"remove","id":"93445"}`,
		},
		wantN:    0,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b"},
	}, {
		// A remove naming a request this connection never saw says nothing
		// about the rest of the set — it is what a reconnect produces.
		name: "v247 remove of an unknown id is a no-op",
		frames: []string{
			`{"operation":"upsert","request":` + req1 + `}`,
			`{"operation":"remove","id":"nobody"}`,
		},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b"},
	}, {
		// The load-bearing rule. Not "ignore the frame", not "keep the last
		// value": actively disarm, so every guard refuses.
		name:    "an unrecognised operation UN-reports the cell",
		frames:  []string{`{"requests":[` + req1 + `]}`, `{"operation":"coalesce","batch":[]}`},
		wantN:   0,
		wantRep: false,
	}, {
		name:    "an upsert with no request disarms",
		frames:  []string{`{"operation":"upsert"}`},
		wantN:   0,
		wantRep: false,
	}, {
		name:    "a remove with no id disarms",
		frames:  []string{`{"operation":"remove"}`},
		wantN:   0,
		wantRep: false,
	}, {
		// Recovery: a recognised frame after an unrecognised one re-arms.
		name: "a recognised frame after a disarm re-arms the cell",
		frames: []string{
			`{"operation":"coalesce"}`,
			`{"operation":"snapshot","requests":[` + req1 + `]}`,
		},
		wantN:    1,
		wantRep:  true,
		wantSeen: []string{"qwen3.6-27b"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := wireServer(t)
			for _, f := range tc.frames {
				s.trackInFlight("cell", wireFrame(f))
			}
			n, reported := s.InFlight("cell")
			if n != tc.wantN || reported != tc.wantRep {
				t.Fatalf("InFlight = (%d, %v), want (%d, %v)", n, reported, tc.wantN, tc.wantRep)
			}
			for _, m := range tc.wantSeen {
				if _, ok := s.modelLastActivity("cell", m); !ok {
					t.Errorf("no per-model activity stamp for %s; C4's restore_after_idle window reads it as never-active", m)
				}
			}
		})
	}
}

// TestInFlightUnknownShapeIsNotIdle states the consequence directly, in the
// vocabulary of the guards that consume it. The pre-fix build answered
// (0, true) here, and (0, true) is what every busy guard in the fleet reads
// as "safe to act".
func TestInFlightUnknownShapeIsNotIdle(t *testing.T) {
	s := wireServer(t)
	// The stream is LIVE — this is drift, not a disconnection, and the two
	// must not be reported with the same sentence.
	s.setCellUp("cell", true)
	s.trackInFlight("cell", wireFrame(`{"operation":"whatever-v260-does"}`))

	if n, reported := s.InFlight("cell"); reported {
		t.Fatalf("InFlight = (%d, reported) after an unparseable frame; a reported zero disarms drain, suspend, probe and both warm loops at once", n)
	}
	act := s.activityFor("cell")
	if act.InFlight != nil {
		t.Errorf("the activity block reports in_flight = %d; it must be absent", *act.InFlight)
	}
	if !strings.Contains(act.Reason, "whatever-v260-does") {
		t.Errorf("the reason must name the operation so an operator knows to upgrade, got %q", act.Reason)
	}
}

// TestPerModelActivityStampsOnBothEdgesAcrossWires is C4's completion-edge
// rule, checked on the delta wire where it is hardest: the remove edge
// names ONLY an id, so the model can only come from what the fold
// remembered at upsert time. Without that memory C4's restore_after_idle
// window reads every model as never-active and evicts the operator's model
// as soon as fleetd's uptime passes the window.
func TestPerModelActivityStampsOnBothEdgesAcrossWires(t *testing.T) {
	for _, tc := range []struct{ name, start, end string }{
		{"v239", `{"requests":[{"id":"1","model":"qwen"}]}`, `{"requests":[]}`},
		{"v247", `{"operation":"upsert","request":{"id":"1","model":"qwen"}}`, `{"operation":"remove","id":"1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := wireServer(t)
			s.trackInFlight("cell", wireFrame(tc.start))
			started, ok := s.modelLastActivity("cell", "qwen")
			if !ok {
				t.Fatal("the start edge left no activity stamp")
			}
			time.Sleep(3 * time.Millisecond)
			s.trackInFlight("cell", wireFrame(tc.end))
			finished, ok := s.modelLastActivity("cell", "qwen")
			if !ok {
				t.Fatal("the completion edge left no activity stamp — a generation longer than the idle window reads as idle")
			}
			if !finished.After(started) {
				t.Errorf("completion stamp %v is not after the start stamp %v", finished, started)
			}
		})
	}
}

// TestInFlightClearedOnStreamDrop is the case no existing test in this
// module can express: fleetapi_test.go's fake cell never sends an inflight
// frame at all, so its drop/reconnect tests exercise a state where the bug
// cannot appear.
//
// A count belongs to the connection that produced it. The remove edges that
// would have retired those requests were delivered to nobody, and a real
// llama-swap re-seeds a new connection with a current-state snapshot inside
// ~200ms — so keeping the old count buys nothing and can strand a busy
// verdict forever.
func TestInFlightClearedOnStreamDrop(t *testing.T) {
	cell := swaptest.NewCell(t, swaptest.WithModels(swaptest.Model{ID: "qwen", State: "ready"}))
	s := New([]Cell{{Name: "gpu", URL: cell.URL()}}, t.TempDir()+"/history.json",
		func() DaemonInfo { return DaemonInfo{} }, Options{})
	s.baseBackoff = 5 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)

	wireWait(t, "the connect snapshot", func() bool {
		_, reported := s.InFlight("gpu")
		return reported
	})

	// Open a request and leave it open, then kill the stream under it.
	_, done := cell.StartRequest("qwen", "/v1/chat/completions")
	wireWait(t, "the start edge", func() bool {
		n, _ := s.InFlight("gpu")
		return n == 1
	})
	before := cell.Connects()
	cell.DropStreams()

	wireWait(t, "the count to be discarded with the connection", func() bool {
		_, reported := s.InFlight("gpu")
		return !reported
	})

	// And it comes back on its own, from the new connection's snapshot —
	// which still shows the request, because it is still running.
	if !cell.WaitConnects(before+1, 5*time.Second) {
		t.Fatal("fleetd never reconnected")
	}
	wireWait(t, "the re-seeded count", func() bool {
		n, reported := s.InFlight("gpu")
		return reported && n == 1
	})
	done(swaptest.Tokens{Input: 10, Output: 4}, 50*time.Millisecond)
	wireWait(t, "the completion edge", func() bool {
		n, reported := s.InFlight("gpu")
		return reported && n == 0
	})
}

// TestOversizedLogDataDoesNotFlapTheCell exercises the scanner cap at
// watcher.go's scannerBufMax, which has been an untested assumption:
// bufio.Scanner returns ErrTooLong on a line over the cap, streamCell never
// inspects sc.Err(), and the cell would flap up/down at the backoff floor
// forever — calling persistLastSeen() on every flap. The real connect burst
// carries the whole log history in ONE line (106 KB when this was
// recorded), so the distance to 8 MB is a busy week, not a hypothetical.
func TestOversizedLogDataDoesNotFlapTheCell(t *testing.T) {
	cell := swaptest.NewCell(t)
	s := New([]Cell{{Name: "gpu", URL: cell.URL()}}, t.TempDir()+"/history.json",
		func() DaemonInfo { return DaemonInfo{} }, Options{})
	s.baseBackoff = 5 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)

	wireWait(t, "the first connect", func() bool { return cell.Connects() >= 1 })
	connects := cell.Connects()

	// Comfortably under the cap: the stream must survive it.
	cell.EmitLogData(1 << 20)
	time.Sleep(150 * time.Millisecond)
	if got := cell.Connects(); got != connects {
		t.Errorf("a 1 MB logData frame caused %d reconnect(s); the scanner buffer is meant to absorb it", got-connects)
	}
	if _, reported := s.InFlight("gpu"); !reported {
		t.Error("the cell went unreported across a large logData frame")
	}

	// Over the cap: the stream dies. What must NOT happen is a silent
	// spin — this documents the observable behaviour so a future change to
	// scannerBufMax has something to move.
	cell.EmitLogData(scannerBufMax + (1 << 20))
	if !cell.WaitConnects(connects+1, 5*time.Second) {
		t.Fatal("an over-cap logData frame neither survived nor reconnected; the stream is wedged")
	}
	wireWait(t, "the cell to recover", func() bool {
		_, reported := s.InFlight("gpu")
		return reported
	})
}

func wireWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
