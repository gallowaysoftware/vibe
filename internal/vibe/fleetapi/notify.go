package fleetapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
)

// The alarm column, delivered (fleet-control C9). fleetd evaluates the
// design doc §4 class table's "alarm? yes" column against the SAME
// derived snapshot the fleet page and `vibe cell status` render, then
// hands the result to fleetnotify's dwell/dedup state machine.
//
// It is a state differ, not an event bridge, and the reason is in the
// substrate: a persistent fingerprint mismatch publishes exactly one
// event ever (renderPass runs on triggers, and a steady wrong hash
// triggers nothing), and a drain landing on a leased cell publishes
// none at all. Evaluating conditions also means the pager and the page
// cannot disagree — the absence alarm IS the display state.
//
// Nothing here actuates. The only effect of an alarm is an outbound
// message: no render, no warm, no unload, no intent.

// notifyEvalInterval is the default evaluation cadence. With a 2-minute
// dwell on the fastest alarm, a shorter tick buys nothing and a
// subscription to the event hub buys ~30s on one of four conditions.
const notifyEvalInterval = 30 * time.Second

// notifySendTimeout bounds the snapshot the evaluator takes.
const notifySnapshotTimeout = snapshotTimeout + 2*time.Second

// maxNotifyFieldLen bounds an operator-supplied explicit message.
const maxNotifyFieldLen = 2000

// NotifyScope is the fleet-scope declared intent that gates alarm
// DELIVERY (fleet-control C9 §5). It lives in its own file rather than
// in intent.json because every reader of that file treats a key as a
// CELL — one that announces, echoes and reconciles — and a fleet-wide
// pseudo-cell would sit forever as an unresolvable pending request under
// C3's conflict rule.
//
// It is axis 2 (declared), and what it declares is a fact about the
// OPERATOR — "messages sent to me will not be read" — not about any
// cell. It is consumed by exactly one thing: whether a notification is
// delivered now or counted for the digest. It changes no display, no
// routing, no render and no warm, and the name says notify so a later
// phase cannot mistake it for a fleet-level drain.
type NotifyScope struct {
	Scope  string     `json:"scope"` // "away" | "home"
	Since  time.Time  `json:"since"`
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
	By     string     `json:"by,omitempty"`
}

// Scope values.
const (
	ScopeHome = "home"
	ScopeAway = "away"
)

// awayAt reports whether alarms are suppressed at this instant. An
// expired `until` is home again, evaluated lazily at read exactly as a
// lease expiry is — so a forgotten "away" cannot mute the fleet past the
// date the operator themselves declared.
func (n *NotifyScope) awayAt(now time.Time) bool {
	if n == nil || n.Scope != ScopeAway {
		return false
	}
	if n.Until != nil && !now.Before(*n.Until) {
		return false
	}
	return true
}

// maxAwayWindow bounds a declared away window. A vacation has an end
// date; "away until 2099" is a mute button with extra steps.
const maxAwayWindow = 90 * 24 * time.Hour

// notifyRunner bundles the policy state machine with its delivery
// worker. Nil when notifications are not configured.
type notifyRunner struct {
	tracker   *fleetnotify.Tracker
	deliverer *fleetnotify.Deliverer
	enabled   []string
}

// NotifyLoopConfig wires the notifier.
type NotifyLoopConfig struct {
	Sink      fleetnotify.Sink
	Policy    fleetnotify.Policy
	Interval  time.Duration
	Deliverer fleetnotify.DelivererConfig
}

// StartNotifyLoop launches the evaluator and its delivery worker. It
// returns immediately; both exit on Close.
func (s *Server) StartNotifyLoop(cfg NotifyLoopConfig) {
	if cfg.Sink == nil {
		return
	}
	if cfg.Interval <= 0 {
		cfg.Interval = notifyEvalInterval
	}
	tracker := fleetnotify.NewTracker(cfg.Policy)
	enabled := make([]string, 0, len(tracker.Policy().Alarms))
	for _, k := range tracker.Policy().Alarms {
		enabled = append(enabled, string(k))
	}
	r := &notifyRunner{
		tracker:   tracker,
		deliverer: fleetnotify.NewDeliverer(cfg.Sink, cfg.Deliverer),
		enabled:   enabled,
	}
	s.notifyMu.Lock()
	s.notify = r
	s.notifyMu.Unlock()

	slog.Info("fleet notify enabled", "endpoint", cfg.Sink.Endpoint(), "alarms", strings.Join(enabled, ","))

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := s.notifyCtx()
		defer cancel()
		r.deliverer.Run(ctx)
	}()

	s.wg.Add(1)
	go s.notifyLoop(r, cfg.Interval)
}

