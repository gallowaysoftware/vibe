package fleetnotify

import (
	"strings"
	"testing"
	"time"
)

// C9's policy engine. Everything here runs on an explicit clock, so a
// dwell test is a statement about the rule rather than about the
// machine's timer resolution.

var epoch = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func absent(cell string) Condition {
	return Condition{Kind: KindCellAbsent, Scope: cell, Detail: cell + " is OFF/AWAY"}
}

// fastPolicy keeps the shape of the real policy (dwell on both edges, a
// bucket) with durations a test can step through.
func fastPolicy() Policy {
	p := DefaultPolicy()
	p.Dwell = map[Kind]time.Duration{
		KindCellAbsent:     2 * time.Minute,
		KindFingerprint:    15 * time.Minute,
		KindDrainWithLease: 0,
	}
	p.ClearDwell = time.Minute
	return p
}

func states(ns []Notification) string {
	var b strings.Builder
	for i, n := range ns {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(n.State + ":" + DisplayKey(n.Key))
	}
	return b.String()
}

// TestDefaultAlarmsAreExactlyTheClassTablesAlarmColumnPlusWakeFailed
// pins the shipped policy. The design doc §4 table's "alarm? yes"
// column is always_on absence; the futures item adds persistent
// fingerprint drift and drain-with-lease. C14 adds wake_failed, which
// is NOT a class-table row and is allowed in for one reason only: it is
// not an observation of absence (an opportunistic cell's absence never
// alarms) but a declared action of the control plane's own that did not
// complete. Anything else in this list is a notifier the operator will
// learn to ignore.
func TestDefaultAlarmsAreExactlyTheClassTablesAlarmColumnPlusWakeFailed(t *testing.T) {
	got := DefaultAlarms()
	want := []Kind{KindCellAbsent, KindFingerprint, KindDrainWithLease, KindWakeFailed}
	if len(got) != len(want) {
		t.Fatalf("default alarms = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default alarms = %v, want %v", got, want)
		}
	}
	for _, k := range DefaultAlarms() {
		if k == KindModelDegraded {
			t.Fatal("model_degraded is in the default set: C8's verdict has a false-positive tail and the class table does not list it")
		}
	}
}

// TestTracker_DisabledKindNeverNotifiesEvenWhileTrue proves the enabled
// set is a gate on the condition, not merely on the config parser: a
// degraded model handed to a default-policy tracker forever produces
// nothing.
func TestTracker_DisabledKindNeverNotifiesEvenWhileTrue(t *testing.T) {
	tr := NewTracker(fastPolicy())
	cond := Condition{Kind: KindModelDegraded, Scope: "gpu-cell/qwen", Detail: "slow"}
	now := epoch
	for range 100 {
		if out := tr.Step(now, []Condition{cond}, false); len(out) != 0 {
			t.Fatalf("model_degraded notified under the default policy: %s", states(out))
		}
		now = now.Add(time.Minute)
	}
	if len(tr.Status().Alarms) != 0 {
		t.Fatalf("disabled kind entered the tracker: %+v", tr.Status().Alarms)
	}
}

// TestTracker_FiresOnceAfterTheDwellAndNeverRepeats is the positive
// control for every suppression test below, plus the no-repeat rule: a
// pager that re-notifies about a box you already know about is one that
// gets silenced.
func TestTracker_FiresOnceAfterTheDwellAndNeverRepeats(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch

	if out := tr.Step(now, []Condition{absent("hum")}, false); len(out) != 0 {
		t.Fatalf("fired before the dwell: %s", states(out))
	}
	now = now.Add(90 * time.Second)
	if out := tr.Step(now, []Condition{absent("hum")}, false); len(out) != 0 {
		t.Fatalf("fired at 90s of a 2m dwell: %s", states(out))
	}
	now = now.Add(45 * time.Second)
	out := tr.Step(now, []Condition{absent("hum")}, false)
	if len(out) != 1 || out[0].State != StateFiring || out[0].Scope != "hum" {
		t.Fatalf("want one firing notification, got %s", states(out))
	}

	for range 60 {
		now = now.Add(time.Minute)
		if extra := tr.Step(now, []Condition{absent("hum")}, false); len(extra) != 0 {
			t.Fatalf("re-notified an hour into a known alarm: %s", states(extra))
		}
	}
}

