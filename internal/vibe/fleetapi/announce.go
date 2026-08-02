package fleetapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// The announce protocol (fleet-control C3 §1): cells dial OUT and say
// what they serve; fleetd's presence table becomes the availability
// source, and the announce RESPONSE carries desired intent back. Two
// schema rules are load-bearing forever: "v": 1 is required (mixed-version
// announcers are guaranteed — the laptop updates when docked, the heavy
// cell quarterly), and receivers tolerate unknown fields (additive v2
// never breaks v1 readers).

// AnnounceVersion is the only protocol version this fleetd speaks.
const AnnounceVersion = 1

// DefaultAnnounceIntervalS is the cadence fleetd hands announcers when
// nothing else is configured.
const DefaultAnnounceIntervalS = 15

// AnnounceIntent is the cell's echoed intent (axis 2 truth: the box
// you're standing at is always right) or fleetd's desired-intent
// request in the response. Since disambiguates which side changed
// last — newer wins, ties go to the cell.
type AnnounceIntent struct {
	State string    `json:"state"` // "serving" | "drained" | "withdrawing"
	Since time.Time `json:"since,omitempty"`
}

// AnnounceModel is one served model in an announce.
type AnnounceModel struct {
	ID          string `json:"id"`
	State       string `json:"state"` // llama-swap's states mapped through
	FlagsSHA256 string `json:"flags_sha256,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"` // "strict" | "advisory" (default)
	Probe       any    `json:"probe"`                 // reserved (v2 throughput block); null in v1
}

// AnnounceCapacity is the cell's free resources (advisory display data).
type AnnounceCapacity struct {
	VRAMTotalGB float64 `json:"vram_total_gb,omitempty"`
	VRAMFreeGB  float64 `json:"vram_free_gb,omitempty"`
	DiskFreeGB  float64 `json:"disk_free_gb,omitempty"`
}

// AnnounceVersions feeds the version matrix in fleet_status: a
// fingerprint mismatch report becomes "cell is N commits behind"
// instead of a 2 a.m. mystery.
type AnnounceVersions struct {
	LlamaSwap string `json:"llama_swap,omitempty"`
	Vibe      string `json:"vibe,omitempty"`
	DefsSHA   string `json:"defs_sha,omitempty"`
	DefsDirty bool   `json:"defs_dirty,omitempty"`
}

// AnnounceRequest is the cell→fleetd heartbeat.
type AnnounceRequest struct {
	V        int               `json:"v"`
	Cell     string            `json:"cell"`
	Seq      uint64            `json:"seq"`
	Intent   *AnnounceIntent   `json:"intent,omitempty"`
	Models   []AnnounceModel   `json:"models,omitempty"`
	Capacity *AnnounceCapacity `json:"capacity,omitempty"`
	Versions *AnnounceVersions `json:"versions,omitempty"`
}

// AnnounceCommand is one piggybacked verb the cell should execute and
// reflect in its next announce (verbs whose latency doesn't matter
// after C3; interactive paths still use daemon_url when present).
type AnnounceCommand struct {
	Verb  string `json:"verb"` // "unload" | "warm"
	Model string `json:"model"`
}

// AnnounceResponse is fleetd's answer: cadence, desired intent (a
// REQUEST until the cell echoes it — the conflict rule), and queued
// commands.
type AnnounceResponse struct {
	IntervalS     int               `json:"interval_s"`
	DesiredIntent *Intent           `json:"desired_intent,omitempty"`
	Commands      []AnnounceCommand `json:"commands,omitempty"`
}

