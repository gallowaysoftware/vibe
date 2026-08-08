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
	// Probe is C3 §1's reserved per-model throughput-health block, filled
	// in by C8. Nil marshals to `null` exactly as the v1 reservation did,
	// so the wire is unchanged for cells that do not probe.
	Probe *AnnounceProbe `json:"probe"`
}

// Probe verdicts. A verdict is only ever computed from a probe that
// RAN and returned a number: "no probe has run" is VerdictUnknown, and
// unknown means nothing is known — never that the model is fine.
const (
	VerdictOK       = "ok"
	VerdictDegraded = "degraded"
	VerdictUnknown  = "unknown"
)

// AnnounceProbe is the v2 throughput-health block (fleet-control C8):
// one canned, deterministic request the CELL issued against a model that
// was already resident, timed and scored against that model's own recent
// healthy samples.
//
// It is display and event data only. A degraded verdict never changes
// availability (observed), intent (declared) or residency
// (llama-swap-owned), never excludes a model from the front render, and
// never triggers a warm, an unload or a drain — see C8 §5. The one way
// this phase fails is by letting a measurement become an actuator.
type AnnounceProbe struct {
	Kind string    `json:"kind"`           // "chat" | "embed"
	Spec string    `json:"spec,omitempty"` // canned spec id, e.g. "chat/v1:64out"
	At   time.Time `json:"at"`
	// Metric names WHAT was measured, because two engines answer with
	// different evidence: decode_tok_s comes from llama.cpp's own timings
	// block (queueing excluded), e2e_tok_s is the fallback for responses
	// that carry none. Baselines are keyed by metric so a parser change
	// can never read as a regression.
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	// BaselineP50 is the median of the model's stored healthy samples for
	// this (model, flags_sha256, metric) key, EXCLUDING this one; Samples
	// is how many backed it.
	BaselineP50 float64 `json:"baseline_p50,omitempty"`
	Samples     int     `json:"samples,omitempty"`
	Ratio       float64 `json:"ratio,omitempty"`
	Verdict     string  `json:"verdict"`
	// TTFTMS is recorded for context and deliberately NOT scored: a probe
	// that lands behind real work measures the queue, so scoring it would
	// fire the alarm exactly when the box is busy.
	TTFTMS int64 `json:"ttft_ms,omitempty"`
	// DegradedSince marks when the verdict last flipped to degraded, so a
	// model flagged for a week against an old baseline is legible.
	DegradedSince *time.Time `json:"degraded_since,omitempty"`
	// BaselineAt is the newest healthy sample behind BaselineP50.
	BaselineAt *time.Time `json:"baseline_at,omitempty"`
	// FlagsSHA256 binds the numbers to the serving argv they were measured
	// against: a def edit starts a fresh baseline instead of reporting a
	// configuration change as a regression.
	FlagsSHA256 string `json:"flags_sha256,omitempty"`
	// Note carries a refusal or failure reason (not resident, cooldown,
	// request failed) for a probe that produced no number.
	Note string `json:"note,omitempty"`
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

// AnnounceUsage is the cell's CUMULATIVE token counters (fleet-control
// C7a §4), piggybacked on the heartbeat. Cumulative — not per-interval —
// is what makes an offline cell free: a missed heartbeat loses nothing,
// the next successful announce carries the arrears, and fleetd's delta
// rule is idempotent under retries, duplicate announces and a roaming
// laptop that served traffic off-LAN for a day.
//
// It is a NEW field rather than a widening of AnnounceModel.Probe, which
// stays reserved for the v2 throughput block (C3 §1).
type AnnounceUsage struct {
	// Epoch identifies the cell's counter generation. A cell mints a new
	// one when its llama-swap activity ids restart (an in-memory store
	// restarting at 1); fleetd then starts a new ledger row instead of
	// reading the cell as flatlined for months.
	Epoch string `json:"epoch"`
	// LostRows counts activity rows the cell read but did not fold: they
	// aged out of an in-memory ring between two polls, or they arrived in
	// a window whose own contents contradicted the cursor (a swapped or
	// copied activity store — usagemeter's continuity check refuses such a
	// window whole rather than double-counting into an append-only
	// ledger). Reported, not absorbed: silent loss is indistinguishable
	// from an idle cell.
	LostRows int64 `json:"lost_rows,omitempty"`
	// Models carries one entry per (model, basis) the cell has ever
	// metered this epoch.
	Models []AnnounceUsageModel `json:"models,omitempty"`
}

// AnnounceUsageModel is one cumulative (model, basis) counter set.
//
// Basis records WHICH token-semantics branch produced these numbers.
// llama-swap stores no field saying which parser won, and the two
// branches disagree by 1.8x-5x on the same traffic, so without it the
// cache arithmetic is unreconstructable after the fact (C7a §2).
type AnnounceUsageModel struct {
	Model string `json:"model"`
	Basis string `json:"basis"`
	// Req counts billable requests (status 200, measured, not a poke).
	Req int64 `json:"req"`
	// InFresh is prompt tokens actually processed; InCached is prompt
	// tokens served from the KV cache. Billable input is their sum, and
	// C7b prices them at different rates — which is exactly why they are
	// stored apart.
	InFresh  int64 `json:"in_fresh"`
	InCached int64 `json:"in_cached"`
	Out      int64 `json:"out"`
	// PokeReq counts fleet self-traffic (warm targets, warm schedules,
	// warm_model): real metered requests that must never enter a billable
	// sum or a per-request average.
	PokeReq int64 `json:"poke_req"`
	// ErrReq counts non-200 rows; UnmeasuredReq counts 200s that reported
	// no tokens at all (mlx streaming, client-cancelled streams). Both are
	// counted and NEITHER is summed as zero.
	ErrReq        int64 `json:"err_req"`
	UnmeasuredReq int64 `json:"unmeasured_req"`
	// BusyMS is wall time on measured rows.
	BusyMS int64 `json:"busy_ms"`
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
	// Usage is the C7a token ledger feed. Additive: old fleetd ignores
	// it (unknown fields are tolerated by the C3 schema rule) and old
	// cells omit it, rendering as unmeasured rather than as zero.
	Usage *AnnounceUsage `json:"usage,omitempty"`
}

// AnnounceCommand is one piggybacked verb the cell should execute and
// reflect in its next announce (verbs whose latency doesn't matter
// after C3; interactive paths still use daemon_url when present).
type AnnounceCommand struct {
	Verb  string `json:"verb"` // "unload" | "warm" | "probe"
	Model string `json:"model"`
	// Rebaseline applies to the probe verb only: clear this model's
	// stored samples before recording the new one. It is the escape hatch
	// for a LEGITIMATE permanent slowdown (a build that trades tok/s for
	// something else changes no serving flags, so the fingerprint key
	// cannot notice it), and it is explicit because the alternative —
	// letting degraded samples re-baseline on their own — turns the status
	// green while the box is still slow.
	Rebaseline bool `json:"rebaseline,omitempty"`
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
	// C3 §3 reserved a model_degraded event with no emitter; C8 is the
	// emitter, named in the same dotted camelCase the CLI and the live SSE
	// consumers already match on.
	EventModelDegraded  = "fleet.modelDegraded"
	EventModelRecovered = "fleet.modelRecovered"
	// EventWakeFailed is C14's: a wake this control plane DECLARED and
	// then did not deliver. It is not an absence event — an opportunistic
	// cell's absence is normal and silent — it is a promise that failed.
	EventWakeFailed = "fleet.wakeFailed"
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
	// Membership is hosts.yaml and nothing else, and this refusal is what
	// makes that true: a cell cannot announce itself into existence. So
	// there is no such thing as a cell fleetd knows only through its
	// announces — commissioning is install the daemon/announcer, add the
	// hosts.yaml entry, point it at the registry (C3). Loosening this
	// would make an announce a fleet-wide write from an unauthenticated
	// NAME: the fleet token authenticates the connection, never the cell
	// it claims to be (design §6), and an unknown name can then fake
	// SERVING, prune a roaming catalog or cancel a pending drain.
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
		// The one announce string that fed a status surface without
		// hygiene, found by C9's review: id, state, versions, usage and
		// C8's whole probe block (including probe.flags_sha256 on this same
		// model) all get clean(), and the model-level flags_sha256 did not
		// — while it lands in the presence document, the mismatch event's
		// payload, and from C9 in an alarm's detail line. Consistency with
		// its own sibling is the argument; a flags_sha256 is hex by
		// construction, so a non-printable one is a broken or hostile cell
		// either way.
		if err := clean("models[].flags_sha256", m.FlagsSHA256); err != nil {
			return err
		}
		if err := validateProbe(m.Probe); err != nil {
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
	if req.Usage != nil {
		if err := clean("usage.epoch", req.Usage.Epoch); err != nil {
			return err
		}
		for _, m := range req.Usage.Models {
			if err := clean("usage.models[].model", m.Model); err != nil {
				return err
			}
			// basis is deliberately hygiene-checked, not enum-checked: a
			// cell one version ahead that adds a basis must not have its
			// whole heartbeat rejected (that would take presence and the
			// intent echo down with the accounting). An unknown basis is a
			// pricing question, and pricing is C7b's.
			if err := clean("usage.models[].basis", m.Basis); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateProbe applies the same field hygiene the rest of the announce
// gets to C8's throughput block. Deliberately NOT an enum check on kind
// or metric: a cell one version ahead that measures something new must
// not have its whole heartbeat rejected — that would take presence and
// the intent echo down with an accounting field. The verdict IS
// normalized (normalizeProbe), because that one drives an event.
func validateProbe(p *AnnounceProbe) error {
	if p == nil {
		return nil
	}
	for label, v := range map[string]string{
		"models[].probe.kind":         p.Kind,
		"models[].probe.spec":         p.Spec,
		"models[].probe.metric":       p.Metric,
		"models[].probe.verdict":      p.Verdict,
		"models[].probe.note":         p.Note,
		"models[].probe.flags_sha256": p.FlagsSHA256,
	} {
		if err := clean(label, v); err != nil {
			return err
		}
	}
	return nil
}

// normalizeProbe hardens one announced probe block at ingest. The
// announce is untrusted input (C3/C5's posture — the fleet token is
// every cell's voice), and these numbers are read straight into status
// surfaces: negatives are clamped rather than rendered, and an
// unrecognised verdict becomes "unknown" rather than a fourth state
// nothing downstream handles. Clamping at ingest is the same rule C7a's
// clampUsage follows, minus the append-only urgency.
func normalizeProbe(p *AnnounceProbe) {
	if p == nil {
		return
	}
	switch p.Verdict {
	case VerdictOK, VerdictDegraded, VerdictUnknown:
	default:
		p.Verdict = VerdictUnknown
	}
	p.Value = clampNonNegF(p.Value)
	p.BaselineP50 = clampNonNegF(p.BaselineP50)
	p.Ratio = clampNonNegF(p.Ratio)
	if p.Samples < 0 {
		p.Samples = 0
	}
	if p.TTFTMS < 0 {
		p.TTFTMS = 0
	}
	// The cell's clock, bounded exactly like the intent echo's: this is
	// the second place a cell timestamp is consulted, and "last probed in
	// 973 years" is not a thing an operator can act on.
	if max := time.Now().UTC().Add(echoFutureSkew); p.At.After(max) {
		p.At = max
	}
}

// CloneProbe deep-copies a probe block. A struct copy still carries the
// three *time.Time pointers, which is the exact shape C7a's review pass
// had to fix on UsageReport: captured under a lock, dereferenced after
// it. Nothing writes through them today; this is what keeps that true.
func CloneProbe(p *AnnounceProbe) *AnnounceProbe {
	if p == nil {
		return nil
	}
	cp := *p
	if p.DegradedSince != nil {
		t := *p.DegradedSince
		cp.DegradedSince = &t
	}
	if p.BaselineAt != nil {
		t := *p.BaselineAt
		cp.BaselineAt = &t
	}
	return &cp
}

func clampNonNegF(v float64) float64 {
	// NaN fails every comparison, so it is caught by the negation rather
	// than by v < 0: a NaN rendered into fleet_status is not a number a
	// human can act on, and JSON cannot even carry it.
	if !(v >= 0) {
		return 0
	}
	return v
}

// probeEvents reports the verdict TRANSITIONS between two announces from
// one cell. Steady-state heartbeats carrying the same verdict publish
// nothing (C3's transition-gating rule); degraded → unknown is NOT a
// recovery, because losing the evidence is not the same as getting a
// good number back.
func probeEvents(cell string, prev, next []AnnounceModel) []Event {
	was := make(map[string]string, len(prev))
	for _, m := range prev {
		if m.Probe != nil {
			was[m.ID] = m.Probe.Verdict
		}
	}
	var out []Event
	for _, m := range next {
		if m.Probe == nil {
			continue
		}
		before := was[m.ID]
		switch {
		case m.Probe.Verdict == VerdictDegraded && before != VerdictDegraded:
			out = append(out, Event{Cell: cell, Type: EventModelDegraded, Data: probeEventData(m)})
		case m.Probe.Verdict == VerdictOK && before == VerdictDegraded:
			out = append(out, Event{Cell: cell, Type: EventModelRecovered, Data: probeEventData(m)})
		}
	}
	return out
}

// probeEventData carries the model and its numbers on the event, so an
// SSE consumer does not have to re-fetch state to know what changed.
func probeEventData(m AnnounceModel) json.RawMessage {
	data, err := json.Marshal(struct {
		Model string         `json:"model"`
		Probe *AnnounceProbe `json:"probe"`
	}{Model: m.ID, Probe: m.Probe})
	if err != nil {
		return nil
	}
	return data
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

	// Hardened here rather than in the handler so every ingest path — the
	// HTTP endpoint and the in-process callers — gets the same treatment.
	for i := range req.Models {
		normalizeProbe(req.Models[i].Probe)
	}

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
	// Verdict transitions are computed against the SAME prevModels the
	// render triggers use, under the same lock: reading p.Models again
	// after the unlock would race a concurrent announce and either miss a
	// transition or report one twice.
	probeTransitions := probeEvents(req.Cell, prevModels, req.Models)
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
	events = append(events, probeTransitions...)

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
	stopRecord := hasRequest && IsStopRecord(&req2)
	switch {
	// C24, both halves of the stop record's contract, before the
	// conflict rule can see it. A stop record is not a request: it is
	// the unit's own note that it stopped, so it is never handed back
	// (handing it to reconcile runs cell_cmds.drain on a box that just
	// came back), and it is dropped the moment the cell echoes a drain
	// of its own — a drained echo comes only from a declared drain at
	// the box, which outranks a record written by the stop.
	case stopRecord && echo != nil && echo.State == "drained":
		next = make(map[string]Intent, len(s.intents))
		for k, v := range s.intents {
			next[k] = v
		}
		delete(next, req.Cell)
	case stopRecord:
		// desired stays nil: recorded, never commanded.
	case hasRequest && echo != nil && !echo.Since.IsZero() && req2.Since.Before(echo.Since):
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
	case hasRequest:
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

	// C7a: fold the cell's cumulative token counters and credit residency
	// for this heartbeat's gap. Both are no-ops outside the fleetd role
	// and both skip the front structurally (usage.go's fold).
	if s.usage != nil {
		s.usage.fold(req.Cell, req.Usage, now)
		s.usage.foldResidency(req.Cell, req.Models, intervalS, now)
	}

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
// lost the batch whenever the response never arrived. unload and warm
// are idempotent, so a duplicate is harmless; probe is not (it spends
// GPU time), which is why the cell — not this queue — holds the cooldown
// and the daily cap that make a redelivered probe free.
func (s *Server) drainCommands(cell string, seq uint64) []AnnounceCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropHeldWarmsLocked(cell)
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
	case "unload", "warm", "probe":
	default:
		return fmt.Errorf("unknown verb %q (want unload, warm or probe)", cmd.Verb)
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
