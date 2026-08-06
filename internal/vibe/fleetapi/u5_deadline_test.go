package fleetapi

// U5 coverage: the warm / sleep / wake loop DEADLINES.
//
// Four constants bound the four calls this package makes that can hang
// on a far side that accepted the connection and then went quiet —
// warmTimeout (10m), suspendTimeout (90s), sleepReturnGrace (2m) and
// wakeCmdTimeout (30s). Until this file none of them was exercised: the
// package's entire vocabulary for "unreachable" is an immediate
// ECONNREFUSED (http://127.0.0.1:1) or a DNS failure, and both of those
// return in microseconds. A connection that is ACCEPTED and never
// answered is the failure a deadline exists for, and it is the one no
// test had ever produced.
//
// Two shapes are used, and the distinction is what keeps each test's
// name honest:
//
//   - a DIALLED bound (the warmLoopConfig / sleepLoopConfig seams, and
//     the wakeCmdTimeout var) proves the deadline actually FIRES and
//     unblocks the caller, against a far side that really does hang —
//     swaptest.WithChatDelay for the warm, u5HangingWakeCmd for the wake;
//   - a CAPTURED deadline (ctx.Deadline() read from inside the injected
//     verb) proves the production constant is the one wired in, without
//     waiting ten minutes for it. A test may only claim what it read.
//
// sleepReturnGrace needs neither seam: it reads time.Since on a settable
// struct field, so the tests rewind that field under s.mu — c15_test.go's
// swapAuth idiom, the cheapest pattern in the repo.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// ─── probes ─────────────────────────────────────────────────────────────────

// u5Deadline is what an injected verb SAW: whether it was given a
// deadline at all, when, and whether the context was already dead on
// arrival — the failure a zero-valued seam would produce.
type u5Deadline struct {
	calls    int
	deadline time.Time
	set      bool
	ctxErr   error
}

// u5DeadlineProbe records the context each injected verb was handed and
// returns success immediately. It satisfies both the warmFn and the
// SuspendFunc shapes, because the property under test is the same one.
type u5DeadlineProbe struct {
	mu  sync.Mutex
	got u5Deadline
}

func (p *u5DeadlineProbe) record(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got.calls++
	p.got.deadline, p.got.set = ctx.Deadline()
	p.got.ctxErr = ctx.Err()
}

func (p *u5DeadlineProbe) warm(ctx context.Context, _, _ string) error {
	p.record(ctx)
	return nil
}

func (p *u5DeadlineProbe) suspend(ctx context.Context, _, _ string) error {
	p.record(ctx)
	return nil
}

func (p *u5DeadlineProbe) observed() u5Deadline {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.got
}

// u5HangProbe is the far side that accepted the work and never answers:
// it blocks until its context dies and reports why. It is the only thing
// in this package that makes a deadline observable.
type u5HangProbe struct {
	once    sync.Once
	entered chan struct{}
}

func u5NewHangProbe() *u5HangProbe {
	return &u5HangProbe{entered: make(chan struct{})}
}

func (p *u5HangProbe) enter() {
	p.once.Do(func() { close(p.entered) })
}

func (p *u5HangProbe) suspend(ctx context.Context, _, _ string) error {
	p.enter()
	<-ctx.Done()
	return ctx.Err()
}

// waitEntered blocks until the verb is actually in flight, so a test
// never races Close() against a goroutine that has not started.
func (p *u5HangProbe) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the injected verb was never called")
	}
}

// u5RewindAsleep moves a sleep entry's asleepSince back by d, under the
// mutex that owns it. The state-rewind idiom from c15_test.go's
// swapAuth-retry test: the field is settable, so the two-minute grace
// costs a struct write rather than two minutes.
func u5RewindAsleep(s *Server, st *sleepScheduleState, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.asleepSince = time.Now().Add(-d)
}

func u5Asleep(s *Server, st *sleepScheduleState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return st.asleep
}

func u5WarmState(s *Server, st *warmTargetState) warmTargetState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *st
}

