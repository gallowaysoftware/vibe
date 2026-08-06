package fleetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
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
	targets  []WarmTarget
	frontURL string
	warmFn   func(ctx context.Context, frontURL, model string) error
	tick     time.Duration
	// emptyGrace is the time nothing-resident must persist before the
	// empty-restore fires (default 30s ≈ two announce intervals).
	emptyGrace time.Duration
	// warmTimeout bounds one warm call (default warmTimeout, 10m). It is
	// a field for the same reason tick and emptyGrace are: a deadline
	// nothing can dial down is a deadline no test ever reaches, and the
	// repo's whole vocabulary for "unreachable" is an immediate
	// ECONNREFUSED — which returns in microseconds and exercises no
	// deadline at all. Zero means the production constant, resolved at
	// the USE site (restore) rather than at wiring, because restore is
	// called directly with a hand-built config in three tests and a
	// zero there would be an instantly-expired context, not a default.
	warmTimeout time.Duration
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
		cfg.warmFn = s.warmViaFront
	}
	for _, t := range cfg.targets {
		st := &warmTargetState{
			Cell:              t.Cell,
			Model:             t.Model,
			State:             "waiting",
			Detail:            "watching",
			RestoreAfterIdleS: t.RestoreAfterIdle.Seconds(),
		}
		// A target naming a non-chat class is a permanent configuration
		// answer, so it gets a status row and no goroutine — loud at
		// startup rather than at the first tick, and visible in
		// fleet_status rather than silently missing from it.
		refused := s.warmClassRefusal(t.Model)
		if refused != "" {
			st.State, st.Detail = "skipped", refused
			slog.Error("warm target refused: the warm verb is a chat completion", "cell", t.Cell, "model", t.Model, "why", refused)
		}
		s.mu.Lock()
		s.warmStates = append(s.warmStates, st)
		s.mu.Unlock()
		if refused != "" {
			continue
		}
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
	// Drain is checked FIRST, before presence and probes: a drained cell
	// keeps announcing an empty model list by design (fleetannounce: "the
	// unit is stopped reads as an empty model list"), which the
	// nothing-resident branch would read as "restore the default" — and
	// where the drain left llama-swap up, that warm SUCCEEDS, reloading
	// the model onto the GPU the operator just reclaimed.
	if in, ok := s.effectiveIntent(t.Cell); ok && in.State == "drained" {
		s.setWarmState(st, "skipped", "cell drained")
		return
	}
	// A hold (C11) is checked second: it is a declaration, so it is
	// answerable with no evidence at all, and it is the answer to the
	// question the operator is actually asking ("why has my default not
	// come back?"). Drain still outranks it — that is a declaration about
	// the whole box. Everything below this line is evidence, and the
	// point of a hold is that the evidence is right and the conclusion is
	// wrong: the swap looks abandoned because the operator went to lunch
	// mid-evaluation.
	if h, held := s.HoldOn(t.Cell); held {
		s.setWarmState(st, "skipped", holdDetail(h))
		return
	}
	// Third rung, and above every evidence rung: the front's llama-swap has
	// refused fleetd's credential, so this target cannot be warmed through
	// the only channel it has (C15). It is answerable with no evidence at
	// all, like a hold, and it must be answered HERE rather than only at
	// the moment of firing — a refusal decided after the residency read
	// clears `emptySince` on every skip (C11's rule), which makes the
	// status row flap between "nothing resident (confirming)" and
	// "skipped" on alternate ticks and buries the reason.
	//
	// Below the two declarations on purpose: a drained cell and a held one
	// would not be warmed even with a working credential, and naming the
	// front's key on a box the operator took for gaming is the wrong
	// sentence.
	if why, blocked := s.SwapAuthRefusal(fleetcfg.FrontCell); blocked {
		s.setWarmState(st, "skipped", why)
		return
	}
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

	// warmCtx, not context.Background(): the probe is on an s.wg
	// goroutine, so an unlinked one holds Close() for the full
	// snapshotTimeout the same way CC-2's warm did for warmTimeout.
	ctx, cancel := s.warmCtx(snapshotTimeout)
	snap := s.snapshotCell(ctx, Cell{Name: t.Cell, URL: s.cellURL(t.Cell)})
	cancel()
	if !snap.Reachable {
		s.setWarmState(st, "skipped", "cell unreachable")
		return
	}
	s.applyWarmEval(t, st, cfg, snap)
}

