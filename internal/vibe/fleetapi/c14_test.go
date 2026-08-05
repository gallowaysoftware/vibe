package fleetapi

// C14 coverage: the declared night. The property under test throughout
// is the phase's invariant line — a cron DECLARES the suspend and every
// observation can only DEFER it — so each test either drives the real
// state machine (evalSleepEntry, runWakeSequence) or asserts a guard
// rung by breaking exactly one thing about an otherwise-clean night.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	sleepCell    = "gpu-cell"
	suspendCron  = "30 23 * * *"
	wakeCronSpec = "15 7 * * *"
)

// suspendProbe records the suspend verb instead of taking a box down.
type suspendProbe struct {
	mu      sync.Mutex
	calls   []string
	reasons []string
	err     error
}

func (p *suspendProbe) suspend(_ context.Context, cell, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, cell)
	p.reasons = append(p.reasons, reason)
	return p.err
}

func (p *suspendProbe) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.calls...)
}

// wakeProbe records wake deliveries and, crucially, what the intent
// store looked like AT the moment the wake was sent — the clear-first
// ordering is only observable from inside the call.
type wakeProbe struct {
	mu           sync.Mutex
	calls        []string
	intentAtCall []string
	err          error
	s            *Server
}

func (p *wakeProbe) wake(_ context.Context, cell string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, cell)
	in := "none"
	if p.s != nil {
		p.s.mu.Lock()
		if got, ok := p.s.intents[cell]; ok {
			in = got.State + "/" + got.Reason
		}
		p.s.mu.Unlock()
	}
	p.intentAtCall = append(p.intentAtCall, in)
	return "packet → 255.255.255.255:9", p.err
}

func (p *wakeProbe) got() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.calls...), append([]string{}, p.intentAtCall...)
}

func sleepServer(t *testing.T, class string) *Server {
	t.Helper()
	dir := t.TempDir()
	s := New([]Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: sleepCell, URL: "http://127.0.0.1:1", Class: class},
	}, dir+"/hist.json", testDaemonInfo, Options{
		IntentPath: dir + "/intent.json",
		LeasePath:  dir + "/leases.json",
	})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s
}

// cleanNight is the state in which every rung passes and the declared
// suspend is CORRECT to fire: the cell announcing fresh and serving,
// in-flight reported zero, activity observed, nothing recently used.
func cleanNight(t *testing.T, s *Server) {
	t.Helper()
	presenceOf(s, sleepCell)
	s.trackInFlight(sleepCell, inflightFrame(t))
}

func sleepEntry() SleepScheduleEntry {
	return SleepScheduleEntry{
		Cell: sleepCell, Suspend: suspendCron, Wake: wakeCronSpec,
		QuietFor: 15 * time.Minute, MaxDefer: 2 * time.Hour, WakeGrace: 50 * time.Millisecond,
	}
}

func sleepCfg(s *Server, sp *suspendProbe, wp *wakeProbe, warm *warmProbe) sleepLoopConfig {
	if wp != nil {
		wp.s = s
	}
	cfg := sleepLoopConfig{
		loc: time.UTC, frontURL: "http://front.test",
		suspendFn: sp.suspend, tick: time.Minute, poll: 5 * time.Millisecond,
	}
	if wp != nil {
		cfg.wakeFn = wp.wake
	}
	if warm != nil {
		cfg.warmFn = warm.warm
	}
	return cfg
}

// armed builds a state whose next suspend is already due at `now` and
// whose next wake is hours away — the 23:30 minute, without waiting for
// it.
func armed(s *Server, now time.Time) *sleepScheduleState {
	sus := now.Add(-time.Second)
	wake := now.Add(8 * time.Hour)
	st := &sleepScheduleState{
		Cell: sleepCell, SuspendCron: suspendCron, WakeCron: wakeCronSpec,
		State: "watching", NextSuspend: &sus, NextWake: &wake,
	}
	return st
}

func sleepStateOf(s *Server, st *sleepScheduleState) sleepScheduleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSleepState(st)
}

