package cli

// C10 coverage, CLI half: the await conditions. The two rules under test
// everywhere below are (a) missing evidence is never idleness, and (b) a
// wait that could never end fails fast instead of parking — C6's finding,
// extended from cell names to model ids.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

func idleActivity(seconds float64) *fleetapi.CellActivity {
	since := time.Now().Add(-time.Duration(seconds) * time.Second).UTC()
	zero := 0
	return &fleetapi.CellActivity{
		Observed: true, ObservedSince: &since, InFlight: &zero, IdleSeconds: &seconds,
	}
}

func unobservedActivity(cell string) *fleetapi.CellActivity {
	return &fleetapi.CellActivity{
		Reason: "no live event stream to " + cell + " — fleetd is not watching it, so silence is not evidence of idleness",
	}
}

// awaitTestCell is the cell every test below shapes.
func awaitTestCell(models []fleetapi.ModelState, act *fleetapi.CellActivity, leases ...fleetapi.Lease) fleetapi.CellSnapshot {
	return fleetapi.CellSnapshot{
		Name: "gpu-cell", URL: "http://gpu.lan:9000", Reachable: true,
		Display: fleetapi.DisplayServing, Models: models, Activity: act, Leases: leases,
	}
}

func awaitTarget(t *testing.T, ts *httptest.Server) fleetdTarget {
	t.Helper()
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// TestCellAwaitReady_BlocksUntilLlamaSwapReportsTheModelReady: the whole
// point of the flag. The poll counter is the assertion that matters —
// unblocking on "starting" would return on the first poll, which is
// exactly the 6-10 minute cold start the batch was trying to avoid.
func TestCellAwaitReady_BlocksUntilLlamaSwapReportsTheModelReady(t *testing.T) {
	var polls atomic.Int64
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		state := "starting"
		if polls.Add(1) > 3 {
			state = "ready"
		}
		snap := statusState(awaitTestCell([]fleetapi.ModelState{{ID: "qwen3.6-27b", State: state}}, idleActivity(0)))
		snap.StartHistory = map[string]fleetapi.StartStats{"qwen3.6-27b": {Count: 4, P50S: 372}}
		return snap
	})

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, model: "qwen3.6-27b", ready: true}
	if _, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 5*time.Second, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n := polls.Load(); n < 4 {
		t.Errorf("returned after %d polls: a non-ready state was accepted as ready", n)
	}
	s := out.String()
	for _, want := range []string{"gpu-cell is up", "qwen3.6-27b ready", "p50 6m12s"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

// TestCellAwaitReady_UnknownModelOnAReachableCellFailsFast is C6's
// unknown-cell rule for model ids: fleetd answered, the cell is up, its
// catalog is populated and the id is not in it. Waiting cannot fix a
// typo, and --timeout 0 would park until reboot.
func TestCellAwaitReady_UnknownModelOnAReachableCellFailsFast(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell([]fleetapi.ModelState{
			{ID: "qwen3.6-27b", State: "ready"}, {ID: "bge-m3", State: "stopped"},
		}, idleActivity(600)))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, model: "qwen3.6-27", ready: true} // typo
	_, err := awaitCell(ctx, &out, awaitTarget(t, ts), "gpu-cell", cond, 0, 20*time.Millisecond)
	if !errors.Is(err, errUnknownModel) {
		t.Fatalf("err = %v, want errUnknownModel", err)
	}
	if !strings.Contains(err.Error(), "bge-m3") {
		t.Errorf("the error does not name the catalog, so the typo is not fixable from it: %v", err)
	}
}

// TestCellAwaitReady_AnEmptyOrUnreachableCatalogIsNotAnUnknownModel: the
// typo verdict needs POSITIVE evidence. A drained cell announces an
// empty model list by design (C4) and an absent one has no catalog at
// all — erroring there would fail the wait during exactly the outage it
// exists to ride out.
func TestCellAwaitReady_AnEmptyOrUnreachableCatalogIsNotAnUnknownModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		cell fleetapi.CellSnapshot
	}{
		{"drained cell, empty catalog", func() fleetapi.CellSnapshot {
			c := awaitTestCell(nil, idleActivity(600))
			c.Display = fleetapi.DisplayDrained
			return c
		}()},
		{"absent cell", func() fleetapi.CellSnapshot {
			c := awaitTestCell(nil, unobservedActivity("gpu-cell"))
			c.Reachable = false
			c.Display = fleetapi.DisplayOffAway
			return c
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := cannedFleetd(t, func() fleetapi.StateSnapshot { return statusState(tc.cell) })
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			var out bytes.Buffer
			cond := awaitConds{wantUp: true, model: "qwen3.6-27b", ready: true}
			_, err := awaitCell(ctx, &out, awaitTarget(t, ts), "gpu-cell", cond, 0, 20*time.Millisecond)
			if errors.Is(err, errUnknownModel) {
				t.Fatalf("called a model unknown without a catalog to judge it against: %v", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the wait to continue until the context died", err)
			}
		})
	}
}

