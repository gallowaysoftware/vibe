package fleetapi

// C4 coverage: warm-target idle-window state machine, cron next-fire
// math (incl. DST), schedule guard behavior, and the fleet page route.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// warmProbe captures warm calls and scripts llama-swap cells.
type warmProbe struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (p *warmProbe) warm(ctx context.Context, frontURL, model string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, model)
	return p.err
}

func (p *warmProbe) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.calls...)
}

func newWarmServer(t *testing.T, cells []Cell) *Server {
	t.Helper()
	s := New(cells, t.TempDir()+"/hist.json", testDaemonInfo, Options{})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s
}

func presenceOf(s *Server, cell string, models ...AnnounceModel) {
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: cell, Seq: 1,
		Intent: &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Models: models,
	})
}

func TestWarmTarget_IdleWindowStateMachine(t *testing.T) {
	probe := &warmProbe{}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: 200 * time.Millisecond}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	cfg := warmLoopConfig{
		targets:   []WarmTarget{target},
		frontURL:  "http://front.test",
		warmFn:    probe.warm,
		tick:      30 * time.Millisecond,
		idleFloor: time.Hour,
	}
	s.startWarmLoopWithConfig(cfg)

	// Operator swap resident: the target must NOT warm, and requests
	// must reset the window.
	presenceOf(s, "heavy", AnnounceModel{ID: "challenger", State: "ready"})
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(probe.got()) > 0 {
			t.Fatal("warmed while the swap was resident and active — the pin-via-keep-warm bug")
		}
		// Keep the swap busy: requests reset the idle window.
		s.mu.Lock()
		s.modelActivity["heavy\x00challenger"] = time.Now().UTC()
		s.mu.Unlock()
		time.Sleep(40 * time.Millisecond)
	}

	// Now go quiet: after restore_after_idle of true idleness the
	// default warms exactly once, then holds (resident → holding).
	idleStart := time.Now()
	for time.Since(idleStart) < 2*time.Second {
		if got := probe.got(); len(got) == 1 {
			if got[0] != "default-model" {
				t.Fatalf("warmed %v, want [default-model]", got)
			}
			// Simulate the model loading: target becomes resident.
			presenceOf(s, "heavy", AnnounceModel{ID: "default-model", State: "ready"}, AnnounceModel{ID: "challenger", State: "stopped"})
			time.Sleep(200 * time.Millisecond)
			if len(probe.got()) != 1 {
				t.Fatalf("kept warming after the target was resident: %v", probe.got())
			}
			return
		}
		if len(probe.got()) > 1 {
			t.Fatalf("warm storm: %v", probe.got())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("default never warmed after the swap went idle")
}

func TestWarmTarget_SkipsAbsentAndDrainedCells(t *testing.T) {
	probe := &warmProbe{}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: time.Millisecond}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm, tick: 20 * time.Millisecond}
	s.startWarmLoopWithConfig(cfg)

	// Cell stale: skip silently.
	presenceOf(s, "heavy", AnnounceModel{ID: "challenger", State: "ready"})
	s.mu.Lock()
	s.presence["heavy"].Stale = true
	s.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	if len(probe.got()) != 0 {
		t.Fatal("warmed a stale cell")
	}
	var st *warmTargetState
	s.mu.Lock()
	for i := range s.warmStates {
		st = s.warmStates[i]
	}
	s.mu.Unlock()
	if st.State != "skipped" {
		t.Errorf("state = %s, want skipped", st.State)
	}
}

func TestWarmTarget_EmptyRestoreNeedsTwoEvals(t *testing.T) {
	// The live cold-start race: a swap mid-load reads as "nothing
	// resident" for up to one announce interval, and an empty eval
	// must NOT restore until the emptiness has persisted for the full
	// grace window (≥ one announce interval) — the bug seen live twice.
	probe := &warmProbe{}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: time.Hour}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	st := &warmTargetState{Cell: target.Cell, Model: target.Model, RestoreAfterIdleS: 3600}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm, emptyGrace: 100 * time.Millisecond}

	snap := CellSnapshot{}
	for range 3 {
		s.applyWarmEval(target, st, cfg, snap)
		if len(probe.got()) != 0 {
			t.Fatal("restored inside the grace window — the cold-start race")
		}
		time.Sleep(30 * time.Millisecond)
	}
	// Grace elapsed: the restore fires on the next eval.
	time.Sleep(80 * time.Millisecond)
	s.applyWarmEval(target, st, cfg, snap)
	if len(probe.got()) != 1 {
		t.Fatalf("no restore after grace: %v", probe.got())
	}
}