func waitNotWaking(t *testing.T, s *Server, st *sleepScheduleState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		waking := st.waking
		s.mu.Unlock()
		if !waking {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("wake sequence never finished")
}

// ─── U1: the cron evaluator is shared, not forked ───────────────────────────

func TestSleepSchedule_UsesTheWarmScheduleCronEvaluator(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp := &suspendProbe{}
	now := time.Now()
	for _, spec := range []string{suspendCron, "0 3 */2 * *", "0 4 * * 7", "0 5 1-31 * 1"} {
		e := sleepEntry()
		e.Suspend = spec
		s.sleepStates = nil
		cfg := sleepCfg(s, sp, nil, nil)
		cfg.entries = []SleepScheduleEntry{e}
		s.startSleepLoopWithConfig(cfg)

		want, ok := mustCron(t, spec).nextFire(now, time.UTC)
		if !ok {
			t.Fatalf("%q never fires", spec)
		}
		got := s.sleepReport().Entries[0]
		if got.NextSuspend == nil || got.NextSuspend.Sub(want).Abs() > time.Minute {
			t.Fatalf("%q: next_suspend = %v, warmsched evaluator says %v", spec, got.NextSuspend, want)
		}
	}
}

func mustCron(t *testing.T, spec string) cronSpec {
	t.Helper()
	c, err := parseCron(spec)
	if err != nil {
		t.Fatalf("parseCron(%q): %v", spec, err)
	}
	return c
}

// TestSleepSchedule_NoSecondCronEvaluatorInThePackage is the structural
// half of U1: the rule is not "the numbers matched today", it is that
// there is exactly one evaluator to keep correct. warmsched.go's carries
// the Vixie dom/dow fix; a copy would not.
func TestSleepSchedule_NoSecondCronEvaluatorInThePackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(ent.Name())
		if err != nil {
			t.Fatal(err)
		}
		found += strings.Count(string(data), "func parseCron(")
	}
	if found != 1 {
		t.Fatalf("found %d cron parsers in fleetapi; there must be exactly one (warmsched.go's)", found)
	}
}

// ─── U4: the guard ladder, rung by rung ─────────────────────────────────────

func TestSuspendGuard_EveryRungDefersAndNamesItself(t *testing.T) {
	cases := []struct {
		name       string
		class      string
		cell       string
		setup      func(t *testing.T, s *Server)
		want       string
		structural bool
		absent     bool
	}{
		{
			name: "the front is never suspended", class: "opportunistic", cell: "front",
			setup: func(t *testing.T, s *Server) { cleanNight(t, s) },
			want:  "the data plane and the control plane ride it", structural: true,
		},
		{
			name: "unknown cell", class: "opportunistic", cell: "typo",
			setup: func(t *testing.T, s *Server) { cleanNight(t, s) },
			want:  "not in the registry", structural: true,
		},
		{
			name: "always_on never sleeps", class: "always_on",
			setup: func(t *testing.T, s *Server) { cleanNight(t, s) },
			want:  "only opportunistic cells sleep", structural: true,
		},
		{
			name: "roaming never sleeps", class: "roaming",
			setup: func(t *testing.T, s *Server) { cleanNight(t, s) },
			want:  "only opportunistic cells sleep", structural: true,
		},
		{
			name: "a declared drain means the operator took the box", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				cleanNight(t, s)
				if _, err := s.SetIntent(sleepCell, "drained", "gaming", "23:00"); err != nil {
					t.Fatal(err)
				}
			},
			want: "is drained (gaming)",
		},
		{
			name: "a C11 hold", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				cleanNight(t, s)
				if _, err := s.SetHold(sleepCell, "challenger", "evaluating", time.Hour); err != nil {
					t.Fatal(err)
				}
			},
			want: "held: challenger",
		},
		{
			name: "an absent cell is nothing to suspend", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {},
			want:  "already absent", absent: true,
		},
		{
			name: "an unreported in-flight count is not zero", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				presenceOf(s, sleepCell) // announcing, but no inflight frame ever
			},
			want: "in-flight unknown — unknown is not zero",
		},
		{
			name: "in-flight work", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				presenceOf(s, sleepCell)
				s.trackInFlight(sleepCell, inflightFrame(t, "challenger"))
			},
			want: "has 1 in-flight",
		},
		{
			name: "an active lease", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				cleanNight(t, s)
				if err := s.putLease(Lease{Cell: sleepCell, Model: "m", Holder: "batch", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			},
			want: "1 active leases",
		},
		{
			name: "an outstanding probe", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				presenceOf(s, sleepCell, AnnounceModel{ID: "qwen", State: "ready"})
				s.trackInFlight(sleepCell, inflightFrame(t))
				if err := s.QueueCommand(sleepCell, AnnounceCommand{Verb: "probe", Model: "qwen"}); err != nil {
					t.Fatal(err)
				}
			},
			want: "probe of qwen",
		},
		{
			name: "recent request activity (the operator at 23:29)", class: "opportunistic",
			setup: func(t *testing.T, s *Server) {
				presenceOf(s, sleepCell, AnnounceModel{ID: "qwen", State: "ready"})
				s.trackInFlight(sleepCell, inflightFrame(t, "qwen"))
				s.trackInFlight(sleepCell, inflightFrame(t))
			},
			want: "served a request",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sleepServer(t, tc.class)
			tc.setup(t, s)
			cell := tc.cell
			if cell == "" {
				cell = sleepCell
			}
			block, ok := s.suspendGuard(cell, 15*time.Minute)
			if ok {
				t.Fatalf("guard passed; the suspend would have fired")
			}
			if !strings.Contains(block.Why, tc.want) {
				t.Fatalf("reason = %q, want it to name %q", block.Why, tc.want)
			}
			if block.Structural != tc.structural {
				t.Fatalf("structural = %v, want %v (force must never bypass a structural refusal)", block.Structural, tc.structural)
			}
			if block.Absent != tc.absent {
				t.Fatalf("absent = %v, want %v", block.Absent, tc.absent)
			}
		})
	}
}

