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
		targets:  []WarmTarget{target},
		frontURL: "http://front.test",
		warmFn:   probe.warm,
		tick:     30 * time.Millisecond,
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
		// Keep the swap busy through the REAL parser: llama-swap's
		// double-encoded inflight frame is what resets the window in
		// production, so the window is driven end to end here.
		s.trackInFlight("heavy", inflightFrame(t, "challenger"))
		time.Sleep(40 * time.Millisecond)
	}

	// Now go quiet: the remove frame both drops the in-flight count and
	// stamps the completion edge, which is where the idle window starts.
	// After restore_after_idle of true idleness the default warms exactly
	// once, then holds (resident → holding).
	s.trackInFlight("heavy", inflightFrame(t))
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

func TestWarmTarget_SkipsStaleCells(t *testing.T) {
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
	// Read through the production accessor: it copies by VALUE under the
	// hub mutex. Capturing the *warmTargetState and reading it after the
	// unlock races the warm loop's own writes.
	if got := warmTargetStateOf(s, 0); got.State != "skipped" {
		t.Errorf("state = %s, want skipped", got.State)
	}
}

// warmTargetStateOf copies one warm-target state out of the server. Tests
// must never hold a loop-owned pointer past the lock — warmReport is the
// production copy-under-lock path.
func warmTargetStateOf(s *Server, i int) warmTargetState {
	rep := s.warmReport()
	if rep == nil || i >= len(rep.Targets) {
		return warmTargetState{}
	}
	return rep.Targets[i]
}

// schedStateOf is the same idiom for schedule states.
func schedStateOf(s *Server, i int) warmScheduleState {
	rep := s.warmReport()
	if rep == nil || i >= len(rep.Schedule) {
		return warmScheduleState{}
	}
	return rep.Schedule[i]
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
	// Challenger ready: grace cancels; its own idle (no activity evidence
	// → measured from fleetd start, ~0) is far below the 24h restore
	// window, so no fire either.
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
		"* * * * 7":      {}, // Vixie's second spelling of Sunday
		"bad":            {wantErr: true},
		"1 2 3":          {wantErr: true},
		"61 * * * *":     {wantErr: true},
		"* * * * 8":      {wantErr: true}, // the dow upper bound stays pinned
		"0 0 32 1 *":     {wantErr: true},
		"0 0 * * 1-":     {wantErr: true},
		"0 0 * * */0":    {wantErr: true},
		"* * * * sun":    {wantErr: true}, // names are deliberately unsupported
	}
	for spec, tc := range cases {
		_, err := parseCron(spec)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseCron(%q) err = %v, wantErr %v", spec, err, tc.wantErr)
		}
	}
}

func TestCronParseDow7IsSunday(t *testing.T) {
	// 7 must be FOLDED to 0 at parse time: time.Weekday() never returns
	// 7, so a spec normalised anywhere later would silently never match.
	spec, err := parseCron("0 9 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	if spec.dow[7] {
		t.Error("dow 7 survived parse; time.Weekday() never returns 7")
	}
	got, ok := spec.nextFire(time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), time.UTC) // Monday
	if !ok {
		t.Fatal("no fire")
	}
	if want := time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %v, want %v (next Sunday)", got, want)
	}
}

func TestCronNextFire(t *testing.T) {
	loc := time.UTC
	at := func(y, mo, d, h, mi int) time.Time {
		return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC)
	}
	// The dom/dow rule is the one interesting piece of cron semantics and
	// it was untested, which is how an AND implementation passed a gate
	// reported PASS. Vixie: if EITHER field starts with "*", both must
	// match; otherwise either matching fires. The star test is TEXTUAL —
	// "1-31" is not a star, "*/2" is.
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
		{"0 9 1 * *", at(2026, 8, 3, 10, 0), at(2026, 9, 1, 9, 0)},  // dow=* → dom decides

		// Both restricted ⇒ OR, and which side wins depends on the date.
		{"0 9 1 * 1", at(2026, 8, 2, 10, 0), at(2026, 8, 3, 9, 0)},  // dow wins: Mon Aug 3 beats Sep 1
		{"0 9 1 * 1", at(2026, 8, 15, 0, 0), at(2026, 8, 17, 9, 0)}, // dow wins again, mid-month
		{"0 9 1 * 1", at(2026, 8, 31, 10, 0), at(2026, 9, 1, 9, 0)}, // dom wins: Sep 1 is a Tuesday

		// An explicitly enumerated full range is NOT a star.
		{"0 9 1-31 * 1", at(2026, 8, 4, 0, 0), at(2026, 8, 4, 9, 0)}, // dom 1-31 matches every day → OR fires today
		{"0 9 1 * 0-6", at(2026, 8, 2, 10, 0), at(2026, 8, 3, 9, 0)}, // dow 0-6 matches every day → OR fires tomorrow

		// A stepped star IS a star (cronie/Vixie entry.c sets DOM_STAR
		// when the field's first character is '*', before parsing it).
		// Python's croniter disagrees here — it reads "*/2" as restricted
		// and would answer 2026-08-05 / 2026-08-04 for these two rows. The
		// rows above were cross-checked against croniter and agree; these
		// two follow the C implementation the config format comes from.
		{"0 9 */2 * 1", at(2026, 8, 3, 10, 0), at(2026, 8, 17, 9, 0)}, // AND: odd day that is also a Monday
		{"0 9 1 * */2", at(2026, 8, 2, 10, 0), at(2026, 9, 1, 9, 0)},  // AND: the 1st that is also Sun/Tue/Thu/Sat
	}
	for _, tc := range cases {
		spec, err := parseCron(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := spec.nextFire(tc.from, loc)
		if !ok {
			t.Fatalf("%s: no fire within 8 years", tc.spec)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s from %v: got %v, want %v", tc.spec, tc.from, got, tc.want)
		}
	}
}

