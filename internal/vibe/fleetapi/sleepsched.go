package fleetapi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// Sleep schedules (fleet-control C14): warm_schedule's dual. A cron
// entry DECLARES "suspend this cell at 23:30"; in-flight requests,
// active leases, a C11 hold, a declared drain and recent activity DEFER
// that declared action. Nothing here ever infers "the box looks idle,
// suspend it" — the rejected direction (design §9's GPU-idle heuristic)
// stays rejected, and the test for every line in this file is that
// removing a guard could only ever make the suspend happen SOONER at a
// minute cron already named.
//
// The cron evaluator is warmsched.go's, unforked: parseCron and
// cronSpec.nextFire carry the Vixie dom/dow rule (textual star flags)
// and the eight-year scan, and a second copy in the same package is how
// one of them silently rots.

// SleepIntentReason is the reserved intent reason a scheduled suspend
// records. It is also the MATCH the wake half uses before clearing:
// an operator's own `vibe cell drain --reason gaming` must survive a
// 07:15 wake untouched, or the schedule would silently resume a box its
// owner declared they were using.
const SleepIntentReason = "asleep per sleep_schedule"

// Idle-proof statuses carried on CellSuspendResponse. They live here
// with the other cross-package cell-verb strings (DrainWait*) because
// the daemon writes them and the CLI and MCP facade render them.
const (
	SuspendIdleNotRequired = "not_required"
	SuspendIdleVerified    = "verified_idle"
)

// Sleep-schedule defaults and bounds. Every one of them is a clamp
// rather than a rejection (C4/C8's precedent): a too-eager value is a
// working policy visible in fleet_status, a skipped entry is silently
// absent.
const (
	DefaultSleepQuietFor  = 15 * time.Minute
	MinSleepQuietFor      = 5 * time.Minute
	DefaultSleepMaxDefer  = 2 * time.Hour
	MinSleepMaxDefer      = time.Minute
	MaxSleepMaxDefer      = 6 * time.Hour
	DefaultSleepWakeGrace = 10 * time.Minute
	MinSleepWakeGrace     = time.Minute
	MaxSleepWakeGrace     = time.Hour
)

// sleepReturnGrace is how long after a successful suspend the entry
// still treats the cell as asleep even though presence says otherwise.
// Presence is heartbeat-stale by construction (C4's grace, same
// reason): a box that froze five seconds ago still has a fresh
// heartbeat on file for most of a minute.
const sleepReturnGrace = 2 * time.Minute

// SleepScheduleEntry is one cell's declared night, mirrored from the
// daemon config after validation and clamping.
type SleepScheduleEntry struct {
	Cell      string
	Suspend   string
	Wake      string
	QuietFor  time.Duration
	MaxDefer  time.Duration
	WakeGrace time.Duration
	Warm      []string
}

// sleepScheduleState is one entry's status surface. Skips are
// first-class: a nightly schedule that silently never fires costs 365
// nights before anyone notices.
type sleepScheduleState struct {
	Cell        string     `json:"cell"`
	SuspendCron string     `json:"suspend_cron"`
	WakeCron    string     `json:"wake_cron"`
	State       string     `json:"state"` // watching | deferred | asleep | waking | wake_failed | skipped | failed | disabled
	Detail      string     `json:"detail,omitempty"`
	QuietForS   float64    `json:"quiet_for_s"`
	NextSuspend *time.Time `json:"next_suspend,omitempty"`
	NextWake    *time.Time `json:"next_wake,omitempty"`
	LastSuspend *time.Time `json:"last_suspend,omitempty"`
	LastWake    *time.Time `json:"last_wake,omitempty"`
	// DeferredSince marks when the currently-pending suspend was
	// declared, so an operator can see a schedule that has been blocked
	// for three hours rather than one that just fired.
	DeferredSince *time.Time `json:"deferred_since,omitempty"`
	LastSkip      string     `json:"last_skip,omitempty"`
	// WakeFailedSince is the wake this schedule promised and did not
	// deliver. It is the ONE thing in this phase that alarms (C9's
	// KindWakeFailed): not an observation of absence — an opportunistic
	// cell's absence never alarms — but a declared action that did not
	// complete.
	WakeFailedSince *time.Time `json:"wake_failed_since,omitempty"`

	// Loop-owned runtime, all touched under s.mu like warmTargetState's
	// emptySince.
	deferUntil  time.Time
	pending     bool
	asleep      bool
	asleepSince time.Time
	waking      bool
}