// u5HangingWakeCmd is a fallback command that never returns on its own,
// so the ONLY thing that can end it is a deadline or a cancellation.
//
// Eight seconds rather than thirty purely for the mutation registry's
// runtime: an unmutated run never waits for it (the bound fires in
// ~300ms), but a run with the bound REMOVED waits the whole thing, and
// the registry pays that cost on every CI run. It only has to outlast
// the five-second upper bound the two wake tests assert, with enough
// margin that a loaded box cannot make the two readings overlap.
const u5HangingWakeCmd = "sleep 8"

// u5SetWakeCmdTimeout dials the wake fallback's bound down for one test
// and restores it. Safe because nothing in this package runs tests in
// parallel and the var is read only on the Cmd branch of SendWake, which
// exactly one other test reaches (c2's `touch`, synchronously).
func u5SetWakeCmdTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := wakeCmdTimeout
	wakeCmdTimeout = d
	t.Cleanup(func() { wakeCmdTimeout = prev })
}

// ─── warmTimeout: the warm-target restore ───────────────────────────────────

// TestU5_WarmRestoreUnblocksWhenTheFrontAcceptsAndNeverAnswers is the
// case ECONNREFUSED cannot produce. The front is a real llama-swap
// double that holds POST /v1/chat/completions open for thirty seconds;
// only the warm's own deadline ends the call. What the deadline must
// deliver is not merely "returns": a timed-out warm is a DELIVERY
// failure, so it falls back to the announce piggyback, and it must not
// record a restore that never happened.
func TestU5_WarmRestoreUnblocksWhenTheFrontAcceptsAndNeverAnswers(t *testing.T) {
	front := swaptest.NewCell(t,
		swaptest.WithModels(swaptest.Model{ID: "default-model", State: "ready"}),
		swaptest.WithChatDelay(30*time.Second))
	s := newWarmServer(t, []Cell{
		{Name: fleetcfg.FrontCell, URL: front.URL(), Class: "always_on"},
		{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"},
	})
	// The cell announces the model, so the piggyback queue is genuinely
	// available and the fallback is exercised rather than skipped.
	presenceOf(s, "heavy", AnnounceModel{ID: "default-model", State: "stopped"})

	target := WarmTarget{Cell: "heavy", Model: "default-model"}
	st := &warmTargetState{Cell: target.Cell, Model: target.Model}
	cfg := warmLoopConfig{frontURL: front.URL(), warmFn: s.warmViaFront, warmTimeout: 300 * time.Millisecond}

	start := time.Now()
	s.restore(target, st, cfg, "nothing resident")
	elapsed := time.Since(start)

	require.Less(t, elapsed, 5*time.Second,
		"restore never returned from a front that accepted the connection and went quiet — warmTimeout is not applied")
	require.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
		"restore returned before its own deadline: the context was dead on arrival, not bounded")

	got := u5WarmState(s, st)
	require.Equal(t, "waiting", got.State)
	require.Contains(t, got.Detail, "context deadline exceeded",
		"the timeout is not named in the status an operator reads")
	require.Contains(t, got.Detail, "queued for heavy",
		"a warm that timed out is a delivery failure and must fall back to the announce piggyback, not be refused")
	require.Nil(t, got.LastRestore,
		"a warm that timed out recorded a restore: last_restore must only ever mean a warm that actually landed")
}