// TestCellAwaitReady_TransportErrorsKeepRetrying: a restarting fleetd is
// what the retry loop exists for, and it must stay distinguishable from
// fleetd answering "no such model".
func TestCellAwaitReady_TransportErrorsKeepRetrying(t *testing.T) {
	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) <= 2 {
			http.Error(w, "fleetd restarting", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(statusState(
			awaitTestCell([]fleetapi.ModelState{{ID: "qwen3.6-27b", State: "ready"}}, idleActivity(600))))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, model: "qwen3.6-27b", ready: true}
	if _, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 5*time.Second, 20*time.Millisecond); err != nil {
		t.Fatalf("a transport error ended the wait: %v", err)
	}
	if !strings.Contains(out.String(), "(retrying)") {
		t.Errorf("the retry was invisible to the operator:\n%s", out.String())
	}
}

// TestAwaitFlagsRejectWaitsThatCouldNeverEnd covers every combination
// that would park forever or contradict itself, all before the first
// poll. The front case is C8's probeGuard rule: the front's rendered
// config is peers-only, so an id there never reports ready.
func TestAwaitFlagsRejectWaitsThatCouldNeverEnd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cell   string
		cond   awaitConds
		ttlSet bool
		want   string
	}{
		{"front cannot report a peer ready", "front",
			awaitConds{wantUp: true, model: "qwen", ready: true}, false, "peers-only"},
		{"--ready without --model", "gpu-cell",
			awaitConds{wantUp: true, ready: true}, false, "--ready needs --model"},
		{"--model without a condition", "gpu-cell",
			awaitConds{wantUp: true, model: "qwen"}, false, "--model needs a condition"},
		{"--down with --idle", "gpu-cell",
			awaitConds{idle: time.Minute}, false, "--down cannot be combined"},
		{"--down with --ready", "gpu-cell",
			awaitConds{model: "qwen", ready: true}, false, "--down cannot be combined"},
		{"--lease without --model", "gpu-cell",
			awaitConds{wantUp: true, lease: leaseClaim{holder: "batch", ttl: time.Hour}}, false, "--lease needs --model"},
		{"--lease-ttl without --lease", "gpu-cell",
			awaitConds{wantUp: true}, true, "--lease-ttl needs --lease"},
		{"negative --idle", "gpu-cell",
			awaitConds{wantUp: true, idle: -time.Second}, false, "--idle must be a positive"},
		{"--lease on C11's reserved holder", "gpu-cell",
			awaitConds{wantUp: true, model: "qwen", ready: true,
				lease: leaseClaim{holder: fleetapi.HoldHolder, ttl: time.Hour}}, false, "reserved for C11 holds"},
		{"--lease-ttl past the store's bound", "gpu-cell",
			awaitConds{wantUp: true, model: "qwen", ready: true,
				lease: leaseClaim{holder: "batch", ttl: 200 * time.Hour}}, true, "may not exceed 168h"},
		{"a control character fleetd would refuse", "gpu-cell",
			awaitConds{wantUp: true, model: "qwen", ready: true,
				lease: leaseClaim{holder: "batch", ttl: time.Hour, note: "1200\nrows"}}, false, "control character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAwaitFlags(tc.cell, tc.cond, tc.ttlSet)
			if err == nil {
				t.Fatalf("accepted %+v", tc.cond)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if err := validateAwaitFlags("gpu-cell", awaitConds{wantUp: true, model: "qwen", ready: true,
		idle: 10 * time.Minute, unleased: true, lease: leaseClaim{holder: "batch", ttl: time.Hour}}, true); err != nil {
		t.Errorf("the documented full invocation was rejected: %v", err)
	}
	// --timeout 0 is the overnight-batch idiom and the flag default; a
	// default that sneaked in would end every parked wait silently.
	if def := cellAwaitCmd().Flags().Lookup("timeout").DefValue; def != "0s" {
		t.Errorf("--timeout default = %q, want %q (0 = wait forever)", def, "0s")
	}
}

// TestCellAwaitIdle_UnblocksOnObservedSilence is the positive control
// for the evidence test below: without it, "never unblocks" would pass
// on an --idle that is simply broken.
func TestCellAwaitIdle_UnblocksOnObservedSilence(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell(nil, idleActivity(900)))
	})
	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: 10 * time.Minute}
	if _, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 2*time.Second, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "idle 15m0s (>= 10m0s)") {
		t.Errorf("output = %q", out.String())
	}
}

// TestCellAwaitIdle_MissingEvidenceNeverUnblocksAndSaysWhy is the rule
// the phase is arranged around: a cell fleetd is not watching produces
// no idle window, and await must keep waiting — visibly — rather than
// fire a batch job into a box it cannot see.
func TestCellAwaitIdle_MissingEvidenceNeverUnblocksAndSaysWhy(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell([]fleetapi.ModelState{{ID: "qwen3.6-27b", State: "ready"}},
			unobservedActivity("gpu-cell")))
	})
	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: 10 * time.Minute}
	_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 300*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("unblocked with no activity evidence: that fires the parked batch into a busy cell")
	}
	if !strings.Contains(err.Error(), "idle unknown") || !strings.Contains(err.Error(), "not watching it") {
		t.Errorf("the timeout error does not name the missing evidence: %v", err)
	}
	if !strings.Contains(out.String(), "will not treat missing evidence as idleness") {
		t.Errorf("the refusal was silent:\n%s", out.String())
	}
}