// Presence is one cell's announce-derived state in the table.
type Presence struct {
	Cell          string            `json:"cell"`
	Seq           uint64            `json:"seq"`
	Announcing    bool              `json:"announcing"` // at least one announce ever
	Stale         bool              `json:"stale"`      // past stale_after
	Withdrawn     bool              `json:"withdrawn"`  // clean withdraw (intent.state == withdrawing)
	IntentEcho    *AnnounceIntent   `json:"intent_echo,omitempty"`
	Models        []AnnounceModel   `json:"models,omitempty"`
	Capacity      *AnnounceCapacity `json:"capacity,omitempty"`
	Versions      *AnnounceVersions `json:"versions,omitempty"`
	ReceivedAt    time.Time         `json:"received_at"`
	IntervalS     int               `json:"interval_s"`
	HealthyStreak int               `json:"healthy_streak"` // consecutive fresh announces (render hysteresis)
}

// Announce events published on /api/fleet/events.
const (
	EventCellStale     = "fleet.cellStale"
	EventCellWithdrawn = "fleet.cellWithdrawn"
	EventCellReturned  = "fleet.cellReturned"
	EventFingerprint   = "fleet.fingerprintMismatch"
)

// staleAfter derives the staleness bound: 3× the announced interval
// plus jitter allowance (~50s at the default cadence). Computed ONLY
// from fleetd-side received_at — seq is a per-boot hint and cell clocks
// are never consulted, which retires clock skew as a failure class.
func staleAfter(intervalS int) time.Duration {
	if intervalS <= 0 {
		intervalS = DefaultAnnounceIntervalS
	}
	return time.Duration(3*intervalS)*time.Second + 5*time.Second
}

