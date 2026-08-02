package fleetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Warm targets (fleet-control C4 §1): restore a cell's default model
// after the operator's swap goes request-idle — keyed on the swapped-in
// model's activity, NEVER on a clock (the rejected pin/keep-warm
// alternative re-warms the default while the operator's model is in use
// and evicts it mid-session; that stays unbuilt).
//
// The policy keys on evidence: residency from presence/probes, idleness
// from the inflight SSE stream. A cell that's drained, stale, or absent
// is skipped silently (noted in fleet_status) — fleetd policy layered
// over llama-swap's TTL, never a fight with it.

// WarmTarget is one restore-after-idle policy entry.
type WarmTarget struct {
	Cell             string
	Model            string
	RestoreAfterIdle time.Duration
}

// warmTargetState is the per-target status surface in fleet_status.
type warmTargetState struct {
	Cell              string     `json:"cell"`
	Model             string     `json:"model"`
	State             string     `json:"state"` // holding | waiting | restoring | skipped
	Detail            string     `json:"detail,omitempty"`
	RestoreAfterIdleS float64    `json:"restore_after_idle_s"`
	LastRestore       *time.Time `json:"last_restore,omitempty"`

	// emptySince marks the first consecutive nothing-resident
	// observation. Presence is heartbeat-stale: a swap mid-cold-start
	// reads as "nothing resident" for up to an announce interval, and
	// restoring into that window warms the target EARLY (both models
	// instead of swap-then-restore — seen live twice). The
	// empty-restore only fires when the emptiness has persisted for the
	// full grace window (≥ one announce interval).
	emptySince time.Time
}

// warmLoopConfig wires the warm loops. Nil WarmFn uses the front's
// 1-token chat completion (JIT is the start verb).
type warmLoopConfig struct {
	targets   []WarmTarget
	frontURL  string
	warmFn    func(ctx context.Context, frontURL, model string) error
	tick      time.Duration
	idleFloor time.Duration // last-activity unknown → idle since this long ago (fleetd start)
	// emptyGrace is the time nothing-resident must persist before the
	// empty-restore fires (default 30s ≈ two announce intervals).
	emptyGrace time.Duration
}

// StartWarmLoop launches the warm-target policy with production
// defaults (front 1-token warms, 15s eval tick).
func (s *Server) StartWarmLoop(targets []WarmTarget, frontURL string) {
	s.startWarmLoopWithConfig(warmLoopConfig{targets: targets, frontURL: frontURL})
}

// startWarmLoopWithConfig is the full seam (tests inject tick + warmFn).
func (s *Server) startWarmLoopWithConfig(cfg warmLoopConfig) {
	if cfg.tick <= 0 {
		cfg.tick = 15 * time.Second
	}
	if cfg.warmFn == nil {
		cfg.warmFn = warmViaFront
	}
	for _, t := range cfg.targets {
		st := &warmTargetState{
			Cell:              t.Cell,
			Model:             t.Model,
			State:             "waiting",
			Detail:            "watching",
			RestoreAfterIdleS: t.RestoreAfterIdle.Seconds(),
		}
		s.mu.Lock()
		s.warmStates = append(s.warmStates, st)
		s.mu.Unlock()
		s.wg.Add(1)
		go s.warmTargetLoop(t, st, cfg)
	}
}

// warmTargetLoop evaluates one target per tick. One goroutine per
// target keeps states independent; all exit on s.done.
func (s *Server) warmTargetLoop(t WarmTarget, st *warmTargetState, cfg warmLoopConfig) {
	defer s.wg.Done()
	tick := time.NewTicker(cfg.tick)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			s.evalWarmTarget(t, st, cfg)
		}
	}
}