// sleepStatus is the fleet_status block.
type sleepStatus struct {
	Entries []sleepScheduleState `json:"entries,omitempty"`
}

// sleepLoopConfig wires the loop; every seam is injectable so the tests
// drive the real state machine rather than a re-implementation of it.
type sleepLoopConfig struct {
	entries   []SleepScheduleEntry
	loc       *time.Location
	frontURL  string
	suspendFn func(ctx context.Context, cell, reason string) error
	wakeFn    func(ctx context.Context, cell string) (string, error)
	warmFn    func(ctx context.Context, frontURL, model string) error
	tick      time.Duration
	poll      time.Duration
}

// SuspendFunc runs the suspend verb against one cell. It is injected
// because fleetapi has never known how to reach a cell's daemon — the
// same reason DoctorHost and CellAuth are injected (C13).
type SuspendFunc func(ctx context.Context, cell, reason string) error

// StartSleepLoop launches the declared sleep schedules with production
// defaults. nil loc means the process zone.
func (s *Server) StartSleepLoop(entries []SleepScheduleEntry, suspendFn SuspendFunc, loc *time.Location, frontURL string) {
	s.startSleepLoopWithConfig(sleepLoopConfig{
		entries: entries, loc: loc, frontURL: frontURL, suspendFn: suspendFn,
	})
}

func (s *Server) startSleepLoopWithConfig(cfg sleepLoopConfig) {
	if cfg.loc == nil {
		cfg.loc = time.Local
	}
	if cfg.tick <= 0 {
		cfg.tick = time.Minute
	}
	if cfg.poll <= 0 {
		cfg.poll = 15 * time.Second
	}
	if cfg.warmFn == nil {
		cfg.warmFn = warmViaFront
	}
	if cfg.wakeFn == nil {
		cfg.wakeFn = func(ctx context.Context, cell string) (string, error) {
			resp, err := s.SendWake(ctx, cell)
			if err != nil {
				return "", err
			}
			return resp.Sent + " → " + resp.Target, nil
		}
	}
	now := time.Now()
	for _, e := range cfg.entries {
		st := &sleepScheduleState{
			Cell: e.Cell, SuspendCron: e.Suspend, WakeCron: e.Wake,
			State: "watching", Detail: "watching", QuietForS: e.QuietFor.Seconds(),
		}
		sus, susErr := parseCron(e.Suspend)
		wake, wakeErr := parseCron(e.Wake)
		live := true
		switch {
		case susErr != nil:
			live = false
			st.State, st.Detail = "disabled", "invalid suspend cron: "+susErr.Error()
		case wakeErr != nil:
			// The whole entry dies, suspend half included: a broken wake
			// must never yield a box that sleeps forever. It yields a box
			// that never sleeps.
			live = false
			st.State, st.Detail = "disabled", "invalid wake cron: "+wakeErr.Error()+" (the suspend half is disabled too — a suspend with no working wake is a box that never comes back)"
		}
		if live {
			nextSus, okSus := sus.nextFire(now, cfg.loc)
			nextWake, okWake := wake.nextFire(now, cfg.loc)
			switch {
			case !okWake:
				live = false
				st.State, st.Detail = "disabled", "wake cron has no fire time within 8 years (the suspend half is disabled too)"
			case !okSus:
				live = false
				st.State, st.Detail = "disabled", "suspend cron has no fire time within 8 years"
			default:
				st.NextSuspend, st.NextWake = &nextSus, &nextWake
			}
		}
		if !live {
			slog.Warn("sleep schedule disabled", "cell", e.Cell, "detail", st.Detail)
		}
		s.mu.Lock()
		s.sleepStates = append(s.sleepStates, st)
		s.mu.Unlock()
		if live {
			s.wg.Add(1)
			go s.sleepEntryLoop(e, sus, wake, st, cfg)
		}
	}
}