// TestSuspendGuard_NoActivityObservationChannelDefers pins C4/C5's rule
// on this path: "fleetd never looked" is not evidence of silence, and
// the most consequential verb in the plan may not run on it.
func TestSuspendGuard_NoActivityObservationChannelDefers(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	presenceOf(s, sleepCell)
	// An announce with no inflight frame reports no count either, so
	// reach the observation rung by handing it a reported zero without an
	// events stream.
	s.mu.Lock()
	s.inFlight[sleepCell] = 0
	s.inFlightSeen[sleepCell] = false
	s.mu.Unlock()
	block, ok := s.suspendGuard(sleepCell, time.Minute)
	if ok || !strings.Contains(block.Why, "unknown is not zero") {
		t.Fatalf("guard = (%q, %v), want the unreported-in-flight refusal", block.Why, ok)
	}
}

// ─── U5: the clean night ────────────────────────────────────────────────────

func TestSleepSchedule_CleanNightSuspendsOnceAndRecordsWhy(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	cleanNight(t, s)
	sp, e := &suspendProbe{}, sleepEntry()
	cfg := sleepCfg(s, sp, nil, nil)
	now := time.Now()
	st := armed(s, now)

	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now)
	if got := sp.got(); len(got) != 1 || got[0] != sleepCell {
		t.Fatalf("suspend calls = %v, want exactly one for %s", got, sleepCell)
	}
	got := sleepStateOf(s, st)
	if got.State != "asleep" || got.LastSuspend == nil {
		t.Fatalf("state = %+v, want asleep with a last_suspend", got)
	}
	in, ok := s.sleepIntent(sleepCell)
	if !ok || in.Reason != SleepIntentReason {
		t.Fatalf("intent = %+v (%v), want the reserved sleep reason", in, ok)
	}
	if in.ETA == "" {
		t.Fatal("intent has no eta: the operator reading DRAINED must see when the box comes back")
	}

	// A second evaluation the same minute must not re-suspend a sleeping
	// box (the RPC would land on a frozen machine).
	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now.Add(time.Minute))
	if got := sp.got(); len(got) != 1 {
		t.Fatalf("suspend calls = %v, want the sleeping cell left alone", got)
	}
}

