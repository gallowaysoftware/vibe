package fleetapi

// C11 coverage: the hold — a declared, expiring suspension of fleetd's
// own warm policy. The rules under test are the ones a later agent will
// otherwise get wrong: a hold is a LEASE (one store), the ladder is
// drained > held > stale, a hold expires with no release call, a SKIP is
// not an observation of emptiness, and a hold moves nothing on the
// availability or intent axes.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
)

// holdWarmTarget is warmTarget's sibling with the lease store enabled —
// holds live in it, so a server without one cannot take a hold at all.
func holdWarmTarget(t *testing.T, cell, model string, after time.Duration) (*Server, *warmProbe, WarmTarget) {
	t.Helper()
	dir := t.TempDir()
	s := New([]Cell{{Name: cell, URL: "http://127.0.0.1:1", Class: "always_on"}},
		dir+"/hist.json", testDaemonInfo, Options{LeasePath: dir + "/leases.json"})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s, &warmProbe{}, WarmTarget{Cell: cell, Model: model, RestoreAfterIdle: after}
}

// idleSwapOn sets up the state in which every C4/C5 guard passes and the
// restore is CORRECT to fire: a swapped-in model resident, the cell
// announcing fresh and serving, in-flight reported zero, and activity
// observed. Every test below then breaks exactly one thing.
func idleSwapOn(t *testing.T, s *Server, cell, swap string) {
	t.Helper()
	presenceOf(s, cell, AnnounceModel{ID: swap, State: "ready"})
	s.trackInFlight(cell, inflightFrame(t))
}

// TestWarmTarget_ActiveHoldSuppressesTheRestoreAndIssuesNoWarm is the
// phase in one test, and it opens with the positive control: the same
// state WITHOUT a hold restores. Without that control, deleting the hold
// check would still leave a test that "passes" against a policy that
// never fires.
func TestWarmTarget_ActiveHoldSuppressesTheRestoreAndIssuesNoWarm(t *testing.T) {
	s, probe, target := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
	idleSwapOn(t, s, "heavy", "challenger")
	time.Sleep(5 * time.Millisecond) // past restore_after_idle
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}

	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 1 || got[0] != "default-model" {
		t.Fatalf("positive control: restore did not fire with every guard passing: %v (%+v)", got, warmStateOf(s, st))
	}

	if _, err := s.SetHold("heavy", "challenger", "evaluating glm-5", 2*time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 1 {
		t.Fatalf("warmed a HELD cell — the evaluation the operator walked away from is now evicted: %v", got)
	}
	got := warmStateOf(s, st)
	if got.State != "skipped" {
		t.Errorf("state = %s, want skipped", got.State)
	}
	for _, want := range []string{"held: challenger", "left", "evaluating glm-5"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", got.Detail, want)
		}
	}
}

// TestWarmTarget_ExpiredHoldStopsSuppressingWithoutARelease: an operator
// who forgets must not permanently disable their own warm policy. No
// release call appears anywhere in this test — expiry is enforced at
// read, by the lease store, exactly as it is for every other lease.
func TestWarmTarget_ExpiredHoldStopsSuppressingWithoutARelease(t *testing.T) {
	s, probe, target := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
	idleSwapOn(t, s, "heavy", "challenger")
	time.Sleep(5 * time.Millisecond)
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm}

	l, err := s.SetHold("heavy", "challenger", "lunch", time.Hour)
	if err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("warmed while held: %v", got)
	}

	// Time passes. Rewriting the stored expiry is the deterministic form
	// of waiting an hour; the release path is deliberately untouched.
	l.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	s.mu.Lock()
	s.leases[leaseKey(l.Cell, l.Model, l.Holder)] = l
	s.mu.Unlock()

	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 1 || got[0] != "default-model" {
		t.Fatalf("an EXPIRED hold still suppressed the restore: %v (%+v)", got, warmStateOf(s, st))
	}
}