// applyWarmEval runs the policy against a fresh cell snapshot.
func (s *Server) applyWarmEval(t WarmTarget, st *warmTargetState, cfg warmLoopConfig, snap CellSnapshot) {
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
	case len(residents) == 0:
		s.mu.Lock()
		if st.emptySince.IsZero() {
			// Monotonic: this is a duration clock, and .UTC() would strip
			// the monotonic reading, making an NTP step skip or wedge the
			// grace the live gate had to add.
			st.emptySince = time.Now()
		}
		emptyFor := time.Since(st.emptySince)
		s.mu.Unlock()
		grace := cfg.emptyGrace
		if grace <= 0 {
			grace = 30 * time.Second
		}
		if emptyFor < grace {
			s.setWarmState(st, "waiting", "nothing resident (confirming)")
			return
		}
		s.mu.Lock()
		st.emptySince = time.Time{}
		s.mu.Unlock()
		s.restore(t, st, cfg, "nothing resident")
	default:
		s.mu.Lock()
		st.emptySince = time.Time{}
		s.mu.Unlock()
		// An in-flight request IS activity, and one generation longer
		// than restore_after_idle would otherwise read as idle and be
		// evicted mid-stream: the timestamp map only records the frames
		// that mention a model, and a long request produces no frame
		// between its start and its completion.
		if n, reported := s.InFlight(t.Cell).Observed(); reported && n > 0 {
			s.setWarmState(st, "waiting", fmt.Sprintf("cell busy (%d in-flight)", n))
			return
		}
		// An operator swap is resident: restore only after EVERY
		// resident has been request-idle for the window. Any request to
		// any of them resets it.
		idle, idleFor, unknown := s.swapIdleFor(t.Cell, residents)
		// A resident with no recorded activity is evidence of idleness
		// only where fleetd is WATCHING the cell. With no events stream —
		// the announce-only case C3 exists for — "no requests seen" is
		// absence of observation, and restoring on it makes fleetd's own
		// uptime the clock §1 forbids: once uptime passes the window,
		// every resident swap is evicted on the first tick.
		if len(unknown) > 0 && !s.observesActivity(t.Cell) {
			s.setWarmState(st, "skipped", fmt.Sprintf("no activity evidence for %s (no events stream to %s)", strings.Join(unknown, ","), t.Cell))
			return
		}
		if idle >= t.RestoreAfterIdle {
			s.restore(t, st, cfg, fmt.Sprintf("swap idle %s", idleFor))
			return
		}
		detail := fmt.Sprintf("swap %s active (idle %s of %s)", strings.Join(residents, ","), idleFor, t.RestoreAfterIdle)
		if len(unknown) > 0 {
			detail += fmt.Sprintf("; no activity evidence for %s (idle measured from fleetd start)", strings.Join(unknown, ","))
		}
		s.setWarmState(st, "waiting", detail)
	}
}

// observesActivity reports whether fleetd has an activity-observation
// channel to the cell at all: an inflight frame ever received, or the
// cell's /api/events stream open right now. It is the difference between
// "fleetd watched and saw nothing" (evidence of idleness) and "fleetd
// never looked" (no evidence), which the warm policy must not conflate.
func (s *Server) observesActivity(cell string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight[cell].IsKnown() || s.cellUp[cell]
}