// TestSleepSchedule_FailedSuspendRecordsNoIntent is C2's one-writer rule
// on this verb: a failed suspend must not leave the fleet claiming a box
// is asleep.
func TestSleepSchedule_FailedSuspendRecordsNoIntent(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	cleanNight(t, s)
	sp := &suspendProbe{err: context.DeadlineExceeded}
	e := sleepEntry()
	cfg := sleepCfg(s, sp, nil, nil)
	now := time.Now()
	st := armed(s, now)

	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now)
	if _, ok := s.sleepIntent(sleepCell); ok {
		t.Fatal("a failed suspend recorded intent")
	}
	got := sleepStateOf(s, st)
	if got.State != "failed" || !strings.Contains(got.Detail, "no intent recorded") {
		t.Fatalf("state = %+v, want a failed state naming the missing record", got)
	}
}

// ─── U6/U7: the operator at 23:29 ───────────────────────────────────────────

func TestSleepSchedule_DefersWhileTheOperatorIsWorkingThenFires(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	presenceOf(s, sleepCell, AnnounceModel{ID: "qwen", State: "ready"})
	s.trackInFlight(sleepCell, inflightFrame(t, "qwen"))
	s.trackInFlight(sleepCell, inflightFrame(t)) // request finished: stamped now
	sp, e := &suspendProbe{}, sleepEntry()
	cfg := sleepCfg(s, sp, nil, nil)
	now := time.Now()
	st := armed(s, now)

	for i := range 5 {
		s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now.Add(time.Duration(i)*time.Minute))
	}
	if got := sp.got(); len(got) != 0 {
		t.Fatalf("suspended a box the operator was using: %v", got)
	}
	got := sleepStateOf(s, st)
	if got.State != "deferred" || !strings.Contains(got.Detail, "quiet window") {
		t.Fatalf("state = %+v, want deferred naming the quiet window", got)
	}
	if got.DeferredSince == nil {
		t.Fatal("deferred_since is unset: a schedule blocked for hours must be visible as one")
	}

	// The session ends: the same pending suspend fires on the next
	// evaluation without a new cron minute.
	s.mu.Lock()
	s.modelActivity[sleepCell+"\x00qwen"] = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now.Add(6*time.Minute))
	if got := sp.got(); len(got) != 1 {
		t.Fatalf("suspend calls after the session ended = %v, want 1", got)
	}
}

// ─── U8/U9: abandonment ─────────────────────────────────────────────────────

func TestSleepSchedule_AbandonsAfterTheDeferWindow(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	presenceOf(s, sleepCell) // announcing, in-flight never reported: deferred
	sp, e := &suspendProbe{}, sleepEntry()
	e.MaxDefer = 30 * time.Minute
	cfg := sleepCfg(s, sp, nil, nil)
	now := time.Now()
	st := armed(s, now)

	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now)
	if got := sleepStateOf(s, st); got.State != "deferred" {
		t.Fatalf("state = %+v, want deferred", got)
	}
	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now.Add(31*time.Minute))
	got := sleepStateOf(s, st)
	if got.State != "skipped" || !strings.Contains(got.Detail, "abandoned after the defer window") {
		t.Fatalf("state = %+v, want an abandoned night naming the blocker", got)
	}
	if !strings.Contains(got.Detail, "unknown is not zero") {
		t.Fatalf("abandonment detail = %q, want the blocking reason carried into it", got.Detail)
	}
	if len(sp.got()) != 0 {
		t.Fatalf("suspended anyway: %v", sp.got())
	}
}

// TestSleepSchedule_NeverSuspendsAfterItsOwnWake is the rule that keeps
// a night's saving from becoming a fifteen-minute morning outage.
func TestSleepSchedule_NeverSuspendsAfterItsOwnWake(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	presenceOf(s, sleepCell) // deferred: in-flight never reported
	sp, wp, e := &suspendProbe{}, &wakeProbe{}, sleepEntry()
	e.MaxDefer = 24 * time.Hour // the defer window alone would not stop it
	cfg := sleepCfg(s, sp, wp, nil)
	now := time.Now()
	st := armed(s, now)
	wake := now.Add(10 * time.Minute)
	st.NextWake = &wake

	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now)
	if got := sleepStateOf(s, st); got.State != "deferred" {
		t.Fatalf("state = %+v, want deferred", got)
	}
	// The wake minute arrives with the suspend still pending.
	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, wake.Add(time.Second))
	waitNotWaking(t, s, st)
	if len(sp.got()) != 0 {
		t.Fatalf("the suspend fired at its own wake: %v", sp.got())
	}
}