// TestCellAwaitIdle_AnOlderFleetdSendsNoActivityBlockAndSaysSo: version
// skew is guaranteed in this fleet (C3's announce rule). A pre-C10
// fleetd sends no activity field, and "no evidence for this cell" would
// send the operator to look at the cell instead of the registry.
func TestCellAwaitIdle_AnOlderFleetdSendsNoActivityBlockAndSaysSo(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell(nil, nil))
	})
	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: 10 * time.Minute}
	_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 200*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("unblocked against a fleetd that reports no activity at all")
	}
	if !strings.Contains(err.Error(), "pre-C10 fleetd") {
		t.Errorf("err = %v, want the skew named", err)
	}
}

// TestCellAwaitTimeout_NamesTheRegistryFailureNotTheCell: with fleetd
// unreachable for the whole wait there are no unmet conditions to
// report, and "the cell never came up" blames the wrong box.
func TestCellAwaitTimeout_NamesTheRegistryFailureNotTheCell(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fleetd restarting", http.StatusBadGateway)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, model: "qwen", ready: true, idle: time.Minute}
	_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 200*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "last attempt failed") || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want the transport failure named", err)
	}
}

// TestCellAwaitIdle_AnAbsurdIdleFromTheWireIsClamped: the number arrives
// over a network and float→int64 is implementation-defined out of range.
// Clamping keeps a garbled value from becoming a random duration.
func TestCellAwaitIdle_AnAbsurdIdleFromTheWireIsClamped(t *testing.T) {
	for _, secs := range []float64{-1, 1e30} {
		act := idleActivity(0)
		act.IdleSeconds = &secs
		snap := statusState(awaitTestCell(nil, act))
		cond := awaitConds{wantUp: true, idle: 10 * time.Minute}
		ev, err := cond.evaluate(&snap, "gpu-cell")
		if err != nil {
			t.Fatal(err)
		}
		if secs < 0 && ev.ok {
			t.Errorf("a negative idle satisfied a 10m window")
		}
		if secs > 0 && !ev.ok {
			t.Errorf("a clamped huge idle did not satisfy a 10m window: %s", ev.status)
		}
	}
}

// TestCellAwaitIdle_InFlightRequestsAreNotIdleEvenPastTheWindow pins the
// CLI's precedence: a reported in-flight count outranks any idle number
// beside it. C4 learned this the hard way — one generation longer than
// the window reads as idle from timestamps alone.
func TestCellAwaitIdle_InFlightRequestsAreNotIdleEvenPastTheWindow(t *testing.T) {
	act := idleActivity(3600)
	two := 2
	act.InFlight = &two
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot { return statusState(awaitTestCell(nil, act)) })

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: 10 * time.Minute}
	_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 200*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("unblocked on a busy cell")
	}
	if !strings.Contains(out.String(), "2 request(s) in flight") {
		t.Errorf("output = %q", out.String())
	}
}

// TestCellAwaitComposite_ConditionsMustHoldInTheSameSnapshot: the cell
// alternates between ready-but-busy and idle-but-not-ready, so every
// condition is true in some poll and they are never true together.
func TestCellAwaitComposite_ConditionsMustHoldInTheSameSnapshot(t *testing.T) {
	var polls atomic.Int64
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		if polls.Add(1)%2 == 0 {
			return statusState(awaitTestCell(
				[]fleetapi.ModelState{{ID: "qwen", State: "ready"}}, idleActivity(0)))
		}
		return statusState(awaitTestCell(
			[]fleetapi.ModelState{{ID: "qwen", State: "starting"}}, idleActivity(3600)))
	})
	var out bytes.Buffer
	cond := awaitConds{wantUp: true, model: "qwen", ready: true, idle: 10 * time.Minute}
	_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 400*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("unblocked on conditions that were never true in one snapshot")
	}
	if polls.Load() < 4 {
		t.Fatalf("only %d polls: the test did not exercise both phases", polls.Load())
	}
}

// TestAwaitEvaluate_JudgesEveryConditionAgainstOneSnapshot is the
// clock-free unit view of the same composite.
func TestAwaitEvaluate_JudgesEveryConditionAgainstOneSnapshot(t *testing.T) {
	snap := statusState(awaitTestCell(
		[]fleetapi.ModelState{{ID: "qwen", State: "ready"}}, idleActivity(900),
		fleetapi.Lease{Cell: "gpu-cell", Model: "qwen", Holder: "other-batch", ExpiresAt: time.Now().Add(time.Hour)}))
	cond := awaitConds{wantUp: true, model: "qwen", ready: true, idle: 10 * time.Minute, unleased: true}

	ev, err := cond.evaluate(&snap, "gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	if ev.ok {
		t.Fatal("satisfied while another holder's lease is live")
	}
	if len(ev.unmet) != 1 || !strings.Contains(ev.unmet[0], "other-batch") {
		t.Fatalf("unmet = %v, want only the lease", ev.unmet)
	}
	// Every satisfied condition is still reported, so the success line and
	// the timeout error describe the same wait.
	for _, want := range []string{"qwen ready", "idle 15m0s"} {
		if !strings.Contains(ev.status, want) {
			t.Errorf("status %q missing %q", ev.status, want)
		}
	}
	if _, err := cond.evaluate(&snap, "nope"); !errors.Is(err, errUnknownCell) {
		t.Errorf("unknown cell: %v", err)
	}
}