// swapIdleFor reports the SHORTEST idle window across the resident
// models — the most recently used one — so restore fires only when even
// the busiest resident has been quiet for the whole window. The third
// return names the residents no inflight frame has ever mentioned;
// their idle is measured from fleetd's own start, because fleetd cannot
// claim silence it was not running to observe.
func (s *Server) swapIdleFor(cell string, residents []string) (time.Duration, string, []string) {
	now := time.Now()
	minIdle := time.Duration(-1)
	var unknown []string
	for _, id := range residents {
		last, ok := s.modelLastActivity(cell, id).Observed()
		var idle time.Duration
		if !ok {
			unknown = append(unknown, id)
			idle = now.Sub(s.started)
		} else {
			idle = now.Sub(last)
		}
		if minIdle < 0 || idle < minIdle {
			minIdle = idle
		}
	}
	if minIdle < 0 {
		// residents is non-empty at the only call site; defensive.
		minIdle = 0
	}
	return minIdle, minIdle.Round(time.Second).String(), unknown
}

// warmTimeout bounds one warm call. A cold start on a large model is
// minutes, so it is generous; warmCtx is what keeps it from holding
// Close() hostage.
const warmTimeout = 10 * time.Minute

// warmBound resolves a warm's deadline: the injected one when a caller
// dialled it down, the production constant otherwise. A non-positive
// duration must never reach warmCtx — context.WithTimeout(bg, 0) is
// already expired, so a zero-valued seam would silently convert every
// warm into an instant deadline-exceeded instead of a 10-minute one.
func warmBound(d time.Duration) time.Duration {
	if d <= 0 {
		return warmTimeout
	}
	return d
}

// warmCtx builds a warm's timeout context and links cancellation to
// s.done. Both warm loops call warmFn synchronously from goroutines
// registered on s.wg, so an unlinked context lets Close() → wg.Wait()
// block for the full warmTimeout against an unreachable front.
func (s *Server) warmCtx(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	stop := make(chan struct{})
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-stop:
		}
	}()
	// context.CancelFunc is documented as safe to call more than once, so
	// the returned one must be too — the close(stop) would otherwise
	// panic on a second call.
	var once sync.Once
	return ctx, func() {
		once.Do(func() { close(stop) })
		cancel()
	}
}