// ─── U10/U11: the wake half ─────────────────────────────────────────────────

func TestSleepSchedule_WakeClearsItsOwnIntentBeforeSendingThePacket(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp, wp, warm := &suspendProbe{}, &wakeProbe{}, &warmProbe{}
	e := sleepEntry()
	e.Warm = []string{"qwen"}
	cfg := sleepCfg(s, sp, wp, warm)
	if _, err := s.SetIntent(sleepCell, "drained", SleepIntentReason, "07:15"); err != nil {
		t.Fatal(err)
	}
	st := &sleepScheduleState{Cell: sleepCell, SuspendCron: suspendCron, WakeCron: wakeCronSpec, asleep: true}
	// The box comes back one poll after the packet.
	go func() {
		time.Sleep(10 * time.Millisecond)
		presenceOf(s, sleepCell)
	}()
	s.wg.Add(1)
	s.mu.Lock()
	st.waking = true
	s.mu.Unlock()
	s.runWakeSequence(e, st, cfg)

	calls, intents := wp.got()
	if len(calls) != 1 {
		t.Fatalf("wake calls = %v, want one", calls)
	}
	if strings.Contains(intents[0], SleepIntentReason) {
		t.Fatalf("intent at the moment the packet was sent = %q, want the sleep record already cleared "+
			"(a box that comes back to a pending drained request runs its own drain verb)", intents[0])
	}
	if got := warm.got(); len(got) != 1 || got[0] != "qwen" {
		t.Fatalf("warmed %v, want the declared model", got)
	}
	got := sleepStateOf(s, st)
	if got.State != "watching" || got.LastWake == nil || got.WakeFailedSince != nil {
		t.Fatalf("state = %+v, want a clean wake", got)
	}
}