// TestWarmTarget_HoldPrecedence_DrainedWinsHeldBeatsStale pins the
// ladder C11 §3 writes down. Every rung skips, so only the REASON
// differs — and the reason is the whole operator-facing value of the
// status block.
func TestWarmTarget_HoldPrecedence_DrainedWinsHeldBeatsStale(t *testing.T) {
	t.Run("drained outranks held", func(t *testing.T) {
		s, probe, target := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
		idleSwapOn(t, s, "heavy", "challenger")
		if _, err := s.SetHold("heavy", "challenger", "", time.Hour); err != nil {
			t.Fatalf("SetHold: %v", err)
		}
		s.mu.Lock()
		s.intents["heavy"] = Intent{State: "drained", Since: time.Now().UTC()}
		s.mu.Unlock()
		st := &warmTargetState{Cell: target.Cell, Model: target.Model}
		s.evalWarmTarget(target, st, warmLoopConfig{targets: []WarmTarget{target}, warmFn: probe.warm})
		if got := warmStateOf(s, st); got.State != "skipped" || !strings.Contains(got.Detail, "drained") {
			t.Errorf("state = %s/%q, want the drain reason to win", got.State, got.Detail)
		}
	})

	t.Run("held outranks stale", func(t *testing.T) {
		s, probe, target := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
		idleSwapOn(t, s, "heavy", "challenger")
		if _, err := s.SetHold("heavy", "challenger", "", time.Hour); err != nil {
			t.Fatalf("SetHold: %v", err)
		}
		s.mu.Lock()
		s.presence["heavy"].Stale = true
		s.mu.Unlock()
		st := &warmTargetState{Cell: target.Cell, Model: target.Model}
		s.evalWarmTarget(target, st, warmLoopConfig{targets: []WarmTarget{target}, warmFn: probe.warm})
		got := warmStateOf(s, st)
		if got.State != "skipped" || !strings.Contains(got.Detail, "held") {
			t.Errorf("state = %s/%q, want the hold reason (a declaration needs no evidence)", got.State, got.Detail)
		}
		if len(probe.got()) != 0 {
			t.Fatalf("warmed: %v", probe.got())
		}
	})
}

// TestWarmTarget_SkipClearsTheEmptyGraceWindow pins the carried finding.
// The empty-restore requires the emptiness to PERSIST for a grace window
// because presence is heartbeat-stale and a swap mid-cold-start reads as
// "nothing resident" (C4's live gate found this twice). A skip returns
// before any residency is observed, so banking the pre-skip emptiness
// across it lets the first tick after a hold ends fire the restore
// instantly — against exactly the cold start the grace exists for.
func TestWarmTarget_SkipClearsTheEmptyGraceWindow(t *testing.T) {
	s, probe, target := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
	presenceOf(s, "heavy") // announcing, serving, nothing resident
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{
		targets: []WarmTarget{target}, frontURL: "http://front.test",
		warmFn: probe.warm, emptyGrace: 300 * time.Millisecond,
	}

	s.evalWarmTarget(target, st, cfg) // starts the grace window
	if got := warmStateOf(s, st); got.State != "waiting" {
		t.Fatalf("state = %s/%q, want waiting on the grace window", got.State, got.Detail)
	}

	if _, err := s.SetHold("heavy", "challenger", "", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	s.evalWarmTarget(target, st, cfg)
	time.Sleep(350 * time.Millisecond) // the whole grace elapses under the hold
	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("warmed while held: %v", got)
	}

	if _, err := s.ReleaseHold("heavy", "challenger"); err != nil {
		t.Fatalf("ReleaseHold: %v", err)
	}
	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("restored on the first tick after the hold ended, banking emptiness observed before it: %v", got)
	}
	if got := warmStateOf(s, st); !strings.Contains(got.Detail, "confirming") {
		t.Errorf("detail = %q, want the grace window restarted", got.Detail)
	}

	time.Sleep(350 * time.Millisecond)
	s.evalWarmTarget(target, st, cfg)
	if got := probe.got(); len(got) != 1 {
		t.Fatalf("the empty-restore never fired after a full fresh grace window: %v", got)
	}
}