// TestU5_WarmRestoreDefaultsToTheProductionWarmTimeout pins the other
// half of the new seam: an unset warmTimeout is the ten-minute constant,
// NOT context.WithTimeout(bg, 0). The zero-value bug is the one a seam
// like this actually introduces, and it would convert every production
// warm into an instant deadline-exceeded while the suite stayed green.
func TestU5_WarmRestoreDefaultsToTheProductionWarmTimeout(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	probe := &u5DeadlineProbe{}
	st := &warmTargetState{Cell: "heavy", Model: "default-model"}

	before := time.Now()
	s.restore(WarmTarget{Cell: "heavy", Model: "default-model"}, st,
		warmLoopConfig{frontURL: "http://front.test", warmFn: probe.warm}, "nothing resident")

	got := probe.observed()
	require.Equal(t, 1, got.calls)
	require.True(t, got.set, "the warm ran with no deadline at all")
	require.NoError(t, got.ctxErr, "the warm was handed an already-expired context")
	require.WithinDuration(t, before.Add(warmTimeout), got.deadline, 5*time.Second,
		"an unset warmTimeout seam is not the production constant")
}

// TestU5_WarmScheduleCarriesTheSameWarmTimeout covers the second
// consumer of the constant (warmsched.go). evalScheduleEntry's signature
// is fixed by eight existing call sites, so the bound is read out of the
// context the schedule hands its warm rather than dialled down.
func TestU5_WarmScheduleCarriesTheSameWarmTimeout(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	// A clean minute: the cell announces and reports zero in-flight, so
	// every guard rung passes and the warm actually fires.
	presenceOf(s, "heavy", AnnounceModel{ID: "default-model", State: "stopped"})
	s.trackInFlight("heavy", inflightFrame(t))

	spec := mustCron(t, "* * * * *")
	now := time.Now()
	due := now.Add(-time.Second)
	st := &warmScheduleState{Cron: "* * * * *", Model: "default-model", NextFire: &due}
	probe := &u5DeadlineProbe{}
	cellOf := func(string) (string, error) { return "heavy", nil }

	before := time.Now()
	s.evalScheduleEntry(WarmScheduleEntry{Cron: "* * * * *", Model: "default-model"},
		spec, st, cellOf, time.UTC, probe.warm, "http://front.test", now)

	require.Equal(t, "warmed", st.LastNote, "the schedule never fired; the deadline below would be unproven")
	got := probe.observed()
	require.True(t, got.set, "the scheduled warm ran with no deadline at all")
	require.NoError(t, got.ctxErr)
	require.WithinDuration(t, before.Add(warmTimeout), got.deadline, 5*time.Second,
		"the warm schedule does not carry the same bound as the warm target")
}

// ─── suspendTimeout: the most consequential verb ────────────────────────────

// TestU5_ScheduledSuspendUnblocksAtTheSuspendTimeout drives the real
// state machine against a suspend RPC that accepts and never answers —
// a cell whose daemon is up, reachable, and wedged. The deadline must
// end it, and everything downstream must treat it as the failure it is:
// no intent recorded, nothing marked asleep, no last_suspend. A box
// fleetd did not demonstrably put to sleep is a box fleetd must not
// claim to have put to sleep.
func TestU5_ScheduledSuspendUnblocksAtTheSuspendTimeout(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	cleanNight(t, s)
	hang := u5NewHangProbe()
	cfg := sleepLoopConfig{
		loc: time.UTC, frontURL: "http://front.test", suspendFn: hang.suspend,
		tick: time.Minute, poll: 5 * time.Millisecond, suspendTimeout: 300 * time.Millisecond,
	}
	st := &sleepScheduleState{Cell: sleepCell, State: "watching", pending: true}

	start := time.Now()
	s.trySuspend(sleepEntry(), st, cfg)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 5*time.Second,
		"the schedule blocked forever on a wedged suspend RPC — suspendTimeout is not applied")
	require.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
		"the suspend returned before its own deadline: the context was dead on arrival, not bounded")

	got := sleepStateOf(s, st)
	require.Equal(t, "failed", got.State)
	require.Contains(t, got.Detail, "suspend failed")
	require.Contains(t, got.Detail, "context deadline exceeded",
		"the timeout is not named in the status an operator reads")
	require.Contains(t, got.Detail, "no intent recorded")
	require.Nil(t, got.LastSuspend, "a suspend that timed out was recorded as one that landed")

	s.mu.Lock()
	_, recorded := s.intents[sleepCell]
	asleep, pending := st.asleep, st.pending
	s.mu.Unlock()
	require.False(t, recorded,
		"a timed-out suspend wrote a drained intent: the box may still be serving and would now read as asleep")
	require.False(t, asleep, "a timed-out suspend marked the entry asleep")
	// Still pending, deliberately: the declared action is abandoned by
	// the defer window in evalSleepEntry, never by one wedged RPC. A
	// timeout that cleared `pending` would silently drop a declared
	// suspend for the whole night on one slow cell.
	require.True(t, pending, "a timed-out suspend abandoned the declared night instead of leaving it to the defer window")
}