// sleepEntryLoop ticks every minute, aligned to the minute boundary —
// warmsched.go's shape, because a schedule that drifts off the minute
// fires at :01 on some nights and never on others.
func (s *Server) sleepEntryLoop(e SleepScheduleEntry, sus, wake cronSpec, st *sleepScheduleState, cfg sleepLoopConfig) {
	defer s.wg.Done()
	first := time.Now().Truncate(cfg.tick).Add(cfg.tick)
	timer := time.NewTimer(time.Until(first))
	defer timer.Stop()
	var ticker *time.Ticker
	tick := timer.C
	for {
		select {
		case <-s.done:
			if ticker != nil {
				ticker.Stop()
			}
			return
		case now := <-tick:
			if ticker == nil {
				ticker = time.NewTicker(cfg.tick)
				tick = ticker.C
			}
			s.evalSleepEntry(e, sus, wake, st, cfg, now)
		}
	}
}

// evalSleepEntry is one minute's worth of the state machine. Order is
// load-bearing: the wake half runs before the suspend half, so a
// deferred suspend that survived to its own wake minute is abandoned
// rather than fired.
func (s *Server) evalSleepEntry(e SleepScheduleEntry, sus, wake cronSpec, st *sleepScheduleState, cfg sleepLoopConfig, now time.Time) {
	s.reconcileSleepState(e, st)

	if s.sleepDue(st, now) == sleepDueWake {
		next, ok := wake.nextFire(now, cfg.loc)
		s.mu.Lock()
		if ok {
			n := next
			st.NextWake = &n
		} else {
			st.NextWake = nil
		}
		if st.pending {
			// The suspend never fires after its own wake: a 23:30 suspend
			// deferred until 07:00 would be an outage with a
			// power-management theme, not a night's saving.
			st.pending = false
			st.DeferredSince = nil
			st.State, st.Detail = "skipped", "abandoned: still blocked at the paired wake ("+st.LastSkip+")"
		}
		start := !st.waking
		if start {
			st.waking = true
			st.State, st.Detail = "waking", "wake declared; clearing intent and sending the wake"
		}
		s.mu.Unlock()
		if start {
			s.wg.Add(1)
			go s.runWakeSequence(e, st, cfg)
		}
	}

	if s.sleepDue(st, now) == sleepDueSuspend {
		next, ok := sus.nextFire(now, cfg.loc)
		s.mu.Lock()
		if ok {
			n := next
			st.NextSuspend = &n
		} else {
			st.NextSuspend = nil
		}
		if !st.asleep && !st.waking {
			st.pending = true
			p := now.UTC()
			st.DeferredSince = &p
			st.deferUntil = now.Add(e.MaxDefer)
			if st.NextWake != nil && st.NextWake.Before(st.deferUntil) {
				st.deferUntil = *st.NextWake
			}
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	pending, deferUntil := st.pending, st.deferUntil
	s.mu.Unlock()
	if !pending {
		return
	}
	if !now.Before(deferUntil) {
		s.mu.Lock()
		st.pending = false
		st.DeferredSince = nil
		why := st.LastSkip
		if why == "" {
			why = "still blocked"
		}
		st.State, st.Detail = "skipped", "abandoned after the defer window ("+why+")"
		s.mu.Unlock()
		slog.Info("sleep schedule abandoned for tonight", "cell", e.Cell, "why", why)
		return
	}
	s.trySuspend(e, st, cfg)
}

// sleepDue values.
const (
	sleepDueNone = iota
	sleepDueSuspend
	sleepDueWake
)

// sleepDue reports which half (if either) is due, dereferencing the
// next-fire pointers under the lock rather than carrying them out of it.
// The wake is checked first, which is what makes a suspend that survived
// to its own wake minute get abandoned instead of fired.
func (s *Server) sleepDue(st *sleepScheduleState, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case st.NextWake != nil && !now.Before(*st.NextWake):
		return sleepDueWake
	case st.NextSuspend != nil && !now.Before(*st.NextSuspend):
		return sleepDueSuspend
	}
	return sleepDueNone
}

// reconcileSleepState is bookkeeping ONLY: it clears a wake_failed
// record when the cell comes back, and drops the asleep flag when the
// box is demonstrably up again with no sleep record left on it.
//
// It deliberately does NOT resume an early-returning box. Clearing the
// sleep intent because the cell reappeared would be an observation
// initiating an action, and the box is not stranded by leaving it: the
// display reads DRAINED with "asleep per sleep_schedule, eta 07:15",
// which is the true sentence, and `vibe cell resume` is one command.
func (s *Server) reconcileSleepState(e SleepScheduleEntry, st *sleepScheduleState) {
	present := s.cellPresent(e.Cell)
	_, hasSleep := s.sleepIntent(e.Cell)
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.WakeFailedSince != nil && present {
		st.WakeFailedSince = nil
		st.State, st.Detail = "watching", "cell returned after a failed wake"
	}
	if st.asleep && present && !hasSleep && time.Since(st.asleepSince) > sleepReturnGrace {
		st.asleep = false
		if st.State == "asleep" {
			st.State, st.Detail = "watching", "cell is back and its sleep record is gone"
		}
	}
}

// trySuspend applies the guard ladder and, if every rung passes, runs
// the suspend. Intent is recorded AFTER the verb succeeds — a failed
// verb records nothing (C2's one-writer rule).
func (s *Server) trySuspend(e SleepScheduleEntry, st *sleepScheduleState, cfg sleepLoopConfig) {
	block, ok := s.suspendGuard(e.Cell, e.QuietFor)
	switch {
	case ok:
	case block.Absent:
		s.mu.Lock()
		st.pending = false
		st.DeferredSince = nil
		s.mu.Unlock()
		s.setSleepSkip(st, "skipped", block.Why)
		return
	default:
		s.setSleepSkip(st, "deferred", block.Why)
		return
	}

	// The instant the suspend is ISSUED, kept for the record's `since`.
	// The cell stamps its own drained echo while it is still running,
	// i.e. after this instant — so a record stamped when the RPC RETURNED
	// would be permanently newer than the only echo that will ever
	// exist, and the entry would sit as "requested, awaiting ack" all
	// night for an ack a sleeping box cannot give.
	issued := time.Now().UTC()
	ctx, cancel := s.warmCtx(suspendTimeout)
	err := cfg.suspendFn(ctx, e.Cell, SleepIntentReason)
	cancel()
	if err != nil {
		s.setSleepSkip(st, "failed", "suspend failed: "+err.Error()+
			" (no intent recorded; the paired wake still fires)")
		slog.Error("scheduled suspend failed", "cell", e.Cell, "err", err)
		return
	}

	eta := ""
	s.mu.Lock()
	if st.NextWake != nil {
		eta = st.NextWake.In(cfg.loc).Format("15:04")
	}
	s.mu.Unlock()
	detail := "suspended"
	if _, ierr := s.SetIntentAt(e.Cell, "drained", SleepIntentReason, eta, issued); ierr != nil {
		// The box IS asleep; failing to say why is a status problem, not a
		// reason to misreport the verb. Loud either way.
		slog.Error("suspended the cell but could not record intent", "cell", e.Cell, "err", ierr)
		detail = "suspended, but intent not recorded: " + ierr.Error()
	}
	now := time.Now()
	s.mu.Lock()
	st.pending = false
	st.DeferredSince = nil
	st.asleep = true
	st.asleepSince = now
	u := now.UTC()
	st.LastSuspend = &u
	st.State, st.Detail, st.LastSkip = "asleep", detail, ""
	s.mu.Unlock()
	slog.Info("cell suspended per sleep_schedule", "cell", e.Cell, "wake", eta)
}

// suspendTimeout bounds one suspend RPC. Generous relative to the verb
// (a unit stop plus a logind call) and far below the minute tick.
const suspendTimeout = 90 * time.Second

// runWakeSequence is the paired wake: clear OUR intent, send the wake,
// await the return, warm the declared models. Order matters — see the
// intent comment below — and it runs on its own goroutine so the
// entry's minute ticker keeps ticking through a ten-minute wait.
func (s *Server) runWakeSequence(e SleepScheduleEntry, st *sleepScheduleState, cfg sleepLoopConfig) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		st.waking = false
		st.asleep = false
		s.mu.Unlock()
	}()

	// Whether THIS schedule is why the box is away decides one thing only:
	// whether a wake that does not deliver may alarm. The packet is sent
	// either way — the cron declared it.
	s.mu.Lock()
	ours := st.asleep
	s.mu.Unlock()

	// The clear goes FIRST, before the packet. A box that comes back to
	// find fleetd still requesting `drained` receives that request as
	// desired_intent on its first heartbeat and runs its own
	// cell_cmds.drain — a morning with the box powered on and the serving
	// stack stopped.
	cleared := s.clearSleepIntent(e.Cell)
	ours = ours || cleared
	var notes []string
	if cleared {
		notes = append(notes, "sleep intent cleared (resume requested)")
	}

	ctx, cancel := s.warmCtx(e.WakeGrace + warmTimeout)
	defer cancel()

	if s.cellPresent(e.Cell) {
		notes = append(notes, "cell already up; no wake packet sent")
	} else {
		how, err := cfg.wakeFn(ctx, e.Cell)
		if err != nil {
			s.failWake(st, strings.Join(append(notes, "wake could not be sent: "+err.Error()), "; "), ours)
			return
		}
		notes = append(notes, "wake sent ("+how+")")
	}

	if !s.awaitCellReturn(ctx, e.Cell, e.WakeGrace, cfg.poll) {
		s.failWake(st, strings.Join(append(notes, fmt.Sprintf("cell did not come back within %s", e.WakeGrace)), "; "), ours)
		return
	}
	notes = append(notes, "cell returned")

	// The wake fires whether or not this schedule was the reason the box
	// was away, so the warms take the same guards the warm loops take: a
	// declared drain (C4 §1's eviction fight, at an hour when nobody is
	// watching) and a C11 hold, which is the operator's declaration that
	// fleetd must not act on its own warm policy on this cell at all.
	in, drained := s.effectiveIntent(e.Cell)
	h, held := s.HoldOn(e.Cell)
	switch {
	case drained && in.State == "drained":
		notes = append(notes, "warms skipped: cell is drained ("+in.Reason+")")
	case held:
		notes = append(notes, "warms skipped: "+holdDetail(h))
	default:
		for _, m := range e.Warm {
			notes = append(notes, s.wakeWarm(ctx, e.Cell, m, cfg))
		}
	}

	now := time.Now().UTC()
	s.mu.Lock()
	st.LastWake = &now
	st.WakeFailedSince = nil
	st.State, st.Detail, st.LastSkip = "watching", strings.Join(notes, "; "), ""
	s.mu.Unlock()
	slog.Info("cell woken per sleep_schedule", "cell", e.Cell, "detail", strings.Join(notes, "; "))
}