// TestCellAwaitExtras_ATransitionEventDoesNotSatisfyAModelCondition:
// C3's events fast-path returns the moment a cellUp frame arrives, which
// is sound for a reachability wait and wrong for every other condition —
// the cell coming back says nothing about whether the model is warm.
func TestCellAwaitExtras_ATransitionEventDoesNotSatisfyAModelCondition(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(
			[]fleetapi.ModelState{{ID: "qwen", State: "starting"}}, idleActivity(3600))))
	})
	mux.HandleFunc("GET /api/fleet/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				fmt.Fprint(w, "event:message\ndata:{\"cell\":\"gpu-cell\",\"type\":\"fleet.cellUp\"}\n\n")
				flusher.Flush()
			}
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, model: "qwen", ready: true}
	_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 300*time.Millisecond, time.Second)
	if err == nil {
		t.Fatalf("a cellUp event satisfied a --ready wait:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "qwen starting") {
		t.Errorf("err = %v, want the unmet model condition", err)
	}
}

// TestCellAwaitReleasesItsEventStreamOnSuccess: the events goroutine
// only exits on the context, and under `--timeout 0` (the overnight
// idiom) awaitCell used to return without cancelling anything. Harmless
// in a process about to exit; a leak in a function tests and loops call.
func TestCellAwaitReleasesItsEventStreamOnSuccess(t *testing.T) {
	var once sync.Once
	connected := make(chan struct{})
	released := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		<-connected // the stream is established before the wait can succeed
		_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(nil, idleActivity(0))))
	})
	mux.HandleFunc("GET /api/fleet/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		once.Do(func() { close(connected) })
		<-r.Context().Done()
		close(released)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	if _, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", awaitConds{wantUp: true}, 0, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("the events stream is still open after the wait returned")
	}
}

// leaseFleetd is a fleetd with a mutable lease store and C9's notify
// route, so a test can exercise the whole primitive: wait for a holder
// to clear, claim, and page. `calls` is the ORDERED route log — the
// C9/C10 merge decided that the push goes after the claim, and order is
// the only thing that pins it.
type leaseFleetd struct {
	mu           sync.Mutex
	leases       []fleetapi.Lease
	posted       []map[string]string
	notified     []map[string]string
	calls        []string
	status       int
	notifyStatus int
	activity     *fleetapi.CellActivity
}

func newLeaseFleetd(t *testing.T, models []fleetapi.ModelState) (*httptest.Server, *leaseFleetd) {
	t.Helper()
	f := &leaseFleetd{status: http.StatusOK, notifyStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		leases := append([]fleetapi.Lease{}, f.leases...)
		act := f.activity
		f.mu.Unlock()
		if act == nil {
			act = idleActivity(1800)
		}
		_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(models, act, leases...)))
	})
	mux.HandleFunc("POST /api/fleet/lease", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posted = append(f.posted, body)
		f.calls = append(f.calls, "lease")
		st := f.status
		f.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, "unknown cell", st)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"holder": body["holder"]})
	})
	mux.HandleFunc("POST /api/fleet/notify/send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.notified = append(f.notified, body)
		f.calls = append(f.calls, "notify")
		st := f.notifyStatus
		f.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, "fleet notifications are not configured", st)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, f
}

// snapshot copies the recorded traffic under the lock.
func (f *leaseFleetd) snapshot() (calls []string, notified []map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...), append([]map[string]string{}, f.notified...)
}

