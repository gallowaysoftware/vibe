// Package fleetnotify turns the fleet's alarm CONDITIONS into delivered
// messages (fleet-control C9). It is deliberately not an event
// forwarder: two of the four alarms the design's class table asks for
// have no edge to forward (a persistent fingerprint mismatch publishes
// exactly one event and then goes silent forever; a drain landing on a
// leased cell publishes none at all), and an edge cannot carry a dwell,
// which is the only thing that stops a flapping cell from paging all
// night.
//
// The package holds no fleet state and imports no fleet package: the
// caller decides which conditions are true right now, and Tracker
// decides which of them are worth a human's attention. That seam is what
// makes the policy testable without a clock, a socket or a cell.
package fleetnotify

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind is one alarm kind. The default set is the design doc §4 class
// table's "alarm? yes" column and nothing else — a notifier that fires
// on everything is one the operator learns to swipe away, which is
// worse than no notifier at all.
type Kind string

const (
	// KindCellAbsent is an always_on cell that is absent with no declared
	// explanation. Class is part of the condition, not a filter bolted on
	// after: an opportunistic workstation being off and a roaming laptop
	// leaving the building are the normal case, forever.
	KindCellAbsent Kind = "cell_absent"
	// KindFingerprint is a serving-flags mismatch that has persisted (see
	// Policy.Dwell) — friction pain 4, where drift is silent retrieval
	// damage.
	KindFingerprint Kind = "fingerprint_drift"
	// KindDrainWithLease is a drain recorded against a cell that still
	// holds advisory leases: the "did I just strand a 19-hour job?"
	// question, answered without being asked.
	KindDrainWithLease Kind = "drain_with_lease"
	// KindModelDegraded is C8's throughput verdict. Implemented, and
	// deliberately NOT in the default set: the class table does not list
	// it, and the measurement has a false-positive tail that C8 already
	// refused to let actuate anything.
	KindModelDegraded Kind = "model_degraded"
	// KindWakeFailed is C14's: fleetd suspended a cell on a declared
	// schedule and its own paired wake did not bring the box back. It is
	// in the default set despite not appearing in the class table, and
	// the distinction is the whole reason it is allowed to be: the class
	// table governs ABSENCE, which for an opportunistic cell is normal
	// and silent forever. This is not absence — it is a declared action
	// of the control plane's own that did not complete, and the
	// alternative to paging is the operator discovering a fleet-less
	// morning at 09:00.
	KindWakeFailed Kind = "wake_failed"
)

// AllKinds is every kind this package understands, for config
// validation. Order is display order.
func AllKinds() []Kind {
	return []Kind{KindCellAbsent, KindFingerprint, KindDrainWithLease, KindModelDegraded, KindWakeFailed}
}

// DefaultAlarms is the class table's alarm column, verbatim, plus C14's
// undelivered-wake — the one alarm that is not about a cell's state at
// all but about a promise this control plane made. Changing this list
// changes what the fleet pages about; it is test-pinned.
func DefaultAlarms() []Kind {
	return []Kind{KindCellAbsent, KindFingerprint, KindDrainWithLease, KindWakeFailed}
}