// wakeWarm warms one declared model through the front, falling back to
// the piggyback queue exactly as the warm schedule does (C6's rule: a
// 4xx is the far side answering and stays an error).
func (s *Server) wakeWarm(ctx context.Context, cell, model string, cfg sleepLoopConfig) string {
	// The third producer of the same chat completion, and the one that
	// fires at 07:15 with nobody watching.
	if refused := s.warmClassRefusal(model); refused != "" {
		return "warm " + model + " refused: " + refused
	}
	var err error
	if s.frontCanRoute(cell) {
		err = cfg.warmFn(ctx, cfg.frontURL, model)
	} else {
		err = noFrontRoute(cell)
	}
	if err == nil {
		return "warmed " + model
	}
	note, qerr := s.queueWarm(cell, model, err)
	if qerr != nil {
		return "warm " + model + " failed: " + qerr.Error()
	}
	return "warm " + model + " " + note
}

// failWake records a wake that did not deliver. `ours` is what decides
// whether it may ALARM, and the distinction is the alarm's whole
// justification: the class table says an opportunistic cell's absence
// never pages, forever, so the only thing this phase is allowed to page
// about is a suspend THIS schedule performed whose paired wake did not
// bring the box back — a promise the control plane made and broke.
//
// A wake that fires on its cron against a box nobody here suspended (the
// operator switched it off on Friday) found an absence, and an absence
// of that class is not a fault. It stays visible in fleet_status and the
// log; it does not page, does not set wake_failed_since, and does not
// raise the event. C9 shipped exactly this bug once already — an alarm
// on an opportunistic cell being switched off — and this one would have
// fired every single morning.
func (s *Server) failWake(st *sleepScheduleState, detail string, ours bool) {
	now := time.Now().UTC()
	s.mu.Lock()
	cell := st.Cell
	if !ours {
		st.State = "skipped"
		st.Detail = detail + " — this schedule did not suspend it (an opportunistic cell may simply be off)"
		st.LastSkip = st.Detail
		s.mu.Unlock()
		slog.Info("scheduled wake found nothing to bring back", "cell", cell, "detail", detail)
		return
	}
	if st.WakeFailedSince == nil {
		st.WakeFailedSince = &now
	}
	st.LastWake = &now
	st.State, st.Detail = "wake_failed", detail
	s.mu.Unlock()
	slog.Error("scheduled wake did not bring the cell back", "cell", cell, "detail", detail)
	s.publish(Event{Cell: cell, Type: EventWakeFailed})
}