// TestTracker_ResolveFiresOnceWhenTheConditionClears covers the
// await-unblocked half: the thing you were told was broken says it is
// better, exactly once.
func TestTracker_ResolveFiresOnceWhenTheConditionClears(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch
	tr.Step(now, []Condition{absent("hum")}, false)
	now = now.Add(3 * time.Minute)
	tr.Step(now, []Condition{absent("hum")}, false)

	// The first false evaluation only ENTERS clearing; the clear dwell
	// runs from there.
	now = now.Add(30 * time.Second)
	if out := tr.Step(now, nil, false); len(out) != 0 {
		t.Fatalf("resolved on the first false evaluation: %s", states(out))
	}
	now = now.Add(30 * time.Second)
	if out := tr.Step(now, nil, false); len(out) != 0 {
		t.Fatalf("resolved at 30s of a 1m clear dwell: %s", states(out))
	}
	now = now.Add(45 * time.Second)
	out := tr.Step(now, nil, false)
	if len(out) != 1 || out[0].State != StateResolved {
		t.Fatalf("want one resolve, got %s", states(out))
	}
	now = now.Add(time.Hour)
	if extra := tr.Step(now, nil, false); len(extra) != 0 {
		t.Fatalf("kept resolving a cleared alarm: %s", states(extra))
	}
}

// TestTracker_TwoHundredFlapsBelowTheDwellNotifyZeroTimes is the phase's
// headline coalescing claim, stated as the literal number the futures
// item used. A cell that cannot stay absent for its dwell never leaves
// pending, so it produces nothing in EITHER direction — not one per
// flap, not one per cycle.
func TestTracker_TwoHundredFlapsBelowTheDwellNotifyZeroTimes(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch
	sent := 0
	for range 200 {
		now = now.Add(30 * time.Second)
		sent += len(tr.Step(now, []Condition{absent("hum")}, false))
		now = now.Add(30 * time.Second)
		sent += len(tr.Step(now, nil, false))
	}
	if sent != 0 {
		t.Fatalf("200 flaps produced %d notifications; the dwell is not holding", sent)
	}
}

// TestTracker_FlapBackDuringTheClearDwellNeitherResolvesNorRefires is
// the other half of the both-edges rule: a blip inside the clear window
// is the same alarm, not a new one.
func TestTracker_FlapBackDuringTheClearDwellNeitherResolvesNorRefires(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch
	tr.Step(now, []Condition{absent("hum")}, false)
	now = now.Add(3 * time.Minute)
	if out := tr.Step(now, []Condition{absent("hum")}, false); len(out) != 1 {
		t.Fatalf("setup: want the initial firing, got %s", states(out))
	}
	now = now.Add(10 * time.Second)
	tr.Step(now, nil, false) // enters clearing
	now = now.Add(10 * time.Second)
	if out := tr.Step(now, []Condition{absent("hum")}, false); len(out) != 0 {
		t.Fatalf("a flap back during clearing notified: %s", states(out))
	}
	now = now.Add(time.Hour)
	if out := tr.Step(now, []Condition{absent("hum")}, false); len(out) != 0 {
		t.Fatalf("the returned alarm re-fired: %s", states(out))
	}
}

// TestTracker_ZeroDwellKindFiresOnTheFirstEvaluation pins
// drain_with_lease's deliberate lack of a dwell: the batch job is being
// truncated right now.
func TestTracker_ZeroDwellKindFiresOnTheFirstEvaluation(t *testing.T) {
	tr := NewTracker(fastPolicy())
	cond := Condition{Kind: KindDrainWithLease, Scope: "gpu-cell", Detail: "batch-a holds qwen"}
	out := tr.Step(epoch, []Condition{cond}, false)
	if len(out) != 1 || out[0].State != StateFiring || !strings.Contains(out[0].Message, "batch-a") {
		t.Fatalf("want an immediate firing naming the holder, got %+v", out)
	}
}

// TestTracker_AwaySuppressesDeliveryButKeepsTheAlarmVisible is the
// vacation promise: away defers, it does not mute, and the evidence is
// in Status where fleet_status reads it.
func TestTracker_AwaySuppressesDeliveryButKeepsTheAlarmVisible(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch
	tr.Step(now, []Condition{absent("hum")}, true)
	now = now.Add(3 * time.Minute)
	if out := tr.Step(now, []Condition{absent("hum")}, true); len(out) != 0 {
		t.Fatalf("away delivered an alarm: %s", states(out))
	}
	st := tr.Status()
	if st.Suppressed != 1 || len(st.SuppressedKeys) != 1 || st.SuppressedKeys[0] != "cell_absent/hum" {
		t.Fatalf("suppression is invisible in status: %+v", st)
	}
	var active bool
	for _, a := range st.Alarms {
		if a.Key == "cell_absent/hum" && a.State == "active" {
			active = true
		}
	}
	if !active {
		t.Fatalf("the alarm itself vanished while away: %+v", st.Alarms)
	}
}