// ParseKind resolves a configured kind name.
func ParseKind(s string) (Kind, error) {
	for _, k := range AllKinds() {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown alarm kind %q (want one of %s)", s, joinKinds(AllKinds()))
}

func joinKinds(ks []Kind) string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// Condition is one alarm condition the caller found true in this
// evaluation round. Scope identifies WHAT is alarming ("gpu-cell",
// "gpu-cell/bge-m3") and, with Kind, forms the dedup key: a re-worded
// Detail never re-notifies.
type Condition struct {
	Kind   Kind
	Scope  string
	Detail string
}

// Key is the notification identity.
func (c Condition) Key() string { return string(c.Kind) + "\x00" + c.Scope }

// DisplayKey is the human/JSON spelling of a key.
func DisplayKey(key string) string { return strings.ReplaceAll(key, "\x00", "/") }

// Notification states.
const (
	StateFiring   = "firing"
	StateResolved = "resolved"
	StateDigest   = "digest"
	StateExplicit = "explicit"
)

// Notification is one message to deliver.
type Notification struct {
	Key     string    `json:"key,omitempty"`
	Kind    Kind      `json:"kind,omitempty"`
	Scope   string    `json:"scope,omitempty"`
	State   string    `json:"state"`
	Title   string    `json:"title"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
	// Priority is ntfy's 1-5 scale; it rides the Priority header in text
	// format and the body in json format.
	Priority int `json:"priority,omitempty"`
}

// Policy is the tracker's configuration. The zero value is not usable;
// use DefaultPolicy and override.
type Policy struct {
	// Alarms is the enabled kind set. Conditions of other kinds are
	// dropped at the door, so a caller may always pass everything it can
	// see.
	Alarms []Kind
	// Dwell is how long a condition must be CONTINUOUSLY true before it
	// fires, per kind. Zero means fire on the first evaluation.
	Dwell map[Kind]time.Duration
	// ClearDwell is how long a fired condition must be continuously FALSE
	// before it resolves. Together with Dwell it is what makes a flapping
	// cell produce zero notifications rather than two per cycle.
	ClearDwell time.Duration
	// RatePerHour and Burst bound deliveries absolutely. Anything the
	// dwells let through is paced by this bucket; rate-limited
	// notifications are DEFERRED (bounded, dedup by key), never dropped —
	// a shredder on the storm path defeats the purpose.
	RatePerHour float64
	Burst       int
	// MaxDeferred bounds the deferral queue.
	MaxDeferred int
	// Resolve sends a notification when a fired alarm clears. This is
	// also the passive half of the futures list's "await-unblocked": the
	// thing you were told was broken telling you it is better.
	Resolve bool
}

// DefaultPolicy is the shipped policy: the alarm column, dwells sized to
// the failure each one describes, and a bucket that bounds the worst
// case at a dozen messages an hour.
func DefaultPolicy() Policy {
	return Policy{
		Alarms: DefaultAlarms(),
		Dwell: map[Kind]time.Duration{
			// Long enough to ride out a fleetd restart, a network blip and
			// one missed heartbeat; short enough that a real outage reaches
			// the phone before the operator has left the house.
			KindCellAbsent: 2 * time.Minute,
			// A def edit is expected to be followed by a cell re-render.
			// Fifteen minutes is "you forgot", not "you are mid-deploy".
			KindFingerprint: 15 * time.Minute,
			// None. The batch job is being truncated right now; a
			// two-minute-late page is a bereavement notice.
			KindDrainWithLease: 0,
			KindModelDegraded:  10 * time.Minute,
			// None. The wake grace window IS the dwell — the condition only
			// exists after fleetd has already waited the declared minutes
			// for the box to come back.
			KindWakeFailed: 0,
		},
		ClearDwell:  time.Minute,
		RatePerHour: 12,
		Burst:       4,
		MaxDeferred: 32,
		Resolve:     true,
	}
}

// dwellFor is the per-kind fire dwell (missing = fire immediately).
func (p Policy) dwellFor(k Kind) time.Duration { return p.Dwell[k] }

func (p Policy) enabled(k Kind) bool {
	for _, e := range p.Alarms {
		if e == k {
			return true
		}
	}
	return false
}

// alarm-state values.
const (
	statePending  = "pending"
	stateActive   = "active"
	stateClearing = "clearing"
)

type alarmState struct {
	kind    Kind
	scope   string
	detail  string
	state   string
	since   time.Time // when the current state began
	firstAt time.Time // when the condition first went true
	firedAt time.Time
}

// AlarmStatus is one alarm's status-surface row. Every alarm the tracker
// knows about appears here whether or not it was delivered — that is
// what keeps an away-suppressed alarm visible in fleet_status.
type AlarmStatus struct {
	Key    string `json:"key"`
	Kind   Kind   `json:"kind"`
	Scope  string `json:"scope"`
	State  string `json:"state"` // pending | active | clearing
	Detail string `json:"detail,omitempty"`
	// Since is when the CONDITION first went true, not when it last
	// changed machine state: "gpu-cell has been absent since 03:14" is
	// the sentence an operator needs, and a clearing alarm that flapped
	// back would otherwise reset it.
	Since   time.Time  `json:"since"`
	FiredAt *time.Time `json:"fired_at,omitempty"`
}

// Status is the tracker's half of the fleet_status notify block.
type Status struct {
	Alarms []AlarmStatus `json:"alarms,omitempty"`
	// Suppressed counts alarm notifications withheld because the fleet
	// scope is "away", by key. They are NOT lost: coming home sends one
	// digest naming them, and until then they are right here.
	Suppressed     int      `json:"suppressed,omitempty"`
	SuppressedKeys []string `json:"suppressed_keys,omitempty"`
	// Deferred counts notifications waiting on the rate bucket.
	Deferred int `json:"deferred,omitempty"`
}

// Tracker is the alarm state machine. Not safe for concurrent use; the
// caller runs it from one goroutine (the evaluation loop) and reads
// Status under its own lock.
type Tracker struct {
	pol    Policy
	alarms map[string]*alarmState

	tokens   float64
	lastFill time.Time

	deferred    []Notification
	deferredIdx map[string]int

	suppressed map[string]int
	wasAway    bool
}

// NewTracker builds a tracker. Unset Policy fields take DefaultPolicy's
// values, so a caller overriding one dwell does not silently disable the
// rate bucket.
func NewTracker(p Policy) *Tracker {
	d := DefaultPolicy()
	if len(p.Alarms) == 0 {
		p.Alarms = d.Alarms
	}
	// Merge per KIND, not per map: a caller overriding one dwell would
	// otherwise leave every other kind at zero, which means "fire on the
	// first evaluation" — turning a 15-minute persistence threshold into
	// an instant page by omission.
	if p.Dwell == nil {
		p.Dwell = map[Kind]time.Duration{}
	}
	for k, v := range d.Dwell {
		if _, set := p.Dwell[k]; !set {
			p.Dwell[k] = v
		}
	}
	if p.ClearDwell <= 0 {
		p.ClearDwell = d.ClearDwell
	}
	if p.RatePerHour <= 0 {
		p.RatePerHour = d.RatePerHour
	}
	if p.Burst <= 0 {
		p.Burst = d.Burst
	}
	if p.MaxDeferred <= 0 {
		p.MaxDeferred = d.MaxDeferred
	}
	return &Tracker{
		pol:         p,
		alarms:      map[string]*alarmState{},
		tokens:      float64(p.Burst),
		deferredIdx: map[string]int{},
		suppressed:  map[string]int{},
	}
}

// Policy returns the effective policy (defaults filled in).
func (t *Tracker) Policy() Policy { return t.pol }

// Step advances the state machine one evaluation round and returns the
// notifications to deliver, in key order so a caller's output is
// deterministic.
//
// away gates DELIVERY only: alarms fire, resolve and appear in Status
// exactly as they would at home. That is the difference between a
// vacation switch and a mute button, and it is why the return from away
// can name what was missed.
func (t *Tracker) Step(now time.Time, conds []Condition, away bool) []Notification {
	var out []Notification

	// The return from away is the first thing that happens, so the digest
	// precedes any alarm that fires in the same round.
	if t.wasAway && !away {
		if n, ok := t.digest(now); ok {
			out = append(out, n)
		}
	}
	t.wasAway = away

	live := make(map[string]Condition, len(conds))
	for _, c := range conds {
		if !t.pol.enabled(c.Kind) || c.Scope == "" {
			continue
		}
		live[c.Key()] = c
	}

	for key, c := range live {
		a := t.alarms[key]
		if a == nil {
			t.alarms[key] = &alarmState{
				kind: c.Kind, scope: c.Scope, detail: c.Detail,
				state: statePending, since: now, firstAt: now,
			}
			continue
		}
		a.detail = c.Detail
		if a.state == stateClearing {
			// Came back before the clear dwell expired: it never stopped
			// being the same alarm, so nothing is emitted in either
			// direction.
			a.state = stateActive
			a.since = now
		}
	}

	for key, a := range t.alarms {
		if _, still := live[key]; still {
			continue
		}
		switch a.state {
		case statePending:
			// Never fired, so nothing to resolve. This is the flap kill: a
			// condition that cannot stay true for its dwell produces no
			// notification at all, in either direction, forever.
			delete(t.alarms, key)
		case stateActive:
			a.state = stateClearing
			a.since = now
		}
	}

	var fire, resolve []string
	for key, a := range t.alarms {
		switch a.state {
		case statePending:
			if now.Sub(a.since) >= t.pol.dwellFor(a.kind) {
				a.state = stateActive
				a.since = now
				a.firedAt = now
				fire = append(fire, key)
			}
		case stateClearing:
			if now.Sub(a.since) >= t.pol.ClearDwell {
				resolve = append(resolve, key)
			}
		}
	}
	sort.Strings(fire)
	sort.Strings(resolve)

	for _, key := range fire {
		out = append(out, t.gate(now, firingNotification(now, t.alarms[key]), away)...)
	}
	for _, key := range resolve {
		a := t.alarms[key]
		delete(t.alarms, key)
		if t.pol.Resolve {
			out = append(out, t.gate(now, resolvedNotification(now, a), away)...)
		}
	}

	if !away {
		// The deferral queue is drained only at home. A notification that
		// the bucket held back before the operator left is still an alarm,
		// and delivering it mid-vacation would be the one form of noise
		// "away" exists to stop — the backlog waits for the return like
		// everything else.
		out = append(out, t.drainDeferred(now)...)
	}
	return out
}

// Explicit stamps an operator-requested message. It never touches the
// alarm state machine and is never gated by away: a human asking for a
// message right now is not an alarm, and the one command that proves the
// pager works must not be the one command that silently does nothing
// while you are away.
func Explicit(now time.Time, title, message string) Notification {
	return Notification{State: StateExplicit, Title: title, Message: message, At: stamp(now), Priority: 3}
}

// stamp normalises a timestamp for the wire. The caller is expected to
// hand Step a reading that still carries the monotonic clock — every
// dwell here is a Sub, and a wall-clock step must not move a threshold —
// so UTC is applied on the way OUT, once, where the value becomes a
// rendered field rather than an interval.
func stamp(t time.Time) time.Time { return t.UTC() }

func firingNotification(now time.Time, a *alarmState) Notification {
	return Notification{
		Key: string(a.kind) + "\x00" + a.scope, Kind: a.kind, Scope: a.scope,
		State: StateFiring, Title: "fleet: " + a.scope, Message: a.detail,
		At: stamp(now), Priority: 4,
	}
}

func resolvedNotification(now time.Time, a *alarmState) Notification {
	return Notification{
		Key: string(a.kind) + "\x00" + a.scope, Kind: a.kind, Scope: a.scope,
		State: StateResolved, Title: "fleet: " + a.scope + " resolved",
		Message: string(a.kind) + " cleared (" + a.detail + ")",
		At:      stamp(now), Priority: 3,
	}
}

// maxDigestKeys bounds the digest body; the rest are counted.
const maxDigestKeys = 10

// digest is the one message the return from away sends. Without it,
// "away" would be indistinguishable from a mute switch that ate the
// week's alarms.
func (t *Tracker) digest(now time.Time) (Notification, bool) {
	if len(t.suppressed) == 0 {
		return Notification{}, false
	}
	keys := make([]string, 0, len(t.suppressed))
	total := 0
	for k, n := range t.suppressed {
		keys = append(keys, k)
		total += n
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d alarm notifications were suppressed while away:", total)
	for i, k := range keys {
		if i == maxDigestKeys {
			fmt.Fprintf(&b, "\n… and %d more", len(keys)-maxDigestKeys)
			break
		}
		state := "cleared since"
		if a := t.alarms[k]; a != nil && a.state == stateActive {
			state = "STILL ACTIVE"
		}
		fmt.Fprintf(&b, "\n· %s ×%d (%s)", DisplayKey(k), t.suppressed[k], state)
	}
	t.suppressed = map[string]int{}
	return Notification{
		State: StateDigest, Title: "fleet: welcome back", Message: b.String(),
		At: stamp(now), Priority: 4,
	}, true
}

// gate applies the away suppression and the rate bucket. A suppressed
// alarm is counted (and stays in Status); a rate-limited one is deferred
// with last-write-wins per key.
func (t *Tracker) gate(now time.Time, n Notification, away bool) []Notification {
	if away && n.State != StateExplicit && n.State != StateDigest {
		t.suppressed[n.Key]++
		return nil
	}
	if !t.allow(now) {
		t.deferNotification(n)
		return nil
	}
	return []Notification{n}
}

func (t *Tracker) deferNotification(n Notification) {
	key := n.Key + "\x00" + n.State
	if i, ok := t.deferredIdx[key]; ok {
		t.deferred[i] = n
		return
	}
	if len(t.deferred) >= t.pol.MaxDeferred {
		// Drop the OLDEST: in a storm the newest state of the fleet is the
		// one worth a human's attention.
		old := t.deferred[0]
		t.deferred = t.deferred[1:]
		delete(t.deferredIdx, old.Key+"\x00"+old.State)
		for k, i := range t.deferredIdx {
			t.deferredIdx[k] = i - 1
		}
	}
	t.deferredIdx[key] = len(t.deferred)
	t.deferred = append(t.deferred, n)
}

func (t *Tracker) drainDeferred(now time.Time) []Notification {
	var out []Notification
	for len(t.deferred) > 0 && t.allow(now) {
		n := t.deferred[0]
		t.deferred = t.deferred[1:]
		delete(t.deferredIdx, n.Key+"\x00"+n.State)
		for k, i := range t.deferredIdx {
			t.deferredIdx[k] = i - 1
		}
		out = append(out, n)
	}
	return out
}

// allow is the token bucket. Refill is computed from elapsed wall time
// rather than a ticker so the tracker stays a pure function of the
// timestamps it is handed.
func (t *Tracker) allow(now time.Time) bool {
	if t.lastFill.IsZero() {
		t.lastFill = now
	}
	if elapsed := now.Sub(t.lastFill); elapsed > 0 {
		t.tokens += elapsed.Seconds() * t.pol.RatePerHour / 3600
		if t.tokens > float64(t.pol.Burst) {
			t.tokens = float64(t.pol.Burst)
		}
		t.lastFill = now
	}
	if t.tokens < 1 {
		return false
	}
	t.tokens--
	return true
}

// Status renders the tracker for fleet_status.
func (t *Tracker) Status() Status {
	st := Status{Deferred: len(t.deferred)}
	for key, a := range t.alarms {
		row := AlarmStatus{
			Key: DisplayKey(key), Kind: a.kind, Scope: a.scope,
			State: a.state, Detail: a.detail, Since: stamp(a.firstAt),
		}
		if !a.firedAt.IsZero() {
			f := stamp(a.firedAt)
			row.FiredAt = &f
		}
		st.Alarms = append(st.Alarms, row)
	}
	sort.Slice(st.Alarms, func(i, j int) bool { return st.Alarms[i].Key < st.Alarms[j].Key })
	for k, n := range t.suppressed {
		st.Suppressed += n
		st.SuppressedKeys = append(st.SuppressedKeys, DisplayKey(k))
	}
	sort.Strings(st.SuppressedKeys)
	return st
}