// TestHold_SuppressesScheduledWarm pins the inherited half of §2: C4's
// schedule guard already skips on an active lease, and a hold IS one. A
// later refactor of leasesForCellActive must not silently drop it.
func TestHold_SuppressesScheduledWarm(t *testing.T) {
	s, probe, _ := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
	entry := WarmScheduleEntry{Cron: "* * * * *", Model: "default-model"}
	spec, err := parseCron(entry.Cron)
	if err != nil {
		t.Fatal(err)
	}
	cellOf := func(string) (string, error) { return "heavy", nil }
	now := time.Now()
	s.mu.Lock()
	s.inFlight["heavy"] = observed.Known(0)
	s.schedStates = []*warmScheduleState{{Cron: entry.Cron, Model: entry.Model, NextFire: &now}}
	st := s.schedStates[0]
	s.mu.Unlock()

	if _, err := s.SetHold("heavy", "challenger", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)
	if got := probe.got(); len(got) != 0 {
		t.Fatalf("a scheduled warm fired into a held cell: %v", got)
	}
	if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "held: challenger") {
		t.Errorf("note = %q, want the hold named (an operator reading '1 active leases' learns nothing)", note)
	}

	// Released → the schedule fires again: the hold suspended the policy,
	// it did not disable it.
	if _, err := s.ReleaseHold("heavy", "challenger"); err != nil {
		t.Fatalf("ReleaseHold: %v", err)
	}
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now.Add(time.Minute))
	if got := probe.got(); len(got) != 1 || got[0] != "default-model" {
		t.Fatalf("schedule did not resume after the hold was released: %v", got)
	}
}

// TestHold_RefusesProbe pins the other inherited suppression: a probe
// spends real GPU time, and C8's shared guard already skips a leased
// cell.
func TestHold_RefusesProbe(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}},
		dir+"/hist.json", testDaemonInfo, Options{LeasePath: dir + "/leases.json"})
	t.Cleanup(s.Close)
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready"})
	s.trackInFlight("gpu-cell", inflightFrame(t))

	if why, ok := s.ProbeGuard("gpu-cell", "qwen"); !ok {
		t.Fatalf("positive control: probe refused before any hold (%s)", why)
	}
	if _, err := s.SetHold("gpu-cell", "qwen", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	why, ok := s.ProbeGuard("gpu-cell", "qwen")
	if ok {
		t.Fatal("probed a held cell")
	}
	if !strings.Contains(why, "held: qwen") {
		t.Errorf("refusal = %q, want the hold named with its remaining time", why)
	}
}

// TestHold_IsOneLeaseInTheLeaseStoreAndSurvivesReload is the "no second
// store" gate: everything a hold needs is a property of the lease store,
// so a hold must be visible through every lease surface and must survive
// the restart the lease file exists for.
func TestHold_IsOneLeaseInTheLeaseStoreAndSurvivesReload(t *testing.T) {
	s, ts, opts := newFleetdServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1"},
		{Name: "heavy", URL: "http://127.0.0.1:2", Class: "always_on"},
	})
	if _, err := s.SetHold("heavy", "challenger", "evaluating glm-5", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	// Re-issuing REFRESHES the same key rather than adding a second entry.
	if _, err := s.SetHold("heavy", "challenger", "evaluating glm-5", 2*time.Hour); err != nil {
		t.Fatalf("SetHold refresh: %v", err)
	}

	var wrap struct {
		Leases []Lease `json:"leases"`
	}
	getJSONInto(t, ts.URL+"/api/fleet/leases?cell=heavy", &wrap)
	if len(wrap.Leases) != 1 {
		t.Fatalf("leases = %+v, want exactly one (a re-issue is a refresh)", wrap.Leases)
	}
	l := wrap.Leases[0]
	if !l.Hold || l.Holder != HoldHolder || l.Model != "challenger" {
		t.Errorf("lease = %+v, want a hold under the reserved holder", l)
	}
	if !l.ExpiresAt.After(time.Now().Add(90 * time.Minute)) {
		t.Errorf("expires_at = %s, want the refreshed 2h", l.ExpiresAt)
	}

	// The guard consumers and the cell snapshot see the same entry.
	if got := s.leasesForCellActive("heavy"); len(got) != 1 {
		t.Errorf("leasesForCellActive = %+v, want the hold", got)
	}
	snap := s.snapshotCell(context.Background(), Cell{Name: "heavy", URL: "http://127.0.0.1:2"})
	if len(snap.Leases) != 1 || !snap.Leases[0].Hold {
		t.Errorf("cell snapshot leases = %+v, want the hold (fleet_status, the page and the pre-drain report read this)", snap.Leases)
	}

	// A fleetd restart re-reads the file, hold flag and expiry intact.
	reloaded := loadLeases(opts.LeasePath)
	back, ok := reloaded[leaseKey("heavy", "challenger", HoldHolder)]
	if !ok || !back.Hold || !back.ExpiresAt.Equal(l.ExpiresAt) {
		t.Errorf("reloaded = %+v (ok=%v), want the hold preserved across a restart", back, ok)
	}

	if existed, err := s.ReleaseHold("heavy", "challenger"); err != nil || !existed {
		t.Fatalf("ReleaseHold = %v, %v; want (true, nil)", existed, err)
	}
	if _, held := s.HoldOn("heavy"); held {
		t.Error("hold survived its release")
	}
	// Releasing nothing is not an error, and must not claim it undid one.
	if existed, err := s.ReleaseHold("heavy", "challenger"); err != nil || existed {
		t.Errorf("second ReleaseHold = %v, %v; want (false, nil)", existed, err)
	}
	// A typo'd cell is the one case where "no active hold" is definitely
	// the wrong answer — it reads as "already gone".
	if _, err := s.ReleaseHold("heavvy", "challenger"); err == nil ||
		!strings.Contains(err.Error(), "unknown cell") {
		t.Errorf("ReleaseHold on an unknown cell = %v, want the typo named", err)
	}
}