func TestWarmTarget_SwapAppearingMidGraceResets(t *testing.T) {
	// A swap reported resident before the grace elapses must cancel the
	// empty-restore entirely (the restore then belongs to the swap's
	// own idle window, not the empty grace).
	probe := &warmProbe{}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: 24 * time.Hour}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	st := &warmTargetState{Cell: target.Cell, Model: target.Model, RestoreAfterIdleS: 86400}
	cfg := warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm, emptyGrace: 100 * time.Millisecond}

	snap := CellSnapshot{}
	s.applyWarmEval(target, st, cfg, snap) // empty: grace starts
	s.applyWarmEval(target, st, cfg, CellSnapshot{Models: []ModelState{{ID: "challenger", State: "ready"}}})
	// Challenger ready: grace cancels; its own idle (unknown → floor
	// 1h) is far below the 24h restore window, so no fire either.
	time.Sleep(120 * time.Millisecond)
	s.applyWarmEval(target, st, cfg, snap) // empty again — grace RESTARTS from here
	if len(probe.got()) != 0 {
		t.Fatalf("fired with a restarted grace: %v", probe.got())
	}
}

func TestWarmTarget_NothingResidentRestores(t *testing.T) {
	probe := &warmProbe{}
	target := WarmTarget{Cell: "heavy", Model: "default-model", RestoreAfterIdle: time.Hour}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	s.startWarmLoopWithConfig(warmLoopConfig{targets: []WarmTarget{target}, frontURL: "http://front.test", warmFn: probe.warm, tick: 20 * time.Millisecond, emptyGrace: 50 * time.Millisecond})

	// Zero resident (the swap TTL'd out): restore regardless of the
	// window — the steady state is the target warm.
	presenceOf(s, "heavy")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(probe.got()) == 1 && probe.got()[0] == "default-model" {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("nothing-resident did not restore: %v", probe.got())
}

func TestCronParse(t *testing.T) {
	cases := map[string]struct {
		spec    string
		wantErr bool
	}{
		"30 6 * * *":     {},
		"*/15 * * * *":   {},
		"0 9-17 * * 1-5": {},
		"0 0 1 */3 *":    {},
		"0 0 * * 0,2,4":  {},
		"bad":            {wantErr: true},
		"1 2 3":          {wantErr: true},
		"61 * * * *":     {wantErr: true},
		"* * * * 7":      {wantErr: true},
		"0 0 32 1 *":     {wantErr: true},
		"0 0 * * 1-":     {wantErr: true},
		"0 0 * * */0":    {wantErr: true},
	}
	for spec, tc := range cases {
		_, err := parseCron(spec)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseCron(%q) err = %v, wantErr %v", spec, err, tc.wantErr)
		}
	}
}

func TestCronNextFire(t *testing.T) {
	loc := time.UTC
	at := func(y, mo, d, h, mi int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC)
	}
	cases := []struct {
		spec string
		from time.Time
		want time.Time
	}{
		{"30 6 * * *", at(2026, 8, 2, 6, 30), at(2026, 8, 3, 6, 30)}, // exactly at → next day (strictly after)
		{"30 6 * * *", at(2026, 8, 2, 6, 0), at(2026, 8, 2, 6, 30)},
		{"*/20 * * * *", at(2026, 8, 2, 6, 40), at(2026, 8, 2, 7, 0)},
		{"0 0 29 2 *", at(2026, 3, 1, 0, 0), at(2028, 2, 29, 0, 0)}, // 2027 not a leap year
		{"0 9 * * 1", at(2026, 8, 2, 10, 0), at(2026, 8, 3, 9, 0)},  // Sunday → Monday
	}
	for _, tc := range cases {
		spec, err := parseCron(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := spec.nextFire(tc.from, loc)
		if !ok {
			t.Fatalf("%s: no fire within a year", tc.spec)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s from %v: got %v, want %v", tc.spec, tc.from, got, tc.want)
		}
	}
}