// TestU5_ScheduledSuspendDefaultsToTheProductionSuspendTimeout is the
// zero-value half of the seam, as for the warm.
func TestU5_ScheduledSuspendDefaultsToTheProductionSuspendTimeout(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	cleanNight(t, s)
	probe := &u5DeadlineProbe{}
	cfg := sleepLoopConfig{
		loc: time.UTC, frontURL: "http://front.test", suspendFn: probe.suspend,
		tick: time.Minute, poll: 5 * time.Millisecond,
	}
	st := &sleepScheduleState{Cell: sleepCell, State: "watching", pending: true}

	before := time.Now()
	s.trySuspend(sleepEntry(), st, cfg)

	got := probe.observed()
	require.Equal(t, 1, got.calls, "the guard ladder blocked the suspend; the deadline below would be unproven")
	require.True(t, got.set, "the suspend RPC ran with no deadline at all")
	require.NoError(t, got.ctxErr, "the suspend was handed an already-expired context")
	require.WithinDuration(t, before.Add(suspendTimeout), got.deadline, 5*time.Second,
		"an unset suspendTimeout seam is not the production constant")
}

// TestU5_AWedgedSuspendDoesNotHoldClose pins the linkage half: the
// suspend's context comes from warmCtx, so it dies with the server.
// Without it a fleetd asked to shut down while a suspend RPC is wedged
// waits the full ninety seconds — on the one goroutine that is holding a
// box's power state open.
func TestU5_AWedgedSuspendDoesNotHoldClose(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	cleanNight(t, s)
	hang := u5NewHangProbe()
	cfg := sleepLoopConfig{
		loc: time.UTC, frontURL: "http://front.test", suspendFn: hang.suspend,
		tick: time.Minute, poll: 5 * time.Millisecond, suspendTimeout: time.Hour,
	}
	st := &sleepScheduleState{Cell: sleepCell, State: "watching", pending: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.trySuspend(sleepEntry(), st, cfg)
	}()
	hang.waitEntered(t)
	s.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the suspend outlived Close(): its context is not linked to s.done, so shutdown waits the full suspendTimeout")
	}
	require.Contains(t, sleepStateOf(s, st).Detail, "suspend failed",
		"a suspend cancelled by shutdown must still report as a failure — it did not take the box down")
}

// ─── sleepReturnGrace: asleep vs a heartbeat that is merely stale ───────────

// TestU5_TheAsleepFlagSurvivesUntilTheReturnGrace. Presence is
// heartbeat-stale by construction: a box that suspended five seconds ago
// still has a fresh announce on file for most of a minute, so the
// instant after a successful suspend the cell reads as PRESENT. Clearing
// `asleep` on that reading is how a schedule forgets it put the box down
// — and the next minute's evaluation then re-fires the suspend it
// already performed. Both sides of the two-minute boundary are asserted,
// which is what pins the constant's value rather than its existence.
func TestU5_TheAsleepFlagSurvivesUntilTheReturnGrace(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	e := sleepEntry()
	presenceOf(s, sleepCell) // fresh heartbeat: the box "looks" back
	st := &sleepScheduleState{Cell: sleepCell, State: "asleep", asleep: true, asleepSince: time.Now()}

	s.reconcileSleepState(e, st)
	require.True(t, u5Asleep(s, st),
		"the asleep flag was cleared by the stale heartbeat of the box it had just suspended")

	u5RewindAsleep(s, st, sleepReturnGrace-2*time.Second)
	s.reconcileSleepState(e, st)
	require.True(t, u5Asleep(s, st), "the return grace ends early: presence inside it is not evidence the box is back")
	require.Equal(t, "asleep", sleepStateOf(s, st).State)

	u5RewindAsleep(s, st, sleepReturnGrace+2*time.Second)
	s.reconcileSleepState(e, st)
	require.False(t, u5Asleep(s, st), "the box is demonstrably back past the grace and the entry still calls it asleep")
	got := sleepStateOf(s, st)
	require.Equal(t, "watching", got.State)
	require.Contains(t, got.Detail, "sleep record is gone")
}