// TestTracker_ComingHomeSendsExactlyOneDigestNamingWhatWasSuppressed is
// what makes away a deferral rather than a week-long mute.
func TestTracker_ComingHomeSendsExactlyOneDigestNamingWhatWasSuppressed(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch
	tr.Step(now, []Condition{absent("hum"), absent("front")}, true)
	now = now.Add(3 * time.Minute)
	tr.Step(now, []Condition{absent("hum"), absent("front")}, true)

	now = now.Add(time.Minute)
	out := tr.Step(now, []Condition{absent("hum"), absent("front")}, false)
	if len(out) != 1 || out[0].State != StateDigest {
		t.Fatalf("want exactly one digest on return, got %s", states(out))
	}
	for _, want := range []string{"cell_absent/hum", "cell_absent/front", "STILL ACTIVE"} {
		if !strings.Contains(out[0].Message, want) {
			t.Fatalf("digest does not name %q: %q", want, out[0].Message)
		}
	}
	now = now.Add(time.Minute)
	if extra := tr.Step(now, []Condition{absent("hum"), absent("front")}, false); len(extra) != 0 {
		t.Fatalf("a second digest went out: %s", states(extra))
	}
	if tr.Status().Suppressed != 0 {
		t.Fatalf("the suppressed tally survived the digest: %+v", tr.Status())
	}
}

// TestTracker_AwayNeverSuppressesAnExplicitMessage: `notify test` and
// `await --notify` are a human asking for a message now, not an alarm.
// The one command that proves the pager works must not be the one
// command that silently does nothing while you are away.
func TestTracker_AwayNeverSuppressesAnExplicitMessage(t *testing.T) {
	tr := NewTracker(fastPolicy())
	n := Explicit(epoch, "vibe fleet: test", "hello")
	if n.State != StateExplicit {
		t.Fatalf("explicit state = %q", n.State)
	}
	// The explicit path bypasses Step entirely; assert the gate it would
	// have hit does not claim it.
	out := tr.gate(epoch, n, true)
	if len(out) != 1 {
		t.Fatalf("away suppressed an explicit message: %s", states(out))
	}
	if tr.Status().Suppressed != 0 {
		t.Fatalf("an explicit message was counted as a suppressed alarm: %+v", tr.Status())
	}
}

// TestTracker_RateLimitDefersRatherThanDroppingAndDrainsLater: the
// bucket is a pacer, not a shredder — an alarm it stops is delivered
// when a token returns, because the state machine will never re-fire it.
func TestTracker_RateLimitDefersRatherThanDroppingAndDrainsLater(t *testing.T) {
	p := fastPolicy()
	p.Burst = 1
	p.RatePerHour = 60 // one token a minute
	p.Dwell = map[Kind]time.Duration{KindDrainWithLease: 0}
	p.Alarms = []Kind{KindDrainWithLease}
	tr := NewTracker(p)

	cond := func(cell string) Condition {
		return Condition{Kind: KindDrainWithLease, Scope: cell, Detail: cell + " drained with a lease"}
	}
	out := tr.Step(epoch, []Condition{cond("a"), cond("b"), cond("c")}, false)
	if len(out) != 1 {
		t.Fatalf("burst 1 delivered %d: %s", len(out), states(out))
	}
	if tr.Status().Deferred != 2 {
		t.Fatalf("want 2 deferred, got %+v", tr.Status())
	}
	next := tr.Step(epoch.Add(time.Minute), []Condition{cond("a"), cond("b"), cond("c")}, false)
	if len(next) != 1 || next[0].State != StateFiring {
		t.Fatalf("the deferred alarm did not drain: %s", states(next))
	}
	final := tr.Step(epoch.Add(2*time.Minute), []Condition{cond("a"), cond("b"), cond("c")}, false)
	if len(final) != 1 {
		t.Fatalf("the last deferred alarm did not drain: %s", states(final))
	}
	if tr.Status().Deferred != 0 {
		t.Fatalf("deferral queue not empty: %+v", tr.Status())
	}
}

// TestTracker_DeferralQueueIsBoundedByMaxDeferred: a storm must not turn
// the pacer into unbounded memory.
func TestTracker_DeferralQueueIsBoundedByMaxDeferred(t *testing.T) {
	p := fastPolicy()
	p.Burst = 1
	p.RatePerHour = 0.0001
	p.MaxDeferred = 4
	p.Alarms = []Kind{KindDrainWithLease}
	p.Dwell = map[Kind]time.Duration{KindDrainWithLease: 0}
	tr := NewTracker(p)

	var conds []Condition
	for i := range 40 {
		conds = append(conds, Condition{Kind: KindDrainWithLease, Scope: string(rune('a'+i%26)) + string(rune('0'+i/26)), Detail: "x"})
	}
	tr.Step(epoch, conds, false)
	if got := tr.Status().Deferred; got != 4 {
		t.Fatalf("deferral queue = %d, want the 4-entry bound", got)
	}
}

