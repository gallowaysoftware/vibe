package fleetapi

import (
	"fmt"
	"time"
)

// Cell request-activity evidence (fleet-control C10): the read side of
// the same inflight fold C4's warm targets key their idle windows on
// (watcher.go:trackInFlight), aggregated per CELL rather than per model
// and published on the state snapshot so `vibe cell await --idle`, the
// page and an MCP agent all read one derivation.
//
// The rule this block exists to keep is C4's, and it is the whole
// design: fleetd may never claim silence it was not connected to
// observe. Absence of an observation channel is reported as absence of
// EVIDENCE — never as idleness — because the consumer of "idle" is a
// batch job about to take the GPU.

// CellActivity is the evidence, not a verdict: a consumer decides what
// window it needs. Every field is nil-able where the honest answer is
// "unknown", and Reason qualifies whatever the numbers say.
type CellActivity struct {
	// Observed reports whether fleetd's /api/events stream to the cell is
	// connected RIGHT NOW. C4's observesActivity also accepts "an inflight
	// frame was seen once, ever", which is sticky forever; a wait that is
	// about to release a batch job needs the channel to be live, not to
	// have existed.
	Observed bool `json:"observed"`
	// ObservedSince is when that stream connected. It is the floor on the
	// idle window: a watcher that reconnected 8s ago can prove 8s of
	// silence and no more, whatever the frame history looks like.
	ObservedSince *time.Time `json:"observed_since,omitempty"`
	// InFlight is the cell's last REPORTED in-flight count. nil means no
	// frame has ever arrived — unreported is not zero (C4/C8's rule).
	InFlight *int `json:"in_flight,omitempty"`
	// LastRequest is the last inflight frame from the cell. Frames are
	// edges (add or remove), so either one is request activity.
	LastRequest *time.Time `json:"last_request,omitempty"`
	// IdleSeconds is the request-silence window fleetd can actually
	// vouch for. nil means it cannot be computed; Reason says why.
	IdleSeconds *float64 `json:"idle_s,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// activityFor derives one cell's activity block. Cheap and lock-local:
// it runs inside the per-cell snapshot goroutine like decorate.
func (s *Server) activityFor(cell string) *CellActivity {
	now := time.Now()
	s.mu.Lock()
	up := s.cellUp[cell]
	since, haveSince := s.cellUpSince[cell]
	last, haveLast := s.lastInFlightFrame[cell]
	count, reported := s.inFlight[cell], s.inFlightSeen[cell]
	s.mu.Unlock()

	act := &CellActivity{Observed: up && haveSince}
	if haveLast {
		t := last.UTC()
		act.LastRequest = &t
	}
	if !act.Observed {
		// The one branch that must never be mistaken for "idle": no live
		// stream means fleetd is not watching, and a batch gate reading
		// silence here would be reading its own blindness.
		act.Reason = fmt.Sprintf("no live event stream to %s — fleetd is not watching it, so silence is not evidence of idleness", cell)
		return act
	}
	t := since.UTC()
	act.ObservedSince = &t
	if reported {
		c := count
		act.InFlight = &c
	}
	if reported && count > 0 {
		if haveLast && last.Before(since) {
			// The count survives a reconnect; the knowledge does not. A
			// non-zero count last stamped BEFORE the current connection is
			// not a claim about now — the remove edge that closed it out
			// arrived while fleetd was disconnected, or the request is
			// still running, and nothing here can tell those apart. Report
			// it as the absent evidence it is (no window) rather than as a
			// live busy count: the first is a refusal a consumer can read
			// and act on, the second is a false statement of fact that
			// also never resolves.
			act.Reason = fmt.Sprintf("%s's last inflight frame (%d in flight) predates the current stream connection — that count is not evidence about now, and no frame has arrived since fleetd reconnected", cell, count)
			return act
		}
		zero := 0.0
		act.IdleSeconds = &zero
		act.Reason = fmt.Sprintf("%d request(s) in flight", count)
		return act
	}
	floor := since
	if haveLast && last.After(floor) {
		floor = last
	}
	idle := now.Sub(floor)
	if idle < 0 {
		idle = 0
	}
	secs := idle.Seconds()
	act.IdleSeconds = &secs
	if !reported {
		act.Reason = "no inflight frame seen yet; idle measured from the stream connect"
	}
	return act
}