// TestU5_TheReturnGraceOutlastsTheWindowWhereADeadBoxStillReadsFresh.
// The other two grace tests reference sleepReturnGrace by NAME, so they
// prove the constant is the one wired in and prove nothing about its
// value — shrink it to two seconds and they stay green. This one pins
// the value, and it pins it against a real quantity rather than a magic
// number: cellPresent reads an announce as present until
// staleAfter(interval) has elapsed (50s at the default cadence), so a
// box suspended at T still reads PRESENT for the next fifty seconds. A
// return grace inside that window clears `asleep` for a box that is
// genuinely down, which is the exact reading the grace exists to
// survive.
func TestU5_TheReturnGraceOutlastsTheWindowWhereADeadBoxStillReadsFresh(t *testing.T) {
	stale := staleAfter(DefaultAnnounceIntervalS)
	require.Greater(t, sleepReturnGrace, stale,
		"the return grace is shorter than the window in which a suspended box's last announce still reads fresh: "+
			"the entry would forget it suspended the box while the only evidence of presence is a heartbeat from before the suspend")

	// The same fact, driven: a heartbeat as old as the whole stale window
	// is still not evidence the box came back.
	s := sleepServer(t, "opportunistic")
	presenceOf(s, sleepCell)
	st := &sleepScheduleState{Cell: sleepCell, State: "asleep", asleep: true}
	u5RewindAsleep(s, st, stale)
	s.reconcileSleepState(sleepEntry(), st)
	require.True(t, u5Asleep(s, st),
		"the entry woke up its own record inside the stale window; the announce it trusted predates the suspend")
}

// TestU5_TheReturnGraceNeedsBothHalvesOfTheEvidence. The grace expiring
// is a necessary condition, never a sufficient one. Absent evidence is
// not a healthy value: a cell nobody can see has not "come back", and a
// standing sleep record is fleetd's own declaration that it put the box
// down. Either one alone must keep the entry asleep however long ago the
// suspend was.
func TestU5_TheReturnGraceNeedsBothHalvesOfTheEvidence(t *testing.T) {
	t.Run("the sleep record still stands", func(t *testing.T) {
		s := sleepServer(t, "opportunistic")
		e := sleepEntry()
		presenceOf(s, sleepCell)
		if _, err := s.SetIntent(sleepCell, "drained", SleepIntentReason, "07:15"); err != nil {
			t.Fatal(err)
		}
		st := &sleepScheduleState{Cell: sleepCell, State: "asleep", asleep: true}
		u5RewindAsleep(s, st, 10*sleepReturnGrace)
		s.reconcileSleepState(e, st)
		require.True(t, u5Asleep(s, st),
			"the entry forgot it suspended the box while its own sleep record was still on file")
		require.Equal(t, "asleep", sleepStateOf(s, st).State)
	})

	t.Run("the cell is not actually back", func(t *testing.T) {
		s := sleepServer(t, "opportunistic")
		e := sleepEntry()
		// No announce, no stream: the box is exactly as absent as a
		// suspended box should be.
		st := &sleepScheduleState{Cell: sleepCell, State: "asleep", asleep: true}
		u5RewindAsleep(s, st, 10*sleepReturnGrace)
		s.reconcileSleepState(e, st)
		require.True(t, u5Asleep(s, st),
			"an absent cell was reconciled to awake: absence of evidence became evidence of return")
		require.Equal(t, "asleep", sleepStateOf(s, st).State)
	})
}