// runAwait drives the real cobra command, which is the only path that
// includes flag validation and the lease claim.
func runAwait(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := cellAwaitCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

// TestCellAwaitLeases_OtherHoldersBlockOwnHolderDoesNotAndSuccessClaims
// is the composition the futures entry promised: await + idle + leases
// is a queue of one batch at a time.
func TestCellAwaitLeases_OtherHoldersBlockOwnHolderDoesNotAndSuccessClaims(t *testing.T) {
	ts, f := newLeaseFleetd(t, []fleetapi.ModelState{{ID: "qwen", State: "ready"}})
	base := []string{"gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
		"--idle", "10m", "--unleased", "--interval", "20ms"}

	// 1. Another consumer holds the cell: the wait does not end.
	f.mu.Lock()
	f.leases = []fleetapi.Lease{{Cell: "gpu-cell", Model: "qwen", Holder: "other-batch", ExpiresAt: time.Now().Add(time.Hour)}}
	f.mu.Unlock()
	out, err := runAwait(t, append(append([]string{}, base...), "--lease", "nightly-eval", "--timeout", "300ms")...)
	if err == nil {
		t.Fatal("unblocked while another holder had the cell")
	}
	if !strings.Contains(err.Error(), "other-batch") {
		t.Errorf("timeout error does not name the blocker: %v (%s)", err, out)
	}

	// 2. The cell's own residue never blocks its own re-run.
	f.mu.Lock()
	f.leases = []fleetapi.Lease{{Cell: "gpu-cell", Model: "qwen", Holder: "nightly-eval", ExpiresAt: time.Now().Add(time.Hour)}}
	f.mu.Unlock()
	out, err = runAwait(t, append(append([]string{}, base...), "--lease", "nightly-eval", "--lease-ttl", "2h", "--lease-note", "1200 rows", "--timeout", "2s")...)
	if err != nil {
		t.Fatalf("a stale lease under our own holder deadlocked the re-run: %v (%s)", err, out)
	}
	if !strings.Contains(out, "lease held: nightly-eval") {
		t.Errorf("output = %q", out)
	}
	f.mu.Lock()
	posted := append([]map[string]string{}, f.posted...)
	f.mu.Unlock()
	if len(posted) != 1 {
		t.Fatalf("lease POSTs = %d, want exactly one", len(posted))
	}
	for k, want := range map[string]string{"cell": "gpu-cell", "model": "qwen", "holder": "nightly-eval", "ttl": "2h0m0s", "note": "1200 rows"} {
		if posted[0][k] != want {
			t.Errorf("lease %s = %q, want %q", k, posted[0][k], want)
		}
	}

	// 3. And that claim is what makes the NEXT batch wait.
	f.mu.Lock()
	f.leases = []fleetapi.Lease{{Cell: "gpu-cell", Model: "qwen", Holder: "nightly-eval", ExpiresAt: time.Now().Add(time.Hour)}}
	f.mu.Unlock()
	_, err = runAwait(t, append(append([]string{}, base...), "--lease", "second-batch", "--timeout", "300ms")...)
	if err == nil || !strings.Contains(err.Error(), "nightly-eval") {
		t.Errorf("the second batch did not queue behind the first: %v", err)
	}
}

// TestCellAwaitLease_ARefusedClaimFailsTheCommand: the operator asked
// for the declaration, and a batch that runs undeclared is invisible to
// the pre-drain report that exists to protect it. `&&` must not proceed.
func TestCellAwaitLease_ARefusedClaimFailsTheCommand(t *testing.T) {
	ts, f := newLeaseFleetd(t, []fleetapi.ModelState{{ID: "qwen", State: "ready"}})
	f.mu.Lock()
	f.status = http.StatusBadRequest
	f.mu.Unlock()

	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
		"--idle", "10m", "--lease", "nightly-eval", "--interval", "20ms", "--timeout", "2s")
	if err == nil {
		t.Fatal("exit 0 with no lease taken")
	}
	if !strings.Contains(err.Error(), "lease was refused") {
		t.Errorf("err = %v (%s)", err, out)
	}
}

// TestCellAwaitCmd_PlainUpIsUnchanged guards the C1 idiom: every new
// condition is opt-in, and the existing invocation must behave exactly
// as it did.
func TestCellAwaitCmd_PlainUpIsUnchanged(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell(nil, unobservedActivity("gpu-cell")))
	})
	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--up", "--interval", "20ms", "--timeout", "2s")
	if err != nil {
		t.Fatalf("plain --up failed: %v (%s)", err, out)
	}
	if strings.TrimSpace(out) != "gpu-cell is up" {
		t.Errorf("output = %q, want the C1 line verbatim", out)
	}
	if _, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--up", "--down"); err == nil {
		t.Error("--up --down accepted")
	}
}

// TestCellAwaitProgressLinesPrintOnlyOnChange: a ten-minute idle window
// at a five-second poll is 120 identical lines otherwise.
func TestCellAwaitProgressLinesPrintOnlyOnChange(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell(nil, idleActivity(60)))
	})
	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: time.Hour}
	_, _ = awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 300*time.Millisecond, 20*time.Millisecond)
	if n := strings.Count(out.String(), "await gpu-cell:"); n != 1 {
		t.Errorf("%d progress lines for one unchanged state:\n%s", n, out.String())
	}
}

// ─── adversarial review pass (C10) ──────────────────────────────────────────

// TestCellAwaitDoesNotRePollOnUnrelatedEvents: fleetd forwards EVERY
// upstream llama-swap payload from EVERY cell onto /api/fleet/events
// (logData, modelStatus, metrics — handleUpstream publishes them all),
// and /api/fleet/state is an uncached probe round of the whole fleet.
// Falling through to a re-poll on any frame turns the long wait --idle
// exists for into a probe flood against the llama-swaps that are
// serving the traffic. Only a matching transition for THIS cell is a
// reason to re-evaluate, which is what the phase doc already said.
func TestCellAwaitDoesNotRePollOnUnrelatedEvents(t *testing.T) {
	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(nil, idleActivity(60))))
	})
	mux.HandleFunc("GET /api/fleet/events", func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			// A busy neighbour and a log frame from the awaited cell: both
			// are noise for every condition await can evaluate.
			fmt.Fprint(w, "event:message\ndata:{\"cell\":\"other-cell\",\"type\":\"fleet.cellUp\"}\n\n")
			fmt.Fprint(w, "event:message\ndata:{\"cell\":\"gpu-cell\",\"type\":\"logData\"}\n\n")
			flusher.Flush()
			time.Sleep(time.Millisecond)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: time.Hour}
	_, _ = awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 600*time.Millisecond, 200*time.Millisecond)
	// 600ms at a 200ms tick is 4 polls; anything near the ~600 frames the
	// stream delivered means the poll rate is the fleet's event rate.
	if n := polls.Load(); n > 12 {
		t.Errorf("%d state polls in 600ms at a 200ms interval: unrelated SSE frames are driving the poll loop", n)
	}
}

