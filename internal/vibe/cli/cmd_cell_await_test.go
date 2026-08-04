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
	if err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 5*time.Second, 20*time.Millisecond); err != nil {
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
	err := awaitCell(ctx, &out, awaitTarget(t, ts), "gpu-cell", cond, 0, 20*time.Millisecond)
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
			err := awaitCell(ctx, &out, awaitTarget(t, ts), "gpu-cell", cond, 0, 20*time.Millisecond)
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
	if err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 5*time.Second, 20*time.Millisecond); err != nil {
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
	if err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 2*time.Second, 20*time.Millisecond); err != nil {
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
	err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 300*time.Millisecond, 20*time.Millisecond)
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
	err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 200*time.Millisecond, 20*time.Millisecond)
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
	err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 400*time.Millisecond, 20*time.Millisecond)
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
	err := awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 300*time.Millisecond, time.Second)
	if err == nil {
		t.Fatalf("a cellUp event satisfied a --ready wait:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "qwen starting") {
		t.Errorf("err = %v, want the unmet model condition", err)
	}
}

// leaseFleetd is a fleetd with a mutable lease store, so a test can
// exercise the whole primitive: wait for a holder to clear, then claim.
type leaseFleetd struct {
	mu     sync.Mutex
	leases []fleetapi.Lease
	posted []map[string]string
	status int
}

func newLeaseFleetd(t *testing.T, models []fleetapi.ModelState) (*httptest.Server, *leaseFleetd) {
	t.Helper()
	f := &leaseFleetd{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		leases := append([]fleetapi.Lease{}, f.leases...)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(statusState(awaitTestCell(models, idleActivity(1800), leases...)))
	})
	mux.HandleFunc("POST /api/fleet/lease", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.posted = append(f.posted, body)
		st := f.status
		f.mu.Unlock()
		if st != http.StatusOK {
			http.Error(w, "unknown cell", st)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"holder": body["holder"]})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, f
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
	_ = awaitCell(t.Context(), &out, awaitTarget(t, ts), "gpu-cell", cond, 300*time.Millisecond, 20*time.Millisecond)
	if n := strings.Count(out.String(), "await gpu-cell:"); n != 1 {
		t.Errorf("%d progress lines for one unchanged state:\n%s", n, out.String())
	}
}
