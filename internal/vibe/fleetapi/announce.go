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

// maxAnnounceFieldLen bounds off-box strings; they land in fleet_status
// (human and agent contexts alike).
const maxAnnounceFieldLen = 256

// clean rejects an off-box string that is oversized or carries control
// characters. Shared by every ingest that feeds a status surface — the
// announce endpoint sanitised exactly this class of data while the lease
// endpoint, whose entries print in the same tables, did not.
func clean(label, v string) error {
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

// validateAnnounce enforces enum values and field hygiene (length,
// control characters) on the parts of an announce that flow into
// status surfaces. Unknown FIELDS stay tolerated (the version-skew
// rule); unknown VALUES of defined fields are rejected.
func validateAnnounce(req *AnnounceRequest) error {
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
	prevModels := p.Models
	p.Models = req.Models
	p.Capacity = req.Capacity
	p.Versions = req.Versions

	// A model-set change IS a membership transition (C3 §4): the catalog
	// derives from presence, so a cell that starts or stops serving a
	// model triggers a re-render exactly like a cell that left or
	// returned.
	modelChanged := modelSetChanged(prevModels, req.Models)
	// Fingerprint drift with a stable id set is NOT a membership change,
	// but enforcement runs ONLY inside a render pass — so without this
	// second trigger a strict mismatch on an always_on or opportunistic
	// cell (exactly where strict embed defs live) raised nothing until
	// some unrelated transition happened, against the design's "a
	// mismatch always raises a loud event". Two independent reasons to
	// re-render, one trigger.
	fingerprintChanged := modelFingerprintChanged(prevModels, req.Models)
	withdrawn := p.Withdrawn
	s.mu.Unlock()
	if modelChanged || fingerprintChanged {
		s.noteRenderTrigger(req.Cell)
	}

	// Transition-gated events: named transitions fire on transitions,
	// not on every steady-state heartbeat.
	var events []Event
	switch {
	case firstEver:
		events = append(events, Event{Cell: req.Cell, Type: EventCellReturned})
	case withdrawn && wasWithdrawing:
		// Steady-state withdraw: no new transition, no event drip.
	case withdrawn:
		events = append(events, Event{Cell: req.Cell, Type: EventCellWithdrawn})
	case wasStaleOrWithdrawn:
		events = append(events, Event{Cell: req.Cell, Type: EventCellReturned})
	}

	// The conflict rule, registry side (intentMu orders this against
	// concurrent SetIntent calls — mu stays the leaf): a NEWER echo
	// resolves the request either way — the cell complied (echo matches
	// the request) or the human at the box overrode it (echo diverges).
	// Only an older echo hands the request back as desired_intent.
	var desired *Intent
	var next map[string]Intent
	s.intentMu.Lock()
	s.mu.Lock()
	req2, hasRequest := s.intents[req.Cell]
	echo := p.IntentEcho
	if hasRequest && echo != nil && !echo.Since.IsZero() && req2.Since.Before(echo.Since) {
		next = make(map[string]Intent, len(s.intents))
		for k, v := range s.intents {
			next[k] = v
		}
		if req2.State == "drained" && echo.State == "drained" {
			// The cell complied, so the request BECOMES the record —
			// deleting it dropped the operator's reason and ETA one
			// heartbeat after every ack. Since is the echo's exactly, so
			// decorate's echo override never fires and the entry can't
			// read as a pending request.
			next[req.Cell] = Intent{State: "drained", Reason: req2.Reason, ETA: req2.ETA, Since: echo.Since}
		} else {
			delete(next, req.Cell)
		}
	} else if hasRequest {
		d := req2
		desired = &d
	}
	s.mu.Unlock()
	if next != nil {
		// Clone → persist → swap (C1's discipline): a failed write must not
		// leave memory claiming a resolution the file doesn't carry, or a
		// restart resurrects a resolved drain. The unresolved request stays
		// in memory and the next heartbeat retries.
		if err := s.persistIntents(next); err != nil {
			slog.Warn("intent persist failed (echo resolution); retrying next announce", "cell", req.Cell, "err", err)
		} else {
			s.mu.Lock()
			s.intents = next
			s.mu.Unlock()
		}
	}
	s.intentMu.Unlock()

	// Presence makes last_seen truth: a fresh announce IS a sighting. The
	// persist is age-gated with a forced write on transitions — a cell
	// that only ever announces (no inbound port, C3's whole destination)
	// otherwise had NO persisted sighting at all.
	s.recordSighting(req.Cell, now, firstEver || wasStaleOrWithdrawn || withdrawn)

	for _, ev := range events {
		s.publish(ev)
	}

	cmds := s.drainCommands(req.Cell, req.Seq)
	resp := &AnnounceResponse{IntervalS: intervalS, DesiredIntent: desired, Commands: cmds}

	// withdrawn is the value captured under the first critical section, not
	// a re-read of the shared pointer: the trigger fires on the state as of
	// THIS announce even if a concurrent one has already moved p.
	if withdrawn || firstEver || wasStaleOrWithdrawn {
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

// pruneStaleServingRequest drops a serving-state intent entry when
// its cell goes stale: a dead cell can't reconcile a resume, and the
// entry would otherwise linger forever (a permanently-gone cell's
// pending resume is noise, not signal). Drained entries survive — a
// stale drained cell is exactly what the display needs to say.
func (s *Server) pruneStaleServingRequest(cell string) {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	s.mu.Lock()
	in, ok := s.intents[cell]
	if !ok || in.State != "serving" {
		s.mu.Unlock()
		return
	}
	next := make(map[string]Intent, len(s.intents))
	for k, v := range s.intents {
		next[k] = v
	}
	delete(next, cell)
	s.mu.Unlock()
	// Clone → persist → swap: mutating in place first means a failed write
	// resurrects the dropped request on the next restart.
	if err := s.persistIntents(next); err != nil {
		slog.Warn("serving-request prune persist failed", "cell", cell, "err", err)
		return
	}
	s.mu.Lock()
	s.intents = next
	s.mu.Unlock()
	slog.Info("dropped unresolvable serving request (cell went stale)", "cell", cell)
}

// modelFingerprintChanged reports whether any model's announced
// flags_sha256 changed for an id the cell was ALREADY announcing. Only
// the hash is compared: State flips running/stopped constantly, so
// folding the whole model struct in would turn every load and TTL
// unload into a membership transition.
func modelFingerprintChanged(prev, next []AnnounceModel) bool {
	if len(prev) == 0 || len(next) == 0 {
		return false
	}
	prevSHA := make(map[string]string, len(prev))
	for _, m := range prev {
		prevSHA[m.ID] = m.FlagsSHA256
	}
	for _, m := range next {
		if m.FlagsSHA256 == "" {
			continue
		}
		if was, known := prevSHA[m.ID]; known && was != m.FlagsSHA256 {
			return true
		}
	}
	return false
}

// modelSetChanged compares model id SETS (order-insensitive). Announces
// are untrusted input, so duplicate ids must not hide a change: comparing
// slice lengths and then only next⊆prev misses [A,B] → [A,A].
func modelSetChanged(prev, next []AnnounceModel) bool {
	ids := func(ms []AnnounceModel) map[string]bool {
		out := make(map[string]bool, len(ms))
		for _, m := range ms {
			out[m.ID] = true
		}
		return out
	}
	prevIDs, nextIDs := ids(prev), ids(next)
	if len(prevIDs) != len(nextIDs) {
		return true
	}
	for id := range nextIDs {
		if !prevIDs[id] {
			return true
		}
	}
	return false
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

// drainCommands hands the cell its queued piggyback verbs. Delivery is
// AT-LEAST-ONCE, keyed on the announce seq: the batch moves to an
// in-flight slot stamped with this announce's seq instead of being
// deleted, and only an announce with a HIGHER seq — proof the cell read
// the response it was attached to — retires it. Deleting at hand-off
// lost the batch whenever the response never arrived. Both verbs
// (unload/warm) are idempotent, so a duplicate is harmless.
func (s *Server) drainCommands(cell string, seq uint64) []AnnounceCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pend, ok := s.cmdInflight[cell]; ok {
		if seq > pend.seq {
			delete(s.cmdInflight, cell)
		} else {
			// Same seq (a retried POST) or lower (seq resets per cell
			// boot): redeliver and re-stamp, so a reboot can't pin the
			// slot forever.
			pend.seq = seq
			s.cmdInflight[cell] = pend
			return pend.cmds
		}
	}
	cmds := s.commands[cell]
	if len(cmds) == 0 {
		return nil
	}
	delete(s.commands, cell)
	s.cmdInflight[cell] = inflightCommands{seq: seq, cmds: cmds}
	return cmds
}

// QueueCommand piggybacks a verb for a cell that fleetd cannot reach
// interactively. The model is validated against what the cell ANNOUNCED
// (the comment on queueCommand's bound demands it): a typo must fail at
// the caller rather than sit in a queue nothing will ever execute.
func (s *Server) QueueCommand(cell string, cmd AnnounceCommand) error {
	switch cmd.Verb {
	case "unload", "warm":
	default:
		return fmt.Errorf("unknown verb %q (want unload or warm)", cmd.Verb)
	}
	p := s.PresenceFor(cell)
	if p == nil || !p.Announcing {
		return fmt.Errorf("cell %q has never announced; nothing would collect the command", cell)
	}
	for _, m := range p.Models {
		if m.ID == cmd.Model {
			s.queueCommand(cell, cmd)
			return nil
		}
	}
	return fmt.Errorf("cell %q does not announce a model %q", cell, cmd.Model)
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
	defer s.wg.Done()
	tick := time.NewTicker(s.stalenessTick)
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
				s.pruneStaleServingRequest(name)
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

// inflightCommands is one cell's handed-over command batch, stamped with
// the announce seq it rode out on.
type inflightCommands struct {
	seq  uint64
	cmds []AnnounceCommand
}