// notifyCtx is a context cancelled by Close. Both notify goroutines are
// registered on s.wg, so an unlinked HTTP timeout or backoff sleep would
// hold Close() open for as long as the far side wants — the failure
// warmCtx exists to prevent, one package over.
func (s *Server) notifyCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (s *Server) notifyLoop(r *notifyRunner, interval time.Duration) {
	defer s.wg.Done()
	ctx, cancel := s.notifyCtx()
	defer cancel()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			s.evalNotify(ctx, r)
		}
	}
}

// evalNotify runs one round: snapshot, conditions, state machine,
// enqueue. The tracker lock is never held across the snapshot — the
// snapshot itself renders the notify status block, which takes it.
func (s *Server) evalNotify(ctx context.Context, r *notifyRunner) {
	sctx, cancel := context.WithTimeout(ctx, notifySnapshotTimeout)
	snap := s.Snapshot(sctx)
	cancel()

	now := time.Now().UTC()
	conds := s.notifyConditions(snap)
	away := s.NotifyScopeAt(now).awayAt(now)

	s.notifyMu.Lock()
	out := r.tracker.Step(now, conds, away)
	s.notifyMu.Unlock()

	for _, n := range out {
		r.deliverer.Enqueue(n)
	}
}

// notifyConditions derives every alarm condition from one snapshot. It
// reads ONLY fields the snapshot already carries, so an alarm can never
// describe a fleet the status surfaces do not.
func (s *Server) notifyConditions(snap StateSnapshot) []fleetnotify.Condition {
	var out []fleetnotify.Condition
	for _, c := range snap.Cells {
		if detail, ok := absentAlarm(c); ok {
			out = append(out, fleetnotify.Condition{Kind: fleetnotify.KindCellAbsent, Scope: c.Name, Detail: detail})
		}
		if detail, ok := drainLeaseAlarm(c); ok {
			out = append(out, fleetnotify.Condition{Kind: fleetnotify.KindDrainWithLease, Scope: c.Name, Detail: detail})
		}
		for _, m := range c.Models {
			if m.Probe == nil || m.Probe.Verdict != VerdictDegraded {
				continue
			}
			out = append(out, fleetnotify.Condition{
				Kind:  fleetnotify.KindModelDegraded,
				Scope: c.Name + "/" + m.ID,
				Detail: fmt.Sprintf("%s on %s is degraded: %.1f %s against a baseline of %.1f",
					m.ID, c.Name, m.Probe.Value, m.Probe.Metric, m.Probe.BaselineP50),
			})
		}
	}
	for _, fp := range s.FingerprintMismatches() {
		out = append(out, fleetnotify.Condition{
			Kind:  fleetnotify.KindFingerprint,
			Scope: fp.Cell + "/" + fp.Model,
			Detail: fmt.Sprintf("%s on %s has served mismatched serving flags since %s (%s; expected %s, got %s)",
				fp.Model, fp.Cell, fp.FirstSeen.Format(time.RFC3339), fp.Mode,
				shortSHA(fp.Expected), shortSHA(fp.Got)),
		})
	}
	return out
}

// absentAlarm is the class table's alarm column, verbatim: absence is
// alarming for an always_on cell and normal for the other two classes,
// forever.
//
// Declared intent SUPPRESSES (DRAINED, and OFF — "it was drained
// first"): paging on an explanation the operator wrote down themselves
// is how a notifier gets muted. Inferred intent does nothing at all —
// DRAINED? is read here as the FACT that the intent store holds no
// entry, which is the design's "deliberate stop or crash loop" and
// exactly what an alarm is for. INCONSISTENT is not an alarm: the cell
// answers, the fleet serves, and the design already calls it a nag.
func absentAlarm(c CellSnapshot) (string, bool) {
	if c.Class != string(fleetcfg.ClassAlwaysOn) {
		return "", false
	}
	switch c.Display {
	case DisplayDrainedQ, DisplayOffAway, DisplayOffAwayQ:
	default:
		return "", false
	}
	detail := fmt.Sprintf("always_on cell %s is %s with no declared intent", c.Name, c.Display)
	if c.LastSeen != nil {
		detail += fmt.Sprintf(" (last seen %s)", c.LastSeen.UTC().Format(time.RFC3339))
	}
	return detail, true
}