func TestSleepSchedule_WakeNeverClearsAnOperatorsOwnDrain(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp, wp := &suspendProbe{}, &wakeProbe{}
	e := sleepEntry()
	cfg := sleepCfg(s, sp, wp, nil)
	// The announce first, then the drain: a request NEWER than the cell's
	// echo is what a real `vibe cell drain` produces (C3's conflict rule
	// resolves the other order by design).
	presenceOf(s, sleepCell) // present: no packet, straight through
	if _, err := s.SetIntent(sleepCell, "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	st := &sleepScheduleState{Cell: sleepCell}
	s.wg.Add(1)
	s.runWakeSequence(e, st, cfg)

	s.mu.Lock()
	in := s.intents[sleepCell]
	s.mu.Unlock()
	if in.State != "drained" || in.Reason != "gaming" {
		t.Fatalf("intent after the wake = %+v, want the operator's own drain untouched", in)
	}
}

// ─── U12: a wake that fails is loud ─────────────────────────────────────────

func TestSleepSchedule_FailedWakeIsVisibleAndAlarms(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp, wp := &suspendProbe{}, &wakeProbe{}
	e := sleepEntry()
	cfg := sleepCfg(s, sp, wp, nil)
	if _, err := s.SetIntent(sleepCell, "drained", SleepIntentReason, "07:15"); err != nil {
		t.Fatal(err)
	}
	events := subscribeHub(s)
	st := &sleepScheduleState{Cell: sleepCell, WakeCron: wakeCronSpec}
	s.sleepStates = append(s.sleepStates, st)
	s.wg.Add(1)
	s.runWakeSequence(e, st, cfg) // the cell never comes back

	got := sleepStateOf(s, st)
	if got.State != "wake_failed" || got.WakeFailedSince == nil {
		t.Fatalf("state = %+v, want wake_failed with a since", got)
	}
	if !strings.Contains(got.Detail, "did not come back") {
		t.Fatalf("detail = %q, want it to name what happened", got.Detail)
	}
	if !waitForEvent(events, EventWakeFailed) {
		t.Fatal("no fleet.wakeFailed event: a wake that fails silently is a morning with no fleet")
	}

	// C9 reads the condition off the snapshot, never off loop state.
	conds := s.notifyConditions(StateSnapshot{Sleep: s.sleepReport()})
	found := false
	for _, c := range conds {
		if string(c.Kind) == "wake_failed" && c.Scope == sleepCell {
			found = true
		}
	}
	if !found {
		t.Fatalf("no wake_failed alarm condition in %+v", conds)
	}

	// And it clears when the box turns up.
	presenceOf(s, sleepCell)
	s.reconcileSleepState(e, st)
	if sleepStateOf(s, st).WakeFailedSince != nil {
		t.Fatal("wake_failed survived the cell's return")
	}
	if len(s.notifyConditions(StateSnapshot{Sleep: s.sleepReport()})) != 0 {
		t.Fatal("the alarm condition survived the cell's return")
	}
}

func subscribeHub(s *Server) chan Event {
	ch := make(chan Event, 32)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func waitForEvent(ch chan Event, want string) bool {
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// ─── U13: the ghost-drain trap ──────────────────────────────────────────────

// TestSleepSchedule_TheCellsOwnStampIsWhatPreventsTheGhostDrain shows
// both branches of the trap the cell-side intent stamp exists for. It is
// its own mutation test: delete the stamp in CellSuspend and the second
// subtest is what the fleet does every morning.
func TestSleepSchedule_TheCellsOwnStampIsWhatPreventsTheGhostDrain(t *testing.T) {
	suspendedAt := time.Now().UTC()

	t.Run("with the stamp the returning cell is handed nothing", func(t *testing.T) {
		s := sleepServer(t, "opportunistic")
		presenceOf(s, sleepCell)
		if _, err := s.SetIntent(sleepCell, "drained", SleepIntentReason, "07:15"); err != nil {
			t.Fatal(err)
		}
		// The box wakes and its FIRST heartbeat carries the intent it
		// stamped just before freezing — newer than fleetd's request.
		resp := s.recordAnnounce(&AnnounceRequest{
			V: AnnounceVersion, Cell: sleepCell, Seq: 1,
			Intent: &AnnounceIntent{State: "drained", Since: suspendedAt.Add(time.Second)},
		})
		if resp.DesiredIntent != nil {
			t.Fatalf("desired_intent = %+v, want none: the cell would run its own drain verb on a box that just woke", resp.DesiredIntent)
		}
	})

	t.Run("without it the request comes straight back as a drain", func(t *testing.T) {
		s := sleepServer(t, "opportunistic")
		presenceOf(s, sleepCell)
		if _, err := s.SetIntent(sleepCell, "drained", SleepIntentReason, "07:15"); err != nil {
			t.Fatal(err)
		}
		resp := s.recordAnnounce(&AnnounceRequest{
			V: AnnounceVersion, Cell: sleepCell, Seq: 1,
			Intent: &AnnounceIntent{State: "serving", Since: suspendedAt.Add(-time.Hour)},
		})
		if resp.DesiredIntent == nil || resp.DesiredIntent.State != "drained" {
			t.Fatalf("desired_intent = %+v, want the drain handed back — this subtest documents the failure the stamp prevents", resp.DesiredIntent)
		}
	})
}

// ─── review findings ────────────────────────────────────────────────────────

// TestSleepSchedule_TheSleepRecordIsNotAnUnackableRequest (REV-1). The
// record is dated when the suspend was ISSUED, not when the RPC
// answered, so the cell's own stamp — taken while it was still running —
// is newer and C6's complied-drain branch resolves it. Dating it later
// left every sleeping box showing "requested, awaiting ack" all night
// for an ack a frozen machine cannot give, and turned `vibe fleet
// doctor`'s intent hygiene yellow every morning.
func TestSleepSchedule_TheSleepRecordIsNotAnUnackableRequest(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	cleanNight(t, s)
	sp := &suspendProbe{}
	e := sleepEntry()
	cfg := sleepCfg(s, sp, nil, nil)
	now := time.Now()
	st := armed(s, now)
	s.evalSleepEntry(e, mustCron(t, e.Suspend), mustCron(t, e.Wake), st, cfg, now)

	// The cell's last heartbeat before freezing carries its own stamp.
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: sleepCell, Seq: 2,
		Intent: &AnnounceIntent{State: "drained", Since: time.Now().UTC()},
	})
	snap := s.Snapshot(context.Background())
	var row CellSnapshot
	for _, c := range snap.Cells {
		if c.Name == sleepCell {
			row = c
		}
	}
	if row.Intent == nil || row.Intent.Reason != SleepIntentReason || row.Intent.ETA == "" {
		t.Fatalf("intent = %+v, want the reason and eta preserved through the echo", row.Intent)
	}
	if row.IntentPending {
		t.Fatal("the sleeping cell shows an unacked request: a frozen box cannot ack, so this reads as residue forever")
	}

	rep := DoctorReport{}
	s.checkIntentHygiene(&rep, snap)
	if len(rep.Checks) != 1 || rep.Checks[0].Level != LevelOK {
		t.Fatalf("intent hygiene = %+v, want OK on a fleet doing exactly what it was configured to do", rep.Checks)
	}
}

// TestDoctor_ASleepingBoxIsNotIntentResidue (REV-2) covers the case the
// dating fix cannot reach: a cell that does not announce at all has no
// echo to resolve the request with, so the pending flag stands. It is
// still not residue.
func TestDoctor_ASleepingBoxIsNotIntentResidue(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	old := time.Now().Add(-6 * time.Hour).UTC()
	rep := DoctorReport{}
	s.checkIntentHygiene(&rep, StateSnapshot{Cells: []CellSnapshot{{
		Name: sleepCell, Class: "opportunistic", Display: DisplayOff, IntentPending: true,
		Intent: &Intent{State: "drained", Reason: SleepIntentReason, ETA: "07:15", Since: old},
	}}})
	if len(rep.Checks) != 1 || rep.Checks[0].Level != LevelOK {
		t.Fatalf("intent hygiene = %+v, want OK: a box asleep on a declared schedule cannot echo", rep.Checks)
	}

	// The control: the same shape with an ordinary drain reason IS residue.
	rep = DoctorReport{}
	s.checkIntentHygiene(&rep, StateSnapshot{Cells: []CellSnapshot{{
		Name: sleepCell, Class: "opportunistic", Display: DisplayOff, IntentPending: true,
		Intent: &Intent{State: "drained", Reason: "gaming", Since: old},
	}}})
	if len(rep.Checks) != 1 || rep.Checks[0].Level != LevelWarn {
		t.Fatalf("intent hygiene = %+v, want WARN for a request nothing has ever echoed", rep.Checks)
	}
}

// TestSleepSchedule_WakeDoesNotWarmADrainedCell (REV-3). The wake fires
// on its cron whether or not this schedule was why the box was away, so
// its warms take C4's drain guard — warming a cell the operator declared
// drained is the eviction fight the warm policy exists to avoid, and at
// 07:15 nobody is watching.
func TestSleepSchedule_WakeDoesNotWarmADrainedCell(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp, wp, warm := &suspendProbe{}, &wakeProbe{}, &warmProbe{}
	e := sleepEntry()
	e.Warm = []string{"qwen"}
	cfg := sleepCfg(s, sp, wp, warm)
	presenceOf(s, sleepCell)
	if _, err := s.SetIntent(sleepCell, "drained", "gaming", "10:00"); err != nil {
		t.Fatal(err)
	}
	st := &sleepScheduleState{Cell: sleepCell}
	s.wg.Add(1)
	s.runWakeSequence(e, st, cfg)

	if got := warm.got(); len(got) != 0 {
		t.Fatalf("warmed %v into a cell the operator declared drained", got)
	}
	if d := sleepStateOf(s, st).Detail; !strings.Contains(d, "warms skipped") {
		t.Fatalf("detail = %q, want the skip named", d)
	}
}

// ─── U3: configuration refusals ─────────────────────────────────────────────

func TestSleepSchedule_ABrokenWakeDisablesTheSuspendHalfToo(t *testing.T) {
	cases := []struct {
		name, suspend, wake, want string
	}{
		{"unparseable wake", suspendCron, "not a cron", "invalid wake cron"},
		{"wake that never fires", suspendCron, "0 0 30 2 *", "no fire time within 8 years"},
		{"unparseable suspend", "nope", wakeCronSpec, "invalid suspend cron"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sleepServer(t, "opportunistic")
			cleanNight(t, s)
			sp := &suspendProbe{}
			e := sleepEntry()
			e.Suspend, e.Wake = tc.suspend, tc.wake
			cfg := sleepCfg(s, sp, nil, nil)
			cfg.entries = []SleepScheduleEntry{e}
			s.startSleepLoopWithConfig(cfg)

			rep := s.sleepReport()
			if rep == nil || len(rep.Entries) != 1 {
				t.Fatalf("status = %+v, want the disabled entry still visible", rep)
			}
			got := rep.Entries[0]
			if got.State != "disabled" || !strings.Contains(got.Detail, tc.want) {
				t.Fatalf("entry = %+v, want disabled naming %q", got, tc.want)
			}
			if got.NextSuspend != nil {
				t.Fatal("a disabled entry still has a next suspend")
			}
		})
	}
}