func TestHold_ValidationRejectsBadTargetsAndDurations(t *testing.T) {
	s, ts, _ := newFleetdServer(t, []Cell{
		{Name: "front", URL: "http://127.0.0.1:1"},
		{Name: "heavy", URL: "http://127.0.0.1:2", Class: "always_on"},
	})

	for _, tc := range []struct {
		name              string
		cell, model, note string
		d                 time.Duration
		want              string
	}{
		{"beyond the 24h bound", "heavy", "challenger", "", 25 * time.Hour, "24h"},
		{"zero duration", "heavy", "challenger", "", 0, "positive"},
		{"negative duration", "heavy", "challenger", "", -time.Hour, "positive"},
		{"unknown cell", "laptop", "challenger", "", time.Hour, "unknown cell"},
		{"the front cell", "front", "challenger", "", time.Hour, "no models of its own"},
		{"empty model", "heavy", "", "", time.Hour, "required"},
		{"control character in the note", "heavy", "challenger", "eval\x1b[2J", time.Hour, "control character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SetHold(tc.cell, tc.model, tc.note, tc.d); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetHold err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}

	// The HTTP endpoint enforces the reserved-holder pairing in BOTH
	// directions: a hold under another holder would be invisible to
	// release_hold, and a plain lease squatting on the reserved holder
	// would be deleted by someone else's release.
	for _, tc := range []struct{ name, body string }{
		{"hold under another holder", `{"cell":"heavy","model":"m","holder":"batch","ttl":"1h","hold":true}`},
		{"plain lease squatting on the reserved holder", `{"cell":"heavy","model":"m","holder":"hold","ttl":"1h"}`},
		{"hold beyond the hold bound but inside the lease bound", `{"cell":"heavy","model":"m","holder":"hold","ttl":"48h","hold":true}`},
		{"hold on the front cell", `{"cell":"front","model":"m","holder":"hold","ttl":"1h","hold":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := postLease(t, ts.URL, http.MethodPost, tc.body); code != http.StatusBadRequest {
				t.Fatalf("POST /api/fleet/lease = %d, want 400", code)
			}
		})
	}
	// The lease store still accepts ordinary leases up to its own bound.
	if code := postLease(t, ts.URL, http.MethodPost, `{"cell":"heavy","model":"m","holder":"batch","ttl":"48h"}`); code != http.StatusOK {
		t.Fatalf("an ordinary 48h lease was rejected (%d) — the hold bound leaked onto leases", code)
	}
}

// TestHold_DoesNotTouchAvailabilityIntentOrTheRender: a hold is fleetd
// policy, not a state axis. It must move nothing an operator or the
// render loop reads as truth about the cell.
func TestHold_DoesNotTouchAvailabilityIntentOrTheRender(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}},
		dir+"/hist.json", testDaemonInfo, Options{LeasePath: dir + "/leases.json"})
	t.Cleanup(s.Close)
	presenceOf(s, "gpu-cell",
		AnnounceModel{ID: "qwen", State: "ready"},
		AnnounceModel{ID: "embed-large", State: "ready"})
	// The announce itself is a membership transition; drain those so the
	// only trigger this test can observe is one the hold caused.
	for drained := true; drained; {
		select {
		case <-s.renderTrigger:
		default:
			drained = false
		}
	}
	if _, err := s.SetHold("gpu-cell", "qwen", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}

	snap := s.snapshotCell(context.Background(), Cell{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"})
	if snap.Display != DisplayServing {
		t.Errorf("a hold changed the cell display to %q", snap.Display)
	}
	if !snap.Reachable {
		t.Error("a hold changed observed availability")
	}
	if snap.Intent != nil {
		t.Errorf("a hold invented declared intent: %+v", snap.Intent)
	}
	if len(snap.Models) != 2 {
		t.Errorf("a hold changed the model set: %+v", snap.Models)
	}
	// The render loop derives the front catalog from presence.Models; a
	// hold that pruned or froze it would 404 the model fleet-wide.
	if p := s.presenceFor("gpu-cell"); p == nil || len(p.Models) != 2 {
		t.Errorf("a hold changed the announced model set: %+v", p)
	}
	select {
	case cell := <-s.renderTrigger:
		t.Fatalf("a hold triggered a front render for %s", cell)
	default:
	}
}

// TestHold_StatusSurfacesShowRemainingTime: "held" without "for how
// long" is the failure mode of a declaration that expires.
func TestHold_StatusSurfacesShowRemainingTime(t *testing.T) {
	s, probe, target := holdWarmTarget(t, "heavy", "default-model", time.Millisecond)
	idleSwapOn(t, s, "heavy", "challenger")
	s.startWarmLoopWithConfig(warmLoopConfig{
		targets: []WarmTarget{target}, frontURL: "http://front.test",
		warmFn: probe.warm, tick: 20 * time.Millisecond,
	})
	if _, err := s.SetHold("heavy", "challenger", "evaluating glm-5", 90*time.Minute); err != nil {
		t.Fatalf("SetHold: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := warmTargetStateOf(s, 0); st.State == "skipped" && strings.Contains(st.Detail, "held: challenger") {
			if !strings.Contains(st.Detail, "1h29m") && !strings.Contains(st.Detail, "1h30m") {
				t.Fatalf("detail = %q, want the remaining time", st.Detail)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the warm block never reported the hold: %+v", warmTargetStateOf(s, 0))
}

// TestFleetPage_RendersHeldRows: the page is the phone-in-the-hallway
// surface, and a hold has to be visible there without a mutation route.
// A static assertion, like the page's other escaping gates — the browser
// run is a live gate.
func TestFleetPage_RendersHeldRows(t *testing.T) {
	data, err := fleetPageFS.ReadFile("fleet.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	if !regexp.MustCompile(`l\.hold`).MatchString(page) {
		t.Error("the page's lease renderer does not branch on the hold flag")
	}
	if !strings.Contains(page, "leftUntil(l.expires_at)") {
		t.Error("the held line does not compute its remaining time from the absolute expires_at")
	}
	// Same rule as every other cell-supplied string on this page.
	if !regexp.MustCompile(`held:.*\$\{esc\(l\.model\)\}`).MatchString(page) {
		t.Error("the held line does not escape the model id")
	}
}

// TestHold_NeedsTheLeaseStore: holds are fleetd-only by construction —
// the store they live in is enabled only in the fleetd role. A daemon
// without one must fail loudly rather than accept a hold nothing will
// honor.
func TestHold_NeedsTheLeaseStore(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}})
	if _, err := s.SetHold("heavy", "challenger", "", time.Hour); err == nil {
		t.Fatal("a hold was accepted with no lease store to keep it in")
	}
}

func getJSONInto(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postLease(t *testing.T, base, method, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, base+"/api/fleet/lease", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s /api/fleet/lease: %v", method, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