// TestTracker_StatusSinceIsWhenTheConditionStartedNotTheLastTransition:
// "hum has been absent since 03:14" is the sentence an operator needs,
// and a flap back through clearing must not reset it.
func TestTracker_StatusSinceIsWhenTheConditionStartedNotTheLastTransition(t *testing.T) {
	tr := NewTracker(fastPolicy())
	now := epoch
	tr.Step(now, []Condition{absent("hum")}, false)
	now = now.Add(3 * time.Minute)
	tr.Step(now, []Condition{absent("hum")}, false)
	now = now.Add(time.Second)
	tr.Step(now, nil, false)
	now = now.Add(time.Second)
	tr.Step(now, []Condition{absent("hum")}, false)

	st := tr.Status()
	if len(st.Alarms) != 1 || !st.Alarms[0].Since.Equal(epoch) {
		t.Fatalf("since = %v, want the first-true instant %v (%+v)", st.Alarms[0].Since, epoch, st.Alarms)
	}
}

// TestTracker_APartialDwellOverrideKeepsTheOtherKindsDefaults: merging
// per MAP rather than per KIND turned every unlisted kind's threshold
// into zero — an instant page by omission, and the 15-minute
// persistence rule silently gone.
func TestTracker_APartialDwellOverrideKeepsTheOtherKindsDefaults(t *testing.T) {
	tr := NewTracker(Policy{Dwell: map[Kind]time.Duration{KindCellAbsent: time.Second}})
	got := tr.Policy()
	if got.Dwell[KindCellAbsent] != time.Second {
		t.Fatalf("the override was lost: %v", got.Dwell[KindCellAbsent])
	}
	if got.Dwell[KindFingerprint] != 15*time.Minute {
		t.Fatalf("fingerprint dwell = %v, want the 15m default", got.Dwell[KindFingerprint])
	}
	cond := Condition{Kind: KindFingerprint, Scope: "gpu-cell/bge-m3", Detail: "drift"}
	if out := tr.Step(epoch, []Condition{cond}, false); len(out) != 0 {
		t.Fatalf("a fingerprint mismatch paged instantly: %s", states(out))
	}
}

// TestTracker_AwayHoldsTheRateLimitedBacklogToo: a notification the
// bucket held back before the operator left is still an alarm, and
// delivering it mid-vacation is exactly the noise away exists to stop.
func TestTracker_AwayHoldsTheRateLimitedBacklogToo(t *testing.T) {
	p := fastPolicy()
	p.Burst = 1
	p.RatePerHour = 60
	p.Alarms = []Kind{KindDrainWithLease}
	p.Dwell = map[Kind]time.Duration{KindDrainWithLease: 0}
	tr := NewTracker(p)
	cond := func(cell string) Condition {
		return Condition{Kind: KindDrainWithLease, Scope: cell, Detail: cell + " drained with a lease"}
	}
	conds := []Condition{cond("a"), cond("b")}

	if out := tr.Step(epoch, conds, false); len(out) != 1 {
		t.Fatalf("setup: burst 1 delivered %s", states(out))
	}
	if tr.Status().Deferred != 1 {
		t.Fatalf("setup: want one deferred, got %+v", tr.Status())
	}
	// An hour of away with a full bucket: the backlog must not move.
	for i := range 60 {
		if out := tr.Step(epoch.Add(time.Duration(i+1)*time.Minute), conds, true); len(out) != 0 {
			t.Fatalf("away delivered the backlog: %s", states(out))
		}
	}
	if tr.Status().Deferred != 1 {
		t.Fatalf("the backlog changed while away: %+v", tr.Status())
	}
	out := tr.Step(epoch.Add(61*time.Minute), conds, false)
	if len(out) != 1 || out[0].Scope != "b" {
		t.Fatalf("the backlog did not drain on return: %s", states(out))
	}
}

func TestParseKind_RejectsUnknownKindsByName(t *testing.T) {
	if _, err := ParseKind("cell_absent"); err != nil {
		t.Fatalf("cell_absent rejected: %v", err)
	}
	_, err := ParseKind("everything")
	if err == nil || !strings.Contains(err.Error(), "cell_absent") {
		t.Fatalf("want an error listing the vocabulary, got %v", err)
	}
}