// restore fires the warm and marks the state. A warm fleetd cannot
// deliver through the front falls back to the announce piggyback; the
// state stays "waiting" either way, because a QUEUED warm has not
// restored anything yet and LastRestore must not claim it did.
func (s *Server) restore(t WarmTarget, st *warmTargetState, cfg warmLoopConfig, why string) {
	if refused := s.warmClassRefusal(t.Model); refused != "" {
		s.setWarmState(st, "skipped", refused)
		return
	}
	// The same rung again at FIRE time, the shape `warmClassRefusal` uses:
	// the check above keeps the status row honest, this one is what makes
	// the rule hold for any producer that reaches `restore` another way.
	// The warm is not queued to the cell either — a credential fleetd
	// cannot present to the front is not something the cell can fix, and
	// routing around the refusal would hide it.
	if why, blocked := s.SwapAuthRefusal(fleetcfg.FrontCell); blocked {
		s.setWarmState(st, "skipped", why)
		return
	}
	ctx, cancel := s.warmCtx(warmBound(cfg.warmTimeout))
	defer cancel()
	var err error
	if s.frontCanRoute(t.Cell) {
		err = cfg.warmFn(ctx, cfg.frontURL, t.Model)
	} else {
		err = noFrontRoute(t.Cell)
	}
	if err != nil {
		note, qerr := s.queueWarm(t.Cell, t.Model, err)
		if qerr != nil {
			s.setWarmState(st, "waiting", "warm failed: "+qerr.Error())
			slog.Warn("warm-target restore failed", "cell", t.Cell, "model", t.Model, "err", qerr)
			return
		}
		s.setWarmState(st, "waiting", "warm "+note)
		slog.Info("warm-target restore piggybacked on the announce", "cell", t.Cell, "model", t.Model, "why", why, "err", err)
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
	// A SKIP is not an observation of emptiness. Every skip above returns
	// before the residency branches run, so a stale emptySince would
	// survive the whole skip and let the empty-restore fire on the first
	// tick after a hold or a drain ends — against a cell whose model is
	// mid-cold-start, which is the exact live race C4's gate 1 found and
	// the grace window exists to prevent. Restart the window when
	// observation resumes.
	if state == "skipped" {
		st.emptySince = time.Time{}
	}
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

// warmClassRefusal reports why an automated warm must not fire at model,
// or "" when it may. Every warm this package issues is `warmViaFront` —
// a CHAT completion — and hosts.yaml's model_classes is the operator's
// declaration that an id is not a chat model.
//
// It is checked at BOTH ends of every producer, which is the shape this
// repo keeps getting wrong from the other side. `warm_model` has refused
// these since C1; the warm-target restore, the warm schedule and C14's
// post-wake warms did not, so the guard held on the verb an operator
// types by hand and on none of the three that run at 06:30 — the lab
// watched a `warm_schedule` land five chat completions on an embed-class
// id (all HTTP 500, all JIT-loading the model onto a GPU for nothing,
// all recorded as err_req in an append-only ledger) seconds after the
// same fleetd refused `warm_model` for that exact id.
//
// Wiring-time refusal is not redundant with fire-time refusal: a refused
// entry that never gets a goroutine is inert, but only a status row makes
// it VISIBLE, and C4's rule is that a silently absent target is worse
// than a clamped one. Fire time is what makes the rule hold for any
// producer that reaches the loops another way.
func (s *Server) warmClassRefusal(model string) string {
	return s.hosts.WarmClassRefusal(model)
}

// warmViaFront is the default warm: a 1-token chat completion through
// the front — JIT is the start verb.
//
// A method since C15, because the request needs the FRONT's llama-swap
// credential. frontURL and the credential must name the same cell, and
// both come from the front entry in hosts.yaml: `warmViaFront` is the
// name of the route, not a parameter — every caller passes the front's
// url (daemon/warm.go, daemon/sleep.go), because a warm that reached a
// cell directly would be the new data-plane hop the plan forbids.
func (s *Server) warmViaFront(ctx context.Context, frontURL, model string) error {
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
	if err := s.AuthorizeSwap(req, fleetcfg.FrontCell); err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	s.NoteSwapStatus(fleetcfg.FrontCell, resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// The body is llama-swap's, not fleetd's: on a 401 it reads
		// "unauthorized: invalid or missing API key", which says nothing
		// about WHICH config is wrong. swapWarmError attaches the sentence
		// that does, keeping the typed status the piggyback rule reads.
		return s.swapWarmError(&warmHTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))})
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// swapWarmError decorates a front refusal with the credential diagnosis
// when there is one. The wrapped value stays a *warmHTTPError so
// definitiveWarmRefusal still sees the 4xx and refuses to queue it: a 401
// is the front ANSWERING, and telling an operator the warm is "queued for
// the next announce" would hide a broken credential behind a promise.
func (s *Server) swapWarmError(he *warmHTTPError) error {
	if he.Status != http.StatusUnauthorized && he.Status != http.StatusForbidden {
		return he
	}
	why, _ := s.SwapAuthRefusal(fleetcfg.FrontCell)
	if why == "" {
		return he
	}
	return fmt.Errorf("%w — %s", he, why)
}

// warmHTTPError is a warm the front ANSWERED with a non-200. It is a
// type rather than a formatted string so the piggyback fallback below
// can tell a delivery failure (transport error, 5xx) from a definitive
// refusal (4xx) — the distinction fleetmcp's unload fallback already
// draws against a cell's admin port.
type warmHTTPError struct {
	Status int
	Body   string
}

func (e *warmHTTPError) Error() string {
	return fmt.Sprintf("warm through front: HTTP %d: %s", e.Status, e.Body)
}

// definitiveWarmRefusal reports whether the warm was REFUSED rather
// than merely undelivered — the two cases the piggyback queue must not
// be allowed to blur. Anything else — a transport error, a 5xx, an
// injected test error — is a failure to DELIVER, which is what the
// queue exists for.
//
// Two shapes qualify. The front ANSWERING with a 4xx (a *warmHTTPError)
// is C6's rule. A *swapAuthError is C15's: fleetd could not resolve the
// credential, so nothing left this box, and the cell cannot fix the
// front's key file — queueing the warm there would execute it against
// the cell's own llama-swap and hide a broken credential behind a
// promise. Until this was typed, the 401 was refused and the
// unresolvable declaration right beside it was piggybacked.
func definitiveWarmRefusal(err error) bool {
	var he *warmHTTPError
	if errors.As(err, &he) {
		return he.Status >= 400 && he.Status < 500
	}
	var sae *swapAuthError
	return errors.As(err, &sae)
}

// queueWarm is the warm loops' half of the piggyback producer (C6's
// MIN-G finished: fleetmcp's unload landed on the C6 branch, these two
// files were C4's and not on it). It mirrors fleetmcp's queueUnload
// exactly:
//
//   - the model is validated against what the cell ANNOUNCED
//     (QueueCommand's own rule), so a typo fails here rather than
//     sitting in a queue nothing will execute;
//   - a cell that never announces stays an error — nothing would ever
//     collect the command, and pretending otherwise is worse than
//     failing;
//   - a DEFINITIVE 4xx from the front is a real error and is NOT
//     queued. The front answering "no such model" means the cell will
//     refuse the same verb identically, and reporting that as "queued
//     for the next heartbeat" is a lie about a warm that will never
//     happen.
//
// On success it returns the status note; on failure an error naming
// BOTH the original cause and why the piggyback was unavailable.
func (s *Server) queueWarm(cell, model string, cause error) (string, error) {
	// The piggyback is the same actuation by another channel, and the
	// cell executes the verb with no idea what class the model is (C11's
	// dropHeldWarmsLocked exists for the same reason). A refusal that
	// only held on the front's route would be a guard the queue walks
	// around one heartbeat later — which is exactly what the live gate
	// saw: the front answered the embed warm with a 500, and a 500 is a
	// DELIVERY failure, so the refused warm was queued for the cell.
	if refused := s.warmClassRefusal(model); refused != "" {
		return "", errors.New(refused)
	}
	if cell == "" {
		return "", fmt.Errorf("%w (piggyback unavailable: no cell resolved for %s)", cause, model)
	}
	if definitiveWarmRefusal(cause) {
		// Not wrapped with a piggyback note: there is nothing unavailable
		// here, the front gave an answer.
		return "", cause
	}
	if qerr := s.QueueCommand(cell, AnnounceCommand{Verb: "warm", Model: model}); qerr != nil {
		return "", fmt.Errorf("%w (piggyback also unavailable: %v)", cause, qerr)
	}
	return fmt.Sprintf("queued for %s's next announce (≤ one heartbeat) after the front did not answer: %v", cell, cause), nil
}

// frontCanRoute reports whether the front could route a warm to this
// cell at all. The front's peer config is rendered from the registry
// (render_loop.go), and every registry cell carries a url — hosts.yaml
// requires one — so a cell with no url here is a cell that is not in the
// registry. Sending the warm anyway would earn a definitive 404 from the
// front and, by the rule above, correctly refuse the queue.
func (s *Server) frontCanRoute(cell string) bool { return s.cellURL(cell) != "" }

// noFrontRoute names what the missing route actually IS.
//
// It used to say "(announce-only)", describing a cell fleetd knew only
// through its announces — a state that cannot exist. hosts.yaml is the
// single source of cell membership, and POST /api/fleet/announce refuses
// a cell absent from it (announce.go), so nothing can announce itself
// into the registry. What reaches here is a warm target, a backend def's
// `cell:` or a sleep entry naming a box hosts.yaml has never heard of,
// and "announce-only" sent the operator looking for a dead announcer
// instead of a missing registry entry — which the piggyback attempt that
// follows then confirmed with "has never announced".
func noFrontRoute(cell string) error {
	return fmt.Errorf("cell %q is not in fleetd's cell registry (hosts.yaml), so the front has no peer entry to route a warm through", cell)
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