// drainLeaseAlarm is the "did I just strand a 19-hour job?" question,
// answered without being asked. Leases stay advisory: this reports, it
// does not block, and it never un-drains anything.
func drainLeaseAlarm(c CellSnapshot) (string, bool) {
	if c.Intent == nil || c.Intent.State != "drained" || len(c.Leases) == 0 {
		return "", false
	}
	holders := make([]string, 0, len(c.Leases))
	for _, l := range c.Leases {
		holders = append(holders, fmt.Sprintf("%s holds %s", l.Holder, l.Model))
	}
	sort.Strings(holders)
	why := c.Intent.Reason
	if why == "" {
		why = "no reason given"
	}
	return fmt.Sprintf("%s is drained (%s) while %d advisory lease(s) are active: %s",
		c.Name, why, len(c.Leases), strings.Join(holders, "; ")), true
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

// ─── the fingerprint mismatch set ───────────────────────────────────────────

// FingerprintMismatch is one (cell, model) whose announced serving flags
// disagree with fleetd's own render of the def.
//
// It exists because the mismatch EVENT fires once and then goes silent:
// renderPass runs on triggers, drift's trigger compares against the
// previous announce, and a cell that keeps announcing the same wrong
// hash triggers nothing forever. Persistence has to be measured against
// a set, and the render pass is the thing that evaluates it.
type FingerprintMismatch struct {
	Cell      string    `json:"cell"`
	Model     string    `json:"model"`
	Expected  string    `json:"expected"`
	Got       string    `json:"got"`
	Mode      string    `json:"mode"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

func fingerprintKey(cell, model string) string { return cell + "\x00" + model }

// setFingerprintMismatches replaces the set with what one render pass
// found, preserving FirstSeen for entries that survive. A rebuild is the
// honest update: the pass IS the evaluation, so anything it did not find
// is not mismatched as of now.
func (s *Server) setFingerprintMismatches(found []FingerprintMismatch, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]FingerprintMismatch, len(found))
	for _, f := range found {
		key := fingerprintKey(f.Cell, f.Model)
		f.FirstSeen = now
		if prev, ok := s.fpMismatch[key]; ok {
			f.FirstSeen = prev.FirstSeen
		}
		f.LastSeen = now
		next[key] = f
	}
	s.fpMismatch = next
}

// FingerprintMismatches returns the current set, cell-major.
func (s *Server) FingerprintMismatches() []FingerprintMismatch {
	s.mu.Lock()
	out := make([]FingerprintMismatch, 0, len(s.fpMismatch))
	for _, f := range s.fpMismatch {
		out = append(out, f)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cell != out[j].Cell {
			return out[i].Cell < out[j].Cell
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// ─── the scope store ────────────────────────────────────────────────────────

// NotifyScopeAt returns the declared scope, or nil when none was ever
// declared (home).
func (s *Server) NotifyScopeAt(now time.Time) *NotifyScope {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.notifyScope == nil {
		return nil
	}
	cp := *s.notifyScope
	if s.notifyScope.Until != nil {
		t := *s.notifyScope.Until
		cp.Until = &t
	}
	return &cp
}

// SetNotifyScope declares away or home. `until` accepts an RFC3339
// instant or a Go duration from now; away with no `until` is allowed and
// rendered loudly in fleet_status, because the digest on return is what
// keeps it from becoming a silent mute.
func (s *Server) SetNotifyScope(scope, reason, until, by string) (*NotifyScope, error) {
	switch scope {
	case ScopeAway, ScopeHome:
	default:
		return nil, fmt.Errorf("scope must be %q or %q", ScopeAway, ScopeHome)
	}
	for label, v := range map[string]string{"reason": reason, "by": by} {
		if err := clean(label, v); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	next := &NotifyScope{Scope: scope, Since: now, Reason: reason, By: by}
	if until != "" && scope == ScopeAway {
		at, err := parseUntil(now, until)
		if err != nil {
			return nil, err
		}
		next.Until = &at
	}

	s.notifyMu.Lock()
	s.notifyScope = next
	path := s.notifyScopePath
	cp := *next
	s.notifyMu.Unlock()
	if next.Until != nil {
		t := *next.Until
		cp.Until = &t
	}
	if err := saveNotifyScope(path, next); err != nil {
		// Unlike intent, a failed persist does NOT roll the memory back:
		// the operator declared away and is about to stop reading, so
		// honouring it now and losing it on the next restart beats
		// refusing it. The failure is logged and the state is visible.
		slog.Warn("notify scope persist failed; honoured in memory only", "err", err)
	}
	slog.Info("fleet notify scope declared", "scope", scope, "reason", reason, "until", until)
	return &cp, nil
}

// parseUntil accepts an RFC3339 instant or a Go duration from now.
func parseUntil(now time.Time, v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return boundUntil(now, t.UTC())
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("until %q must be an RFC3339 instant or a Go duration (e.g. \"336h\")", v)
	}
	if d <= 0 {
		return time.Time{}, errors.New("until must be in the future")
	}
	return boundUntil(now, now.Add(d))
}

func boundUntil(now, at time.Time) (time.Time, error) {
	if !at.After(now) {
		return time.Time{}, errors.New("until must be in the future")
	}
	if at.Sub(now) > maxAwayWindow {
		return time.Time{}, fmt.Errorf("until is more than %d days out; a vacation has an end date", int(maxAwayWindow.Hours()/24))
	}
	return at, nil
}

// SendNotification delivers an operator-requested message immediately.
// It is NOT an alarm: it skips the state machine, the dwells and the
// away gate, because the one command that proves the pager works must
// not be the one command that silently does nothing while you are away.
func (s *Server) SendNotification(title, message string) error {
	s.notifyMu.Lock()
	r := s.notify
	s.notifyMu.Unlock()
	if r == nil {
		return errors.New("fleet notifications are not configured (set fleet.notify.url or url_file)")
	}
	if !r.deliverer.Enqueue(fleetnotify.Explicit(time.Now().UTC(), title, message)) {
		return errors.New("the notification queue is full")
	}
	return nil
}

// ─── HTTP ───────────────────────────────────────────────────────────────────

type notifyScopeRequest struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason,omitempty"`
	Until  string `json:"until,omitempty"`
	By     string `json:"by,omitempty"`
}

func (s *Server) handleNotifyScope(w http.ResponseWriter, r *http.Request) {
	var req notifyScopeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	scope, err := s.SetNotifyScope(req.Scope, req.Reason, req.Until, req.By)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(scope)
}

type notifySendRequest struct {
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) handleNotifySend(w http.ResponseWriter, r *http.Request) {
	var req notifySendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Title == "" {
		req.Title = "vibe fleet"
	}
	if len(req.Title) > maxAnnounceFieldLen || len(req.Message) > maxNotifyFieldLen {
		http.Error(w, "title or message too long", http.StatusBadRequest)
		return
	}
	if err := s.SendNotification(req.Title, req.Message); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
}

// ─── status ─────────────────────────────────────────────────────────────────

// notifyStatus is the fleet_status notify block. A suppressed alarm is
// visible HERE — that is the promise that makes "away" a deferral rather
// than a mute.
type notifyStatus struct {
	Configured  bool       `json:"configured"`
	Endpoint    string     `json:"endpoint,omitempty"`
	Scope       string     `json:"scope"`
	ScopeSince  *time.Time `json:"scope_since,omitempty"`
	ScopeUntil  *time.Time `json:"scope_until,omitempty"`
	ScopeReason string     `json:"scope_reason,omitempty"`
	Enabled     []string   `json:"enabled,omitempty"`
	fleetnotify.Status
	Delivery *fleetnotify.DeliveryStats `json:"delivery,omitempty"`
	// FingerprintSource names the evaluator behind the fingerprint alarm.
	// Without a render loop there are no passes, the mismatch set is
	// permanently empty, and a silent zero would read as "no drift" — a
	// guard that cannot be evaluated says so (C5's rule).
	FingerprintSource string `json:"fingerprint_source,omitempty"`
}

func (s *Server) notifyReport() *notifyStatus {
	now := time.Now().UTC()
	s.notifyMu.Lock()
	r := s.notify
	scope := s.notifyScope
	var st notifyStatus
	if r != nil {
		st.Configured = true
		st.Endpoint = r.deliverer.Endpoint()
		st.Enabled = r.enabled
		st.Status = r.tracker.Status()
		d := r.deliverer.Stats()
		st.Delivery = &d
	}
	st.Scope = ScopeHome
	if scope != nil {
		if scope.awayAt(now) {
			st.Scope = ScopeAway
		}
		since := scope.Since
		st.ScopeSince = &since
		st.ScopeReason = scope.Reason
		if scope.Until != nil {
			u := *scope.Until
			st.ScopeUntil = &u
		}
	}
	s.notifyMu.Unlock()
	if !st.Configured && scope == nil {
		return nil
	}
	if st.Configured {
		st.FingerprintSource = "render loop"
		if !s.renderLoopRunning() {
			st.FingerprintSource = "unavailable (no fleet.front_config: the render loop that verifies fingerprints is not running)"
		}
	}
	return &st
}

func (s *Server) renderLoopRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renderLoopOn
}

// ─── persistence ────────────────────────────────────────────────────────────

// loadNotifyScope reads the declared scope; missing is home (the safe
// default: an unreadable file must never mute the fleet). Corrupt
// degrades to home, loudly.
func loadNotifyScope(path string) *NotifyScope {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		slog.Warn("notify scope unreadable, assuming home", "path", path, "err", err)
		return nil
	}
	var sc NotifyScope
	if err := json.Unmarshal(data, &sc); err != nil {
		slog.Warn("notify scope corrupt, assuming home", "path", path, "err", err)
		return nil
	}
	return &sc
}

// saveNotifyScope writes the scope atomically (unique tmp + rename),
// same discipline as the intent and lease stores.
func saveNotifyScope(path string, sc *NotifyScope) error {
	if path == "" {
		return errors.New("notify scope store disabled")
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".notify-scope-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
