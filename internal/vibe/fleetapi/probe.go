package fleetapi

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// Throughput probes, fleetd's half (fleet-control C8). fleetd does
// exactly two things with probes: it ASKS (here) and it DISPLAYS
// (fleetapi.go's snapshot). The measurement itself is the cell's, because
// only the cell can refuse to load a model it is asked to measure.
//
// Asking is fleetd's job for one reason: it is the only party that knows
// about leases and in-flight work fleet-wide, so it is the only party
// that can apply C4's eviction-fight guard. The request travels on the
// C3 piggyback command queue, which is what makes an announce-only cell
// with no inbound port probeable like any other.
//
// Nothing here ever acts on a verdict. A degraded model changes no
// render, no warm, no intent and no availability — see C8 §5.

// ProbeTarget is one declared periodic probe. Declared, never implicit:
// no entries means no probing at all, which is the default.
type ProbeTarget struct {
	Cell  string
	Model string
	Every time.Duration
}

// probeTargetState is the per-target status surface in fleet_status.
// Skips are first-class here: a guard that silently skips forever is the
// failure C5 spent a phase fixing.
type probeTargetState struct {
	Cell     string     `json:"cell"`
	Model    string     `json:"model"`
	EveryS   float64    `json:"every_s"`
	State    string     `json:"state"` // waiting | requested | skipped
	Detail   string     `json:"detail,omitempty"`
	LastAsk  *time.Time `json:"last_ask,omitempty"`
	NextDue  *time.Time `json:"next_due,omitempty"`
	LastSkip string     `json:"last_skip,omitempty"`
}

// probeStatus is the fleet_status block for the probe scheduler.
type probeStatus struct {
	Targets []probeTargetState `json:"targets,omitempty"`
	// Degraded lists every model any cell currently reports degraded —
	// the one-line answer to "is anything slow right now", without
	// walking the cell rows.
	Degraded []string `json:"degraded,omitempty"`
}

// StartProbeLoop launches the declared probe targets with production
// defaults (a 30s evaluation tick; the targets' own Every does the real
// pacing).
func (s *Server) StartProbeLoop(targets []ProbeTarget) {
	s.startProbeLoopWithTick(targets, 30*time.Second)
}

// startProbeLoopWithTick is the test seam.
func (s *Server) startProbeLoopWithTick(targets []ProbeTarget, tick time.Duration) {
	if tick <= 0 {
		tick = 30 * time.Second
	}
	for _, t := range targets {
		st := &probeTargetState{
			Cell:   t.Cell,
			Model:  t.Model,
			EveryS: t.Every.Seconds(),
			State:  "waiting",
			Detail: "watching",
		}
		s.mu.Lock()
		s.probeStates = append(s.probeStates, st)
		s.mu.Unlock()
		s.wg.Add(1)
		go s.probeTargetLoop(t, st, tick)
	}
}

func (s *Server) probeTargetLoop(t ProbeTarget, st *probeTargetState, tick time.Duration) {
	defer s.wg.Done()
	tk := time.NewTicker(tick)
	defer tk.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tk.C:
			s.evalProbeTarget(t, st)
		}
	}
}

// evalProbeTarget queues one probe when the target is due AND every
// guard passes. The guards are C4's, verbatim, including the rule a
// whole phase was written to restore: a guard that cannot be EVALUATED
// is a skip. Unknown in-flight is not zero in-flight.
func (s *Server) evalProbeTarget(t ProbeTarget, st *probeTargetState) {
	now := time.Now()
	// Dereferenced UNDER the lock, not after it. setProbeState replaces the
	// pointer rather than writing through it, so carrying the pointer out
	// happens to be safe today — and that is exactly the shape this repo
	// has already shipped a race in twice. The value costs nothing.
	s.mu.Lock()
	var last time.Time
	if st.LastAsk != nil {
		last = *st.LastAsk
	}
	s.mu.Unlock()
	if !last.IsZero() && now.Sub(last) < t.Every {
		return
	}

	if why, ok := s.probeGuard(t.Cell, t.Model); !ok {
		s.setProbeState(st, "skipped", why, false)
		return
	}
	if err := s.QueueCommand(t.Cell, AnnounceCommand{Verb: "probe", Model: t.Model}); err != nil {
		s.setProbeState(st, "skipped", "queue refused: "+err.Error(), false)
		return
	}
	s.setProbeState(st, "requested", "queued for the cell's next announce", true)
	s.setProbeNextDue(st, now.Add(t.Every))
	slog.Info("probe requested", "cell", t.Cell, "model", t.Model)
}