func TestCronNextFireCenturyNonLeap(t *testing.T) {
	// 2100 is not a leap year, so Feb 29 has an 8-year gap around it: a
	// 4-year scan bound reports "impossible spec" for a perfectly valid
	// one, and the schedule silently never runs.
	spec, err := parseCron("0 0 29 2 *")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := spec.nextFire(time.Date(2096, 3, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if !ok {
		t.Fatal("no fire within the scan bound; 2104-02-29 is reachable")
	}
	if want := time.Date(2104, 2, 29, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCronNextFireDSTGapSkips(t *testing.T) {
	// America/Halifax springs forward 2026-03-08 02:00 → 03:00: a 02:30
	// fire that day must SKIP (the wall minute never exists); the next
	// day fires normally.
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

func TestCronNextFireDSTFallBackFiresTwice(t *testing.T) {
	// The honest behaviour, pinned rather than claimed away: America/
	// Halifax falls back 2026-11-01 02:00 ADT → 01:00 AST, so the wall
	// minute 01:30 exists TWICE in absolute time and the scan matches
	// both. The schedule's in-flight/lease guard makes a duplicate warm
	// harmless; a comment asserting first-occurrence-wins would not be.
	loc, err := time.LoadLocation("America/Halifax")
	if err != nil {
		t.Skip("tz database unavailable")
	}
	spec, err := parseCron("30 1 * * *")
	if err != nil {
		t.Fatal(err)
	}
	first, ok := spec.nextFire(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), loc)
	if !ok {
		t.Fatal("no fire")
	}
	if want := time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC); !first.Equal(want) {
		t.Fatalf("first occurrence: got %v, want %v", first, want)
	}
	second, ok := spec.nextFire(first, loc)
	if !ok {
		t.Fatal("no second fire")
	}
	if want := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC); !second.Equal(want) {
		t.Errorf("repeated hour: got %v, want %v (the same wall minute an hour later)", second, want)
	}
}

func TestScheduleGuardSkipsBusyAndLeased(t *testing.T) {
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	entry := WarmScheduleEntry{Cron: "* * * * *", Model: "default-model"}
	spec, _ := parseCron(entry.Cron)
	cellOf := func(string) (string, error) { return "heavy", nil }
	now := time.Now()

	// Due now; cell busy → skip with note, no fire.
	s.mu.Lock()
	s.inFlight["heavy"] = 2
	s.inFlightSeen["heavy"] = true
	s.schedStates = []*warmScheduleState{{Cron: entry.Cron, Model: entry.Model, NextFire: &now}}
	s.mu.Unlock()
	s.mu.Lock()
	st := s.schedStates[0] // the loop-owned pointer evalScheduleEntry mutates
	s.mu.Unlock()
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)
	if len(probe.got()) != 0 {
		t.Fatal("fired into a busy cell — the eviction fight the guard exists to prevent")
	}
	if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "in-flight") {
		t.Errorf("note = %q, want the busy skip", note)
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
	if note := schedStateOf(s, 0).LastNote; !strings.Contains(note, "leases") {
		t.Errorf("note = %q, want the lease skip", note)
	}

	// Clear → fires.
	s.mu.Lock()
	s.leases = map[string]Lease{}
	s.mu.Unlock()
	s.evalScheduleEntry(entry, spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now.Add(2*time.Minute))
	if got := probe.got(); len(got) != 1 || got[0] != "default-model" {
		t.Fatalf("unguarded fire = %v, want [default-model]", got)
	}
	final := schedStateOf(s, 0)
	if final.LastFire == nil {
		t.Error("last_fire not recorded")
	}
	if final.NextFire == nil || !final.NextFire.After(now.Add(2*time.Minute)) {
		t.Errorf("next_fire not re-parked: %+v", final.NextFire)
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