// evalWarmTarget applies the policy once. Residency comes from
// presence when the cell announces (the no-inbound-port case), probes
// otherwise.
func (s *Server) evalWarmTarget(t WarmTarget, st *warmTargetState, cfg warmLoopConfig) {
	if p := s.presenceFor(t.Cell); p != nil && p.Announcing {
		if p.Stale || p.Withdrawn {
			s.setWarmState(st, "skipped", "cell stale/withdrawn")
			return
		}
		snap := CellSnapshot{}
		for _, m := range p.Models {
			snap.Models = append(snap.Models, ModelState{ID: m.ID, State: m.State})
		}
		s.applyWarmEval(t, st, cfg, snap)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	snap := s.snapshotCell(ctx, Cell{Name: t.Cell, URL: s.cellURL(t.Cell)})
	cancel()
	if !snap.Reachable {
		s.setWarmState(st, "skipped", "cell unreachable")
		return
	}
	s.applyWarmEval(t, st, cfg, snap)
}

// applyWarmEval runs the policy against a fresh cell snapshot,
// returning true when the evaluation completed (vs. skip for absence).
func (s *Server) applyWarmEval(t WarmTarget, st *warmTargetState, cfg warmLoopConfig, snap CellSnapshot) bool {
	var targetResident bool
	var residents []string
	for _, m := range snap.Models {
		if m.State == "stopped" || m.State == "" {
			continue
		}
		residents = append(residents, m.ID)
		if m.ID == t.Model {
			targetResident = true
		}
	}
	switch {
	case targetResident:
		s.mu.Lock()
		st.emptySince = time.Time{}
		s.mu.Unlock()
		s.setWarmState(st, "holding", "target resident")
		return true
	case len(residents) == 0:
		s.mu.Lock()
		if st.emptySince.IsZero() {
			st.emptySince = time.Now().UTC()
		}
		emptyFor := time.Since(st.emptySince)
		s.mu.Unlock()
		grace := cfg.emptyGrace
		if grace <= 0 {
			grace = 30 * time.Second
		}
		if emptyFor < grace {
			s.setWarmState(st, "waiting", "nothing resident (confirming)")
			return true
		}
		s.mu.Lock()
		st.emptySince = time.Time{}
		s.mu.Unlock()
		s.restore(t, st, cfg, "nothing resident")
		return true
	default:
		s.mu.Lock()
		st.emptySince = time.Time{}
		s.mu.Unlock()
		// An operator swap is resident: restore only after EVERY
		// resident has been request-idle for the window. Any request to
		// any of them resets it.
		idle, idleFor := s.swapIdleFor(t.Cell, residents, cfg)
		if idle >= t.RestoreAfterIdle {
			s.restore(t, st, cfg, fmt.Sprintf("swap idle %s", idleFor))
			return true
		}
		s.setWarmState(st, "waiting", fmt.Sprintf("swap %s active (idle %s of %s)", strings.Join(residents, ","), idleFor, t.RestoreAfterIdle))
		return true
	}
}

// swapIdleFor reports the OLDEST still-idle window across the resident
// models and how long that is — restore only fires when the quietest
// resident has been quiet for the whole window.
func (s *Server) swapIdleFor(cell string, residents []string, cfg warmLoopConfig) (time.Duration, string) {
	now := time.Now().UTC()
	idleFloor := cfg.idleFloor
	if idleFloor == 0 {
		idleFloor = time.Hour
	}
	oldest := idleFloor
	for _, id := range residents {
		last, ok := s.modelLastActivity(cell, id)
		var idle time.Duration
		if !ok {
			// No inflight frame ever mentioned it: idle since fleetd
			// start — bounded, so a long-idle swap eventually restores
			// rather than pinning forever on missing data.
			idle = idleFloor
		} else {
			idle = now.Sub(last)
		}
		if idle < oldest {
			oldest = idle
		}
	}
	return oldest, oldest.Round(time.Second).String()
}

// restore fires the warm and marks the state.
func (s *Server) restore(t WarmTarget, st *warmTargetState, cfg warmLoopConfig, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := cfg.warmFn(ctx, cfg.frontURL, t.Model); err != nil {
		s.setWarmState(st, "waiting", "warm failed: "+err.Error())
		slog.Warn("warm-target restore failed", "cell", t.Cell, "model", t.Model, "err", err)
		return
	}
	slog.Info("warm-target restored", "cell", t.Cell, "model", t.Model, "why", why)
	s.mu.Lock()
	st.State = "holding"
	st.Detail = "restored (" + why + ")"
	now := time.Now().UTC()
	st.LastRestore = &now
	s.mu.Unlock()
}

func (s *Server) setWarmState(st *warmTargetState, state, detail string) {
	s.mu.Lock()
	st.State = state
	st.Detail = detail
	s.mu.Unlock()
}

func (s *Server) cellURL(name string) string {
	for _, c := range s.cells {
		if c.Name == name {
			return c.URL
		}
	}
	return ""
}

// warmViaFront is the default warm: a 1-token chat completion through
// the front — JIT is the start verb.
func warmViaFront(ctx context.Context, frontURL, model string) error {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "warm"}},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(frontURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("warm through front: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// warmStatus is the fleet_status surface for the warm loops.
type warmStatus struct {
	Targets  []warmTargetState   `json:"targets,omitempty"`
	Schedule []warmScheduleState `json:"schedule,omitempty"`
}

// warmReport builds the status block, copying so a reader never holds
// loop-owned pointers.
func (s *Server) warmReport() *warmStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.warmStates) == 0 && len(s.schedStates) == 0 {
		return nil
	}
	rep := &warmStatus{}
	for _, st := range s.warmStates {
		rep.Targets = append(rep.Targets, *st)
	}
	for _, st := range s.schedStates {
		rep.Schedule = append(rep.Schedule, *st)
	}
	return rep
}