// ─── wakeCmdTimeout: the fallback command ───────────────────────────────────

// TestU5_TheWakeFallbackCommandIsKilledAtTheWakeCmdTimeout. The fallback
// is an operator-supplied shell command (an IPMI call, an ssh to a
// switch) run where fleetd cannot reach L2 broadcast. A command that
// hangs — the far side accepted the TCP connection and stopped talking —
// is the normal failure of exactly those tools, and without the bound it
// pins the wake goroutine forever.
func TestU5_TheWakeFallbackCommandIsKilledAtTheWakeCmdTimeout(t *testing.T) {
	u5SetWakeCmdTimeout(t, 300*time.Millisecond)
	s := newWarmServer(t, []Cell{{
		Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic",
		Wake: &WakeSpec{Cmd: u5HangingWakeCmd},
	}})

	start := time.Now()
	resp, err := s.SendWake(context.Background(), "gpu-cell")
	elapsed := time.Since(start)

	require.Error(t, err, "a wake command that never returned was reported as a delivered wake")
	require.Nil(t, resp)
	require.Less(t, elapsed, 5*time.Second, "the wake fallback ran unbounded — wakeCmdTimeout is not applied")
	require.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
		"the command was killed before its own deadline: the context was dead on arrival, not bounded")
	require.Contains(t, err.Error(), "wake command failed")
}

// TestU5_TheWakeFallbackCommandDiesWithItsCallersContext leaves
// wakeCmdTimeout at its production thirty seconds, so the ONLY thing
// that can end this command is the caller's context. That linkage is
// what connects a wake to Close(): the sleep schedule calls SendWake
// with a warmCtx, and a fallback bounded by context.Background() would
// hold shutdown for half a minute per wedged cell.
func TestU5_TheWakeFallbackCommandDiesWithItsCallersContext(t *testing.T) {
	s := newWarmServer(t, []Cell{{
		Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic",
		Wake: &WakeSpec{Cmd: u5HangingWakeCmd},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(300*time.Millisecond, cancel)

	start := time.Now()
	_, err := s.SendWake(ctx, "gpu-cell")
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, 5*time.Second,
		"SendWake ignored its caller's context: the fallback command is bounded only by its own 30s timeout")
}

// TestU5_PostWakeWarmsCarryTheGraceAndTheWarmTimeout. The wake sequence
// builds ONE context for the whole run — the return grace plus a warm —
// and the warms at the far end of it are the third producer of the same
// chat completion, firing at 07:15 with nobody watching. An unbounded
// one holds an s.wg goroutine past the morning it belongs to.
func TestU5_PostWakeWarmsCarryTheGraceAndTheWarmTimeout(t *testing.T) {
	s := sleepServer(t, "opportunistic")
	sp, wp := &suspendProbe{}, &wakeProbe{}
	probe := &u5DeadlineProbe{}
	e := sleepEntry()
	e.Warm = []string{"qwen"}
	cfg := sleepCfg(s, sp, wp, nil)
	cfg.warmFn = probe.warm
	presenceOf(s, sleepCell) // already up: no packet, straight through to the warms

	st := &sleepScheduleState{Cell: sleepCell}
	before := time.Now()
	s.wg.Add(1)
	s.runWakeSequence(e, st, cfg)

	got := probe.observed()
	require.Equal(t, 1, got.calls, "the declared warm never ran; the deadline below would be unproven")
	require.True(t, got.set, "the post-wake warm ran with no deadline at all")
	require.NoError(t, got.ctxErr, "the post-wake warm was handed an already-expired context")
	require.WithinDuration(t, before.Add(e.WakeGrace+warmTimeout), got.deadline, 5*time.Second,
		"the wake sequence does not bound its warms by the grace plus one warm")
}