// ─── U17: the status block ──────────────────────────────────────────────────

func TestSleepSchedule_StatusCarriesBothResolvedFires(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp := &suspendProbe{}
	cfg := sleepCfg(s, sp, nil, nil)
	cfg.entries = []SleepScheduleEntry{sleepEntry()}
	s.startSleepLoopWithConfig(cfg)

	snap := s.Snapshot(context.Background())
	if snap.Sleep == nil || len(snap.Sleep.Entries) != 1 {
		t.Fatalf("snapshot sleep block = %+v", snap.Sleep)
	}
	e := snap.Sleep.Entries[0]
	if e.NextSuspend == nil || e.NextWake == nil {
		t.Fatalf("entry = %+v, want both resolved fires (a wrong timezone must be visible before the night)", e)
	}
	if e.QuietForS != (15 * time.Minute).Seconds() {
		t.Fatalf("quiet_for_s = %v", e.QuietForS)
	}
}

// TestSleepSchedule_NoEntriesNoBlock keeps the status honest for the
// fleet that declares no nights at all.
func TestSleepSchedule_NoEntriesNoBlock(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	if snap := s.Snapshot(context.Background()); snap.Sleep != nil {
		t.Fatalf("sleep block present with nothing declared: %+v", snap.Sleep)
	}
	rep := DoctorReport{}
	s.checkSleep(&rep, StateSnapshot{})
	if len(rep.Checks) != 0 {
		t.Fatalf("doctor emitted %d sleep checks with nothing declared", len(rep.Checks))
	}
}