// TestCellAwaitLease_ReservedHolderAndOverBoundTTLAreRefusedBeforeTheWait:
// both are refusals fleetd would issue anyway, and the whole question is
// WHEN. The claim is POSTed after the wait, so under `--timeout 0` a
// batch parks all night and then exits non-zero on a 400 that was
// decidable before the first poll. `hold` is worse than late: C11
// reserves it, and --unleased skips its own holder, so `--unleased
// --lease hold` steps over the operator's do-not-touch declaration on
// its way to failing.
func TestCellAwaitLease_ReservedHolderAndOverBoundTTLAreRefusedBeforeTheWait(t *testing.T) {
	var polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(
			[]fleetapi.ModelState{{ID: "qwen", State: "ready"}}, idleActivity(1800))))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	for _, tc := range []struct{ name, ttl, want string }{
		{"reserved holder", "1h", "reserved for C11 holds"},
		{"over-bound ttl", "200h", "may not exceed 168h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holder := "nightly-eval"
			if tc.name == "reserved holder" {
				holder = fleetapi.HoldHolder
			}
			polls.Store(0)
			_, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
				"--unleased", "--lease", holder, "--lease-ttl", tc.ttl, "--interval", "20ms", "--timeout", "2s")
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if polls.Load() != 0 {
				t.Errorf("%d polls: the refusal came after the wait started, which under --timeout 0 is a night later", polls.Load())
			}
		})
	}
}

// TestCellAwaitIdle_ASatisfiedWindowStillNamesTheEvidenceGap: §3 rule 2
// of the phase doc promises the unreported-in-flight qualification is
// "surfaced in the status line either way". The MET path is the half
// that matters — this is the one reading of --idle that rests on the
// cell's silence rather than on an edge fleetd watched, so if v239 turns
// out not to emit inflight frames at all (live gate b, NOT RUN), a
// silent success line is the M2 trap with nothing on screen to catch it.
func TestCellAwaitIdle_ASatisfiedWindowStillNamesTheEvidenceGap(t *testing.T) {
	act := idleActivity(900)
	act.InFlight = nil // no frame has ever arrived from this cell
	act.Reason = "no inflight frame seen yet; idle measured from the stream connect"
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot { return statusState(awaitTestCell(nil, act)) })

	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: 10 * time.Minute}
	if _, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 2*time.Second, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no inflight frame seen yet") {
		t.Errorf("the success line dropped fleetd's qualification, so the gap is invisible:\n%s", out.String())
	}
}

// TestCellAwaitProgressSurvivesAMovingIdleCounter: the dedupe has to key
// on the STATE, not the rendered text. idle_s grows on every poll
// against a real fleetd, so deduplicating on the line deduplicates
// nothing — an overnight `--timeout 0 --idle` wait logs one line per
// poll for eight hours. The original test froze idle_s and so could
// never see it.
func TestCellAwaitProgressSurvivesAMovingIdleCounter(t *testing.T) {
	var polls atomic.Int64
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(awaitTestCell(nil, idleActivity(float64(polls.Add(1)))))
	})
	var out bytes.Buffer
	cond := awaitConds{wantUp: true, idle: time.Hour}
	_, _ = awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 400*time.Millisecond, 20*time.Millisecond)
	if polls.Load() < 5 {
		t.Fatalf("only %d polls: the test did not exercise the moving counter", polls.Load())
	}
	if n := strings.Count(out.String(), "await gpu-cell:"); n != 1 {
		t.Errorf("%d progress lines across %d polls of one unchanged state:\n%s", n, polls.Load(), out.String())
	}
}

// TestCellAwaitUnleased_AHoldIsNamedAsAHold: C11's rule for surfaces
// with no hold flag — key on the reserved holder. "leased by hold" reads
// as a consumer with an odd name, not as the operator's declaration.
func TestCellAwaitUnleased_AHoldIsNamedAsAHold(t *testing.T) {
	snap := statusState(awaitTestCell(nil, idleActivity(900),
		fleetapi.Lease{Cell: "gpu-cell", Model: "qwen", Holder: fleetapi.HoldHolder, Hold: true,
			ExpiresAt: time.Now().Add(time.Hour)}))
	cond := awaitConds{wantUp: true, unleased: true}
	ev, err := cond.evaluate(&snap, "gpu-cell")
	if err != nil {
		t.Fatal(err)
	}
	if ev.ok {
		t.Fatal("--unleased ignored an active hold")
	}
	if !strings.Contains(ev.status, "held: qwen") {
		t.Errorf("status = %q, want the hold named as one", ev.status)
	}
}