func TestCronNextFireDST(t *testing.T) {
	// America/Halifax springs forward 2026-03-08 02:00 → 03:00: a 02:30
	// fire that day must SKIP (the wall minute never exists); the next
	// day fires normally. Fall-back day 2026-11-01 fires at the first
	// occurrence.
	loc, err := time.LoadLocation("America/Halifax")
	if err != nil {
		t.Skip("tz database unavailable")
	}
	spec, err := parseCron("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	got, ok := spec.nextFire(from, loc)
	if !ok {
		t.Fatal("no fire")
	}
	// First match after Mar 7 noon is Mar 9 02:30 (Mar 8 02:30 doesn't
	// exist) — assert the LOCAL wall time, not just a tick later.
	gotLocal := got.In(loc)
	if gotLocal.Day() != 9 || gotLocal.Hour() != 2 || gotLocal.Minute() != 30 {
		t.Errorf("DST gap: fired %v (local), want Mar 9 02:30", gotLocal)
	}
}

func TestScheduleGuardSkipsBusyAndLeased(t *testing.T) {
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	entry := WarmScheduleEntry{Cron: "* * * * *", Model: "default-model"}
	spec, _ := parseCron(entry.Cron)
	cellOf := func(string) string { return "heavy" }
	now := time.Now()

	// Due now; cell busy → skip with note, no fire.
	s.mu.Lock()
	s.inFlight["heavy"] = 2
	s.inFlightSeen["heavy"] = true
	s.schedStates = []*warmScheduleState{{Cron: entry.Cron, Model: entry.Model, NextFire: &now}}
	s.mu.Unlock()
	st := s.schedStates[0]
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)
	if len(probe.got()) != 0 {
		t.Fatal("fired into a busy cell — the eviction fight the guard exists to prevent")
	}
	if !strings.Contains(st.LastNote, "in-flight") {
		t.Errorf("note = %q, want the busy skip", st.LastNote)
	}

	// Idle but leased → still skipped (the first mechanical lease consumer).
	s.mu.Lock()
	s.inFlight["heavy"] = 0
	s.leases["heavy\x00default-model\x00batch"] = Lease{Cell: "heavy", Model: "default-model", Holder: "batch", ExpiresAt: now.Add(time.Hour)}
	s.mu.Unlock()
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now.Add(time.Minute))
	if len(probe.got()) != 0 {
		t.Fatal("fired into a leased cell")
	}
	if !strings.Contains(st.LastNote, "leases") {
		t.Errorf("note = %q, want the lease skip", st.LastNote)
	}

	// Clear → fires.
	s.mu.Lock()
	s.leases = map[string]Lease{}
	s.mu.Unlock()
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now.Add(2*time.Minute))
	if got := probe.got(); len(got) != 1 || got[0] != "default-model" {
		t.Fatalf("unguarded fire = %v, want [default-model]", got)
	}
	if st.LastFire == nil {
		t.Error("last_fire not recorded")
	}
	if st.NextFire == nil || !st.NextFire.After(now.Add(2*time.Minute)) {
		t.Errorf("next_fire not re-parked: %+v", st.NextFire)
	}
}

func TestFleetPageServed(t *testing.T) {
	cell := newFakeCell(t)
	_, ts, _ := newFleetdServer(t, []Cell{{Name: "front", URL: cell.srv.URL, Class: "always_on"}})
	resp, err := ts.Client().Get(ts.URL + "/ui/fleet")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("page: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"fleet", "/api/fleet/state", "/api/fleet/events", "tools/call"} {
		if !strings.Contains(s, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The default daemon (no fleet role) must NOT serve it.
	cell2 := newFakeCell(t)
	_, ts2, _ := newTestServer(t, cell2.srv.URL)
	resp2, err := ts2.Client().Get(ts2.URL + "/ui/fleet")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Errorf("page without fleetd role: HTTP %d, want 404", resp2.StatusCode)
	}
}