// TestDoctor_SleepChecksNameWhatTheyProve is C13's naming rule applied
// here: a failed wake is a FAIL that says the box did not come back, and
// a schedule that is merely deferring is not a fault.
func TestDoctor_SleepChecks(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	now := time.Now().UTC()
	rep := DoctorReport{}
	s.checkSleep(&rep, StateSnapshot{Sleep: &sleepStatus{Entries: []sleepScheduleState{
		{Cell: sleepCell, SuspendCron: suspendCron, WakeCron: wakeCronSpec, State: "deferred", Detail: "cell busy", NextSuspend: &now, NextWake: &now},
		{Cell: "other", State: "disabled", Detail: "invalid wake cron: bad"},
		{Cell: "third", WakeCron: wakeCronSpec, State: "wake_failed", Detail: "cell did not come back within 10m", WakeFailedSince: &now},
	}}})
	if len(rep.Checks) != 3 {
		t.Fatalf("checks = %+v", rep.Checks)
	}
	byLevel := map[Level]DoctorCheck{}
	for _, c := range rep.Checks {
		byLevel[c.Level] = c
	}
	if byLevel[LevelOK].Cell != sleepCell {
		t.Fatalf("a deferring schedule is not a fault: %+v", rep.Checks)
	}
	if byLevel[LevelWarn].Cell != "other" || byLevel[LevelFail].Cell != "third" {
		t.Fatalf("levels = %+v", rep.Checks)
	}
	if byLevel[LevelFail].ID != "sleep.wake" {
		t.Fatalf("failed-wake check id = %q, want sleep.wake", byLevel[LevelFail].ID)
	}
}