// TestCellAwaitDown_AStaleAnnouncerIsNotADownCell is C6's finding on the
// path C6 did not reach. Staleness retires the ANNOUNCE, never the
// probe: `presence.go`'s not-fresh branch sets `snap.Reachable = probeOK`
// precisely so a cell whose announcer died while llama-swap keeps
// serving stays reachable. The events fast-path took `fleet.cellStale`
// as a verdict anyway, so `await --down` reported "down" — with the
// event named in the line, which is worse — for a cell still answering
// every request. `fleet.cellWithdrawn` is the same shape (C6 gates the
// withdraw's HostReachable=false on the probe failing too).
func TestCellAwaitDown_AStaleAnnouncerIsNotADownCell(t *testing.T) {
	for _, evType := range []string{"fleet.cellStale", "fleet.cellWithdrawn"} {
		t.Run(evType, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
				// The cell is answering: llama-swap is fine, its announcer is not.
				_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(
					[]fleetapi.ModelState{{ID: "qwen", State: "ready"}}, idleActivity(60))))
			})
			mux.HandleFunc("GET /api/fleet/events", func(w http.ResponseWriter, r *http.Request) {
				flusher := w.(http.Flusher)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher.Flush()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(10 * time.Millisecond):
						fmt.Fprintf(w, "event:message\ndata:{\"cell\":\"gpu-cell\",\"type\":%q}\n\n", evType)
						flusher.Flush()
					}
				}
			})
			ts := httptest.NewServer(mux)
			t.Cleanup(ts.Close)

			var out bytes.Buffer
			_, err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", awaitConds{}, 300*time.Millisecond, time.Second)
			if err == nil {
				t.Fatalf("%s unblocked a --down wait against a cell that is still answering:\n%s", evType, out.String())
			}
			if !strings.Contains(err.Error(), "timeout waiting for gpu-cell") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// ─── the C9 union: --notify beside the lease claim ──────────────────────────
//
// C9 and C10 both grew `vibe cell await`. Everything below pins the one
// question git could not answer: where the push sits relative to the
// claim, and what a refusal does.

// TestCellAwaitNotify_FiresAfterTheLeaseClaimAndCarriesItsOutcome is the
// ordering decision itself. The push goes LAST, and its message is the
// terminal's success line verbatim plus the claim's result — a page that
// says "the wait ended" while the box went to someone else is a page
// that lied, and the phone is the only surface a sleeping operator has.
func TestCellAwaitNotify_FiresAfterTheLeaseClaimAndCarriesItsOutcome(t *testing.T) {
	ts, f := newLeaseFleetd(t, []fleetapi.ModelState{{ID: "qwen", State: "ready"}})
	// An observed window fleetd QUALIFIES: C10 puts the qualification on
	// the success line, and dropping it from the push would be the same
	// evidence gap on the surface that matters most.
	secs, zero := 1800.0, 0
	since := time.Now().Add(-time.Hour).UTC()
	f.mu.Lock()
	f.activity = &fleetapi.CellActivity{
		Observed: true, ObservedSince: &since, InFlight: &zero, IdleSeconds: &secs,
		Reason: "no inflight frame has ever arrived on this stream",
	}
	f.mu.Unlock()

	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
		"--idle", "10m", "--lease", "nightly-eval", "--lease-ttl", "2h", "--notify",
		"--interval", "20ms", "--timeout", "2s")
	if err != nil {
		t.Fatalf("await failed: %v (%s)", err, out)
	}
	calls, notified := f.snapshot()
	if len(calls) != 2 || calls[0] != "lease" || calls[1] != "notify" {
		t.Fatalf("route order = %v, want exactly [lease notify]", calls)
	}
	msg := notified[0]["message"]
	// The terminal's own success line, character for character: ONE
	// renderer, so the two surfaces cannot describe one wait differently
	// — and the qualification fleetd attached rides both.
	var success string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "gpu-cell is up") {
			success = line
		}
	}
	if success == "" {
		t.Fatalf("no success line in output %q", out)
	}
	if !strings.Contains(msg, "the wait on gpu-cell ended: "+success) {
		t.Errorf("notify message %q does not repeat the terminal line %q", msg, success)
	}
	for _, want := range []string{"qwen ready", "idle 30m0s (>= 10m0s)",
		"no inflight frame has ever arrived on this stream",
		"lease held: nightly-eval on gpu-cell/qwen for 2h0m0s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("notify message %q is missing %q", msg, want)
		}
	}
	if want := "vibe fleet: gpu-cell is up"; notified[0]["title"] != want {
		t.Errorf("title = %q, want %q", notified[0]["title"], want)
	}
}