// awaitCellReturn polls presence and reachability until the cell is
// back or the grace expires. Bounded by ctx (linked to s.done through
// warmCtx) so Close() never waits a full grace window.
func (s *Server) awaitCellReturn(ctx context.Context, cell string, grace, poll time.Duration) bool {
	deadline := time.Now().Add(grace)
	tk := time.NewTicker(poll)
	defer tk.Stop()
	for {
		if s.cellPresent(cell) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return s.cellPresent(cell)
		case <-s.done:
			return s.cellPresent(cell)
		case <-tk.C:
		}
	}
}

// cellPresent reports whether the cell is demonstrably here: a fresh
// announce, or the probe watcher's stream being up. Both are
// observations; neither is ever a trigger.
func (s *Server) cellPresent(cell string) bool {
	if p := s.presenceFor(cell); p != nil && p.Announcing && !p.Stale && !p.Withdrawn {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cellUp[cell]
}

// SuspendBlock is one guard rung's answer: why the box may not go down
// now, and whether that answer is permanent.
//
// Structural marks a CONFIGURATION refusal — the front, an unknown
// cell, the wrong class — which no operator override may bypass:
// forcing a suspend of the front is not an override, it is a bug.
// Everything else is tonight's conditions, which `force` may overrule
// because an operator asking is not fleetd guessing (C11's line).
type SuspendBlock struct {
	Why        string
	Structural bool
	// Absent marks the one answer that is neither a refusal nor a
	// deferral: there is no box here to suspend. The schedule stops
	// retrying for the night and — the part that matters — records no
	// intent, because fleetd did not put this box to sleep and must not
	// claim it did.
	Absent bool
}

// suspendGuard is the ladder, shared by both producers — the schedule
// and the explicit suspend_cell verb — so an agent and the loop can
// never disagree about whether a box may go down. Every rung DEFERS a
// declared action; not one of them can cause one.
func (s *Server) suspendGuard(cell string, quietFor time.Duration) (SuspendBlock, bool) {
	structural := func(f string, a ...any) (SuspendBlock, bool) {
		return SuspendBlock{Why: fmt.Sprintf(f, a...), Structural: true}, false
	}
	policy := func(f string, a ...any) (SuspendBlock, bool) {
		return SuspendBlock{Why: fmt.Sprintf(f, a...)}, false
	}
	if cell == fleetcfg.FrontCell {
		return structural("%s is the front: the data plane and the control plane ride it", fleetcfg.FrontCell)
	}
	if !s.knownCell(cell) {
		return structural("unknown cell %q (not in the registry)", cell)
	}
	if class := s.cellClass(cell); class != string(fleetcfg.ClassOpportunistic) {
		return structural("cell %s is class %s: only opportunistic cells sleep (always_on absence alarms by design; a roaming cell cannot be woken by a packet on this LAN)", cell, classOrUnset(class))
	}
	if in, ok := s.effectiveIntent(cell); ok && in.State == "drained" {
		why := in.Reason
		if why == "" {
			why = "no reason given"
		}
		return policy("cell %s is drained (%s) — a drained box is one the operator took", cell, why)
	}
	if h, held := s.HoldOn(cell); held {
		return policy("%s is %s", cell, holdDetail(h))
	}
	// Absence outranks every evidence rung below it, and the order is
	// load-bearing: an absent cell reports no in-flight count either, and
	// answering "in-flight unknown" would turn "there is no box here"
	// into a deferral that retries all night.
	if !s.cellPresent(cell) {
		return SuspendBlock{
			Why:    fmt.Sprintf("nothing to suspend: %s is already absent (fleetd did not put it to sleep and will not claim it did)", cell),
			Absent: true,
		}, false
	}
	n, reported := s.InFlight(cell)
	switch {
	case !reported:
		return policy("cell %s in-flight unknown — unknown is not zero", cell)
	case n > 0:
		return policy("cell %s has %d in-flight", cell, n)
	}
	if leases := s.leasesForCellActive(cell); len(leases) > 0 {
		return policy("%d active leases on %s", len(leases), cell)
	}
	if models := s.pendingProbes(cell); len(models) > 0 {
		return policy("a probe of %s on %s is outstanding — fleetd asked for that measurement", strings.Join(models, ","), cell)
	}
	// Defence in depth only: reaching here means the in-flight rung above
	// already saw a frame from this cell, which is itself an observation
	// channel, so observesActivity cannot currently be false here. It
	// stays because the day in-flight becomes derivable from a second
	// source is the day this rung matters again — and it is cheap.
	if !s.observesActivity(cell) {
		return policy("no activity evidence for %s (no events stream) — fleetd never looked, which is not the same as saw nothing", cell)
	}
	// The quiet window is a claim about a stretch fleetd was WATCHING, so
	// with no request stamp for this cell it measures from fleetd's own
	// start — C4's swapIdleFor rule verbatim, and for the same reason:
	// fleetd cannot claim silence it was not running to observe. Without
	// the floor a fleetd restarted at 23:29 reads "quiet forever" and
	// suspends the box its operator was using at 23:28, which is exactly
	// the human this window exists for.
	if last, ok := s.cellLastActivity(cell); ok {
		if idle := time.Since(last); idle < quietFor {
			return policy("cell %s served a request %s ago (quiet window %s)", cell, idle.Round(time.Second), quietFor)
		}
	} else if watched := time.Since(s.started); watched < quietFor {
		return policy("no request activity on record for %s and fleetd has only been watching for %s (quiet window %s) — it cannot claim silence it was not running to observe",
			cell, watched.Round(time.Second), quietFor)
	}
	return SuspendBlock{}, true
}

// SuspendGuard is the exported gate for the explicit verbs. A
// non-positive quietFor takes the schedule's default, so an operator
// verb is never accidentally the laxer of the two.
func (s *Server) SuspendGuard(cell string, quietFor time.Duration) (SuspendBlock, bool) {
	if quietFor <= 0 {
		quietFor = DefaultSleepQuietFor
	}
	return s.suspendGuard(cell, quietFor)
}

func classOrUnset(class string) string {
	if class == "" {
		return "unset"
	}
	return class
}

// cellLastActivity is the most recent request stamp across every model
// of a cell — C4's per-model activity map, read cell-wide, because the
// contended resource is the box. The bool is false when no frame has
// ever mentioned any of the cell's models; combined with
// observesActivity that means "watched, saw nothing", which is the only
// reading under which the quiet window may pass.
func (s *Server) cellLastActivity(cell string) (time.Time, bool) {
	prefix := cell + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	var last time.Time
	found := false
	for k, t := range s.modelActivity {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if !found || t.After(last) {
			last, found = t, true
		}
	}
	return last, found
}

// pendingProbes lists probe verbs queued for or handed to a cell. A
// probe fleetd ASKED for is GPU work fleetd started; suspending the box
// loses the measurement and the reason it was taken. The in-flight
// counter covers the running half of the same window (a probe is a
// request like any other on the cell's own llama-swap); this covers the
// half where the request has been handed over and the answer has not
// come back.
func (s *Server) pendingProbes(cell string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	add := func(cmds []AnnounceCommand) {
		for _, c := range cmds {
			if c.Verb == "probe" {
				out = append(out, c.Model)
			}
		}
	}
	add(s.commands[cell])
	if pend, ok := s.cmdInflight[cell]; ok {
		add(pend.cmds)
	}
	return out
}

// sleepIntent returns the cell's intent entry when it is one THIS
// schedule wrote.
func (s *Server) sleepIntent(cell string) (Intent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.intents[cell]
	if !ok || in.State != "drained" || in.Reason != SleepIntentReason {
		return Intent{}, false
	}
	return in, true
}

// clearSleepIntent clears the sleep record — and only the sleep record.
// An operator's own drain reason survives a wake untouched: clearing it
// would silently resume a box its owner declared they were using, which
// is inferred intent acted upon.
func (s *Server) clearSleepIntent(cell string) bool {
	if _, ok := s.sleepIntent(cell); !ok {
		return false
	}
	if _, err := s.SetIntent(cell, "serving", "", ""); err != nil {
		slog.Error("could not clear the sleep intent before waking", "cell", cell, "err", err)
		return false
	}
	return true
}

func (s *Server) setSleepSkip(st *sleepScheduleState, state, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.State, st.Detail, st.LastSkip = state, detail, detail
}

// sleepReport builds the fleet_status block, copying so a reader never
// holds loop-owned pointers.
func (s *Server) sleepReport() *sleepStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sleepStates) == 0 {
		return nil
	}
	rep := &sleepStatus{}
	for _, st := range s.sleepStates {
		rep.Entries = append(rep.Entries, cloneSleepState(st))
	}
	return rep
}

// cloneSleepState deep-copies the *time.Time fields; a struct copy
// would carry loop-owned pointers out of the lock that owns them.
func cloneSleepState(st *sleepScheduleState) sleepScheduleState {
	cp := *st
	for _, pair := range []struct{ src, dst **time.Time }{
		{&st.NextSuspend, &cp.NextSuspend},
		{&st.NextWake, &cp.NextWake},
		{&st.LastSuspend, &cp.LastSuspend},
		{&st.LastWake, &cp.LastWake},
		{&st.DeferredSince, &cp.DeferredSince},
		{&st.WakeFailedSince, &cp.WakeFailedSince},
	} {
		if *pair.src != nil {
			t := **pair.src
			*pair.dst = &t
		}
	}
	return cp
}