// probeGuard is the shared gate for both producers — the scheduler and
// the MCP verb — so an agent and the loop can never disagree about
// whether a box is busy. The reason string is the operator-facing
// explanation; it is never swallowed.
func (s *Server) probeGuard(cell, model string) (string, bool) {
	if cell == fleetcfg.FrontCell {
		// The front's rendered config is peers-only, so a probe there
		// measures a peer THROUGH the front — LAN, proxy hop and the peer's
		// queue folded into one unattributable number (C8 §1). The daemon's
		// config wiring and the MCP verb each refuse this too; it belongs
		// HERE as well, because "one shared guard so the producers cannot
		// drift" is only true of the rules the shared guard actually holds.
		return fmt.Sprintf("%s serves no models of its own; probe the cell that holds the model", fleetcfg.FrontCell), false
	}
	if in, ok := s.effectiveIntent(cell); ok && in.State == "drained" {
		return fmt.Sprintf("cell %s is drained", cell), false
	}
	p := s.presenceFor(cell)
	if p == nil || !p.Announcing {
		return fmt.Sprintf("cell %s has never announced; nothing would collect the probe", cell), false
	}
	if p.Stale || p.Withdrawn {
		return fmt.Sprintf("cell %s is stale/withdrawn", cell), false
	}
	// Residency, fleetd's copy: this is the cheap check that keeps the
	// command queue from filling with probes the cell will refuse. The
	// AUTHORITATIVE one is the cell's own /running read at issue time —
	// this one is up to one heartbeat stale by construction.
	resident := false
	for _, m := range p.Models {
		if m.ID == model {
			resident = m.State == "ready"
			break
		}
	}
	if !resident {
		return fmt.Sprintf("%s is not resident on %s — a probe must not load it; warm it first", model, cell), false
	}
	n, reported := s.InFlight(cell)
	switch {
	case !reported:
		return fmt.Sprintf("cell %s in-flight unknown — not spending GPU time blind", cell), false
	case n > 0:
		return fmt.Sprintf("cell %s has %d in-flight", cell, n), false
	}
	if leases := s.leasesForCellActive(cell); len(leases) > 0 {
		// Same lease guard, but a C11 hold names itself: a probe spends
		// real GPU time on a cell the operator declared hands-off, and the
		// refusal should say so and say for how long.
		if h, held := s.HoldOn(cell); held {
			return fmt.Sprintf("%s is %s", cell, holdDetail(h)), false
		}
		return fmt.Sprintf("%d active leases on %s", len(leases), cell), false
	}
	return "", true
}

// ProbeGuard is the exported gate for the MCP verb.
func (s *Server) ProbeGuard(cell, model string) (string, bool) { return s.probeGuard(cell, model) }

// LatestProbe returns what a cell last announced for one model, or nil.
// The MCP verb reports it beside the queued request so an agent gets an
// answer now and a fresher one on the next heartbeat.
func (s *Server) LatestProbe(cell, model string) *AnnounceProbe {
	p := s.presenceFor(cell)
	if p == nil {
		return nil
	}
	for _, m := range p.Models {
		if m.ID == model && m.Probe != nil {
			return CloneProbe(m.Probe)
		}
	}
	return nil
}

// attachProbes copies the announced probe block onto each model row of a
// cell snapshot. It runs after the row list is complete and it touches
// nothing else on the snapshot — the whole point of C8 §5 is that this
// is the ONLY field a verdict may reach.
func (s *Server) attachProbes(snap *CellSnapshot) {
	s.mu.Lock()
	p := s.presence[snap.Name]
	byID := map[string]*AnnounceProbe{}
	if p != nil {
		for _, m := range p.Models {
			if m.Probe != nil {
				byID[m.ID] = CloneProbe(m.Probe)
			}
		}
	}
	s.mu.Unlock()
	if len(byID) == 0 {
		return
	}
	for i := range snap.Models {
		if pr, ok := byID[snap.Models[i].ID]; ok {
			snap.Models[i].Probe = pr
		}
	}
}

// setProbeNextDue publishes when this target will next be asked. A
// declared schedule whose next fire is invisible is how a wrong interval
// hides (C4 §2's rule for warm schedules, same reason).
func (s *Server) setProbeNextDue(st *probeTargetState, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := at.UTC()
	st.NextDue = &t
}

func (s *Server) setProbeState(st *probeTargetState, state, detail string, asked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.State = state
	st.Detail = detail
	if !asked {
		st.LastSkip = detail
		return
	}
	now := time.Now()
	st.LastAsk = &now
	st.LastSkip = ""
}

// probeReport builds the fleet_status block, copying so a reader never
// holds loop-owned pointers.
func (s *Server) probeReport() *probeStatus {
	s.mu.Lock()
	states := make([]probeTargetState, 0, len(s.probeStates))
	for _, st := range s.probeStates {
		states = append(states, cloneProbeTargetState(st))
	}
	var degraded []string
	for cell, p := range s.presence {
		// Degraded answers "is anything slow RIGHT NOW", so it reads only
		// FRESH announces. A stale or withdrawn cell's last verdict is
		// history — C6's rule that staleness retires the announce as
		// evidence, applied to the one field that is pure evidence. The
		// model row keeps the verdict either way; this is the roll-up.
		if p == nil || !p.Announcing || p.Stale || p.Withdrawn {
			continue
		}
		for _, m := range p.Models {
			if m.Probe != nil && m.Probe.Verdict == VerdictDegraded {
				degraded = append(degraded, cell+"/"+m.ID)
			}
		}
	}
	s.mu.Unlock()
	if len(states) == 0 && len(degraded) == 0 {
		return nil
	}
	slices.Sort(degraded)
	return &probeStatus{Targets: states, Degraded: degraded}
}

// cloneProbeTargetState deep-copies the two *time.Time fields. A struct
// copy carries the pointers out of the lock that owns them — the same
// shape CloneProbe exists for, one struct over.
func cloneProbeTargetState(st *probeTargetState) probeTargetState {
	cp := *st
	if st.LastAsk != nil {
		t := *st.LastAsk
		cp.LastAsk = &t
	}
	if st.NextDue != nil {
		t := *st.NextDue
		cp.NextDue = &t
	}
	return cp
}