// TestCellAwaitNotify_ARefusedLeaseStillPagesAndStillFailsTheCommand:
// the case the ordering exists for. C10 fails the command on a refused
// claim; skipping the push there would answer "is the box mine" with
// silence, which reads as yes to someone who went to bed.
func TestCellAwaitNotify_ARefusedLeaseStillPagesAndStillFailsTheCommand(t *testing.T) {
	ts, f := newLeaseFleetd(t, []fleetapi.ModelState{{ID: "qwen", State: "ready"}})
	f.mu.Lock()
	f.status = http.StatusBadRequest
	f.mu.Unlock()

	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
		"--lease", "nightly-eval", "--notify", "--interval", "20ms", "--timeout", "2s")
	if err == nil || !strings.Contains(err.Error(), "lease was refused") {
		t.Fatalf("err = %v (%s), want the command to fail on the refusal", err, out)
	}
	calls, notified := f.snapshot()
	if len(calls) != 2 || calls[0] != "lease" || calls[1] != "notify" {
		t.Fatalf("route order = %v, want exactly [lease notify]", calls)
	}
	if !strings.Contains(notified[0]["title"], "but the lease was refused") {
		t.Errorf("title = %q: the page does not say the box is not ours", notified[0]["title"])
	}
	for _, want := range []string{"lease REFUSED for holder nightly-eval", "HTTP 400",
		"the command exited non-zero and nothing is holding gpu-cell"} {
		if !strings.Contains(notified[0]["message"], want) {
			t.Errorf("notify message %q is missing %q", notified[0]["message"], want)
		}
	}
}

// TestCellAwaitNotify_APlainWaitPagesOnceWithNoLeaseLine keeps C9's own
// invocation intact: --notify without --lease is one push about the
// wait, and nothing about ownership.
func TestCellAwaitNotify_APlainWaitPagesOnceWithNoLeaseLine(t *testing.T) {
	ts, f := newLeaseFleetd(t, nil)
	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--up", "--notify",
		"--interval", "20ms", "--timeout", "2s")
	if err != nil {
		t.Fatalf("await failed: %v (%s)", err, out)
	}
	calls, notified := f.snapshot()
	if len(calls) != 1 || calls[0] != "notify" {
		t.Fatalf("route calls = %v, want exactly [notify]", calls)
	}
	if got := notified[0]["message"]; !strings.Contains(got, "the wait on gpu-cell ended: gpu-cell is up") ||
		strings.Contains(got, "lease") {
		t.Errorf("notify message = %q", got)
	}
}

// TestCellAwaitNotify_AFailedWaitPagesNothing: --notify is await-
// UNBLOCKED (C9's word). A phone that buzzes for timeouts and typos too
// has taught its owner to ignore it, which is the failure the whole
// alarm policy is written against.
func TestCellAwaitNotify_AFailedWaitPagesNothing(t *testing.T) {
	ts, f := newLeaseFleetd(t, []fleetapi.ModelState{{ID: "qwen", State: "starting"}})
	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
		"--notify", "--interval", "20ms", "--timeout", "200ms")
	if err == nil {
		t.Fatalf("the wait unblocked on a starting model (%s)", out)
	}
	if calls, _ := f.snapshot(); len(calls) != 0 {
		t.Errorf("a timed-out wait pushed %v", calls)
	}

	// Same for the fail-fast typo path, which never even reaches a poll
	// the condition could satisfy.
	_, err = runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwn", "--ready",
		"--notify", "--interval", "20ms", "--timeout", "0")
	if err == nil {
		t.Fatal("unknown model did not fail fast")
	}
	if calls, _ := f.snapshot(); len(calls) != 0 {
		t.Errorf("a fail-fast typo pushed %v", calls)
	}
}

// TestCellAwaitNotify_APushFailureWarnsAndKeepsTheExitCode: the wait
// succeeded and the lease is held. An unconfigured webhook must not turn
// that into a failed command — the delivery is the best-effort half.
func TestCellAwaitNotify_APushFailureWarnsAndKeepsTheExitCode(t *testing.T) {
	ts, f := newLeaseFleetd(t, []fleetapi.ModelState{{ID: "qwen", State: "ready"}})
	f.mu.Lock()
	f.notifyStatus = http.StatusServiceUnavailable
	f.mu.Unlock()

	out, err := runAwait(t, "gpu-cell", "--api", ts.URL, "--model", "qwen", "--ready",
		"--lease", "nightly-eval", "--notify", "--interval", "20ms", "--timeout", "2s")
	if err != nil {
		t.Fatalf("a failed push failed the command: %v (%s)", err, out)
	}
	if !strings.Contains(out, "warning: --notify push failed") {
		t.Errorf("the failure was swallowed: %q", out)
	}
	if !strings.Contains(out, "lease held: nightly-eval") {
		t.Errorf("the lease report went missing: %q", out)
	}
}

// TestNotifyPayloadIsBoundedAndPrintable: fleetd 400s a title carrying a
// control character and caps the message, and the refusal body this
// message quotes is up to 4 KB of whatever fleetd wrote. A --notify that
// 400s is a human who never gets paged.
func TestNotifyPayloadIsBoundedAndPrintable(t *testing.T) {
	if got := notifyText("a\x1b[31mb\tc", 100); got != "a[31mb c" {
		t.Errorf("notifyText = %q", got)
	}
	long := strings.Repeat("x", 5000)
	if got := clampBytes(long, maxNotifyMessageBytes); len(got) > maxNotifyMessageBytes {
		t.Errorf("clamped message = %d bytes, want <= %d", len(got), maxNotifyMessageBytes)
	}
	// A multi-byte rune must not be cut in half on the way out.
	runes := strings.Repeat("é", 200)
	got := clampBytes(runes, 101)
	if len(got) > 101 || !utf8.ValidString(got) {
		t.Errorf("clampBytes split a rune or overran: %q (%d bytes)", got, len(got))
	}
}