// handleAnnounce serves POST /api/fleet/announce.
func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	var req AnnounceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.V != AnnounceVersion {
		http.Error(w, fmt.Sprintf("unsupported announce version %d (this fleetd speaks v=%d)", req.V, AnnounceVersion), http.StatusBadRequest)
		return
	}
	known := false
	for _, c := range s.cells {
		if c.Name == req.Cell {
			known = true
			break
		}
	}
	if !known {
		http.Error(w, fmt.Sprintf("unknown cell %q (not in the registry)", req.Cell), http.StatusBadRequest)
		return
	}
	if err := validateAnnounce(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clampEchoClock(&req)

	resp := s.recordAnnounce(&req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// maxAnnounceFieldLen bounds announce-supplied strings; they land in
// fleet_status (human and agent contexts alike).
const maxAnnounceFieldLen = 256

// validateAnnounce enforces enum values and field hygiene (length,
// control characters) on the parts of an announce that flow into
// status surfaces. Unknown FIELDS stay tolerated (the version-skew
// rule); unknown VALUES of defined fields are rejected.
func validateAnnounce(req *AnnounceRequest) error {
	clean := func(label, v string) error {
		if len(v) > maxAnnounceFieldLen {
			return fmt.Errorf("%s exceeds %d bytes", label, maxAnnounceFieldLen)
		}
		for _, r := range v {
			if !unicode.IsPrint(r) {
				return fmt.Errorf("%s contains a control character", label)
			}
		}
		return nil
	}
	if req.Intent != nil {
		switch req.Intent.State {
		case "serving", "drained", "withdrawing":
		default:
			return fmt.Errorf("intent.state %q must be one of serving, drained, withdrawing", req.Intent.State)
		}
	}
	for _, m := range req.Models {
		if err := clean("models[].id", m.ID); err != nil {
			return err
		}
		if err := clean("models[].state", m.State); err != nil {
			return err
		}
	}
	if req.Versions != nil {
		for label, v := range map[string]string{
			"versions.llama_swap": req.Versions.LlamaSwap,
			"versions.vibe":       req.Versions.Vibe,
			"versions.defs_sha":   req.Versions.DefsSHA,
		} {
			if err := clean(label, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// echoFutureSkew bounds how far ahead of fleetd's clock an echo's
// timestamp may be. The announce client stamps time.Now(), so a small
// allowance covers jitter; a skewed or forged clock (year-2999) gets
// CLAMPED to now, preserving reconciliation without letting a cell
// timestamp cancel requests from the future.
const echoFutureSkew = 2 * time.Minute

// clampEchoClock caps echo.Since at now+echoFutureSkew. Everything else
// in the protocol is fleetd-clock-only (staleness); this is the one
// place a cell clock is consulted, so it's the one place it's bounded.
func clampEchoClock(req *AnnounceRequest) {
	if req.Intent == nil || req.Intent.Since.IsZero() {
		return
	}
	if max := time.Now().UTC().Add(echoFutureSkew); req.Intent.Since.After(max) {
		req.Intent.Since = max
	}
}

// recordAnnounce upserts the presence entry, fires transition events,
// and builds the response (desired intent + queued commands). Split
// from the handler so tests drive it directly.
func (s *Server) recordAnnounce(req *AnnounceRequest) *AnnounceResponse {
	now := time.Now().UTC()
	intervalS := DefaultAnnounceIntervalS

	s.mu.Lock()
	p := s.presence[req.Cell]
	if p == nil {
		p = &Presence{Cell: req.Cell}
		s.presence[req.Cell] = p
	}

	firstEver := !p.Announcing
	wasStaleOrWithdrawn := p.Stale || p.Withdrawn
	wasWithdrawing := p.Withdrawn
	p.Announcing = true
	p.Seq = req.Seq
	p.IntervalS = intervalS
	p.ReceivedAt = now
	p.Stale = false
	if req.Intent != nil {
		p.IntentEcho = req.Intent
		p.Withdrawn = req.Intent.State == "withdrawing"
	} else {
		p.IntentEcho = &AnnounceIntent{State: "serving"}
		p.Withdrawn = false
	}
	// The healthy streak counts consecutive FRESH serving announces — a
	// withdrawing heartbeat isn't one (re-add hysteresis, C3 §4).
	if p.Withdrawn {
		p.HealthyStreak = 0
	} else {
		p.HealthyStreak++
	}
	p.Models = req.Models
	p.Capacity = req.Capacity
	p.Versions = req.Versions

	// Transition-gated events: named transitions fire on transitions,
	// not on every steady-state heartbeat.
	var events []Event
	switch {
	case firstEver:
		events = append(events, Event{Cell: req.Cell, Type: EventCellReturned})
	case p.Withdrawn && wasWithdrawing:
		// Steady-state withdraw: no new transition, no event drip.
	case p.Withdrawn:
		events = append(events, Event{Cell: req.Cell, Type: EventCellWithdrawn})
	case wasStaleOrWithdrawn:
		events = append(events, Event{Cell: req.Cell, Type: EventCellReturned})
	}
	s.mu.Unlock()

	// The conflict rule, registry side (intentMu orders this against
	// concurrent SetIntent calls — mu stays the leaf): a NEWER echo
	// resolves the request either way — the cell complied (echo matches
	// the request) or the human at the box overrode it (echo diverges).
	// Only an older echo hands the request back as desired_intent.
	var desired *Intent
	var intentSnapshot map[string]Intent
	s.intentMu.Lock()
	s.mu.Lock()
	req2, hasRequest := s.intents[req.Cell]
	echo := p.IntentEcho
	if hasRequest && echo != nil && !echo.Since.IsZero() && req2.Since.Before(echo.Since) {
		delete(s.intents, req.Cell)
		intentSnapshot = make(map[string]Intent, len(s.intents))
		for k, v := range s.intents {
			intentSnapshot[k] = v
		}
	} else if hasRequest {
		d := req2
		desired = &d
	}
	s.mu.Unlock()
	if intentSnapshot != nil {
		if err := saveIntents(s.intentPath, intentSnapshot); err != nil {
			slog.Warn("intent persist failed (stale-request drop)", "err", err)
		}
	}
	s.intentMu.Unlock()

	// Presence makes last_seen truth: a fresh announce IS a sighting.
	s.mu.Lock()
	s.lastSeen[req.Cell] = now
	s.mu.Unlock()

	for _, ev := range events {
		s.publish(ev)
	}

	cmds := s.drainCommands(req.Cell)
	resp := &AnnounceResponse{IntervalS: intervalS, DesiredIntent: desired, Commands: cmds}

	if p.Withdrawn || firstEver || wasStaleOrWithdrawn {
		s.noteRenderTrigger(req.Cell)
	} else if s.cellClass(req.Cell) == string(fleetcfg.ClassRoaming) {
		// The hysteresis-clearing announce (the Mth consecutive fresh one
		// after a prune) matches none of the transition cases above —
		// but it's exactly what re-adds a roaming cell to the render.
		// Only roaming cells are prunable, so only they need re-add
		// triggers; the render loop's coalescing makes the rest free.
		s.noteRenderTrigger(req.Cell)
	}
	return resp
}

// cellClass resolves a cell's class from the registry (empty when unset).
func (s *Server) cellClass(name string) string {
	for _, c := range s.cells {
		if c.Name == name {
			return c.Class
		}
	}
	return ""
}

// drainCommands returns (and clears) the queued piggyback commands for
// a cell — verbs fleetd couldn't deliver interactively.
func (s *Server) drainCommands(cell string) []AnnounceCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmds := s.commands[cell]
	delete(s.commands, cell)
	return cmds
}

// maxQueuedCommands bounds the per-cell command queue; beyond it the
// oldest drop off with a log (a cell that never announces must not
// grow memory, and any future producer must validate against the def
// set rather than trust this bound).
const maxQueuedCommands = 64

// queueCommand piggybacks a verb for the cell's next announce. Used
// when the cell can't be reached interactively (after C3, daemon_url
// is an optimization, not a requirement).
func (s *Server) queueCommand(cell string, cmd AnnounceCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.commands[cell]
	if len(q) >= maxQueuedCommands {
		slog.Warn("command queue full; dropping oldest", "cell", cell)
		q = q[1:]
	}
	s.commands[cell] = append(q, cmd)
}

// presenceSnapshot returns a copy of the presence table (read-side,
// for /api/fleet/state and the render loop).
func (s *Server) presenceSnapshot() map[string]Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Presence, len(s.presence))
	for k, v := range s.presence {
		out[k] = *v
	}
	return out
}

// PresenceFor returns one cell's presence, or nil when the cell has
// never announced.
func (s *Server) PresenceFor(cell string) *Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.presence[cell]; p != nil {
		cp := *p
		return &cp
	}
	return nil
}

// presenceFor is the in-package spelling of PresenceFor.
func (s *Server) presenceFor(cell string) *Presence { return s.PresenceFor(cell) }

// stalenessLoop marks cells stale when their announce gap exceeds
// staleAfter, publishing fleet.cellStale. Probe-based availability
// keeps running underneath — presence wins while fresh, probes are the
// fallback for non-announcing cells.
func (s *Server) stalenessLoop() {
	s.wg.Add(1)
	defer s.wg.Done()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-tick.C:
			var staleCells []string
			s.mu.Lock()
			for name, p := range s.presence {
				if p.Announcing && !p.Stale && !p.Withdrawn && now.Sub(p.ReceivedAt) > staleAfter(p.IntervalS) {
					p.Stale = true
					p.HealthyStreak = 0
					staleCells = append(staleCells, name)
				}
			}
			s.mu.Unlock()
			for _, name := range staleCells {
				slog.Info("cell announce went stale", "cell", name)
				s.publish(Event{Cell: name, Type: EventCellStale})
				s.noteRenderTrigger(name)
			}
		}
	}
}

// noteRenderTrigger flags a membership transition the presence-derived
// render loop (render_loop.go) coalesces into a re-render.
func (s *Server) noteRenderTrigger(cell string) {
	select {
	case s.renderTrigger <- cell:
	default:
		// A pending trigger already covers this cell — coalescing is
		// the point (flap storms render at most the cap).
	}
}
