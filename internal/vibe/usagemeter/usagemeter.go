// Package usagemeter is the cell-side half of the usage ledger
// (fleet-control C7a §1): the cell's own daemon tails its LOCAL
// llama-swap activity log over localhost and maintains cumulative
// per-(model, basis) counters in a state file, which the announce loop
// piggybacks to fleetd.
//
// Cell-side rather than a fleetd pull for two reasons. C3 was the
// inversion — cells announce, the catalog is derived — and a pull
// re-inverts it. More concretely, ONLY the cell can resolve aliases:
// llama-swap keys each activity row on `RealModelName(requested)`
// (llama-swap v239 internal/shared/http.go:117-127), and the front's
// rendered config is peers-only, so RealModelName finds nothing there
// and the front records whatever model string the client typed. The
// cell's config has models: populated and resolves qwen3.6-27b-tools →
// qwen3.6-27b. Pricing must key on the canonical def name, never on a
// string a client chose.
//
// No new measurement mechanism exists here. llama-swap already writes
// one row per model-dispatched POST at request COMPLETION, which is why
// this beats a counter scrape: nothing is lost when llama-swap swaps a
// model out mid-burst.
package usagemeter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
)

// Tokens mirrors llama-swap v239's store.TokenMetrics. Two sentinels
// matter and both are -1, not 0: cache_tokens is -1 when the response
// carried no cache figure at all, and draft_tokens/draft_acc_tokens are
// -1 on every non-speculative row. Rows llama-swap could not parse at
// all (non-200s, empty bodies) carry the Go zero value instead, so 0 and
// -1 both mean "not reported".
type Tokens struct {
	CacheTokens     int64   `json:"cache_tokens"`
	DraftTokens     int64   `json:"draft_tokens"`
	DraftAccTokens  int64   `json:"draft_acc_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// ActivityRow is the subset of llama-swap v239's store.ActivityLogEntry
// this package reads. Unknown fields are ignored by encoding/json, which
// is the version-skew posture the rest of the fleet protocol uses too.
type ActivityRow struct {
	ID             int64     `json:"id"`
	Timestamp      time.Time `json:"timestamp"`
	Model          string    `json:"model"`
	ReqPath        string    `json:"req_path"`
	RespStatusCode int       `json:"resp_status_code"`
	Tokens         Tokens    `json:"tokens"`
	DurationMs     int64     `json:"duration_ms"`
}

// activityPage mirrors store.ActivityPage.
type activityPage struct {
	Data       []ActivityRow `json:"data"`
	Page       int           `json:"page"`
	Limit      int           `json:"limit"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
}

// The three bases. Basis is the token-semantics branch, and it is
// recorded per row because llama-swap stores nothing that says which
// parser won.
const (
	// BasisChat is llama.cpp's `timings` block: input_tokens is
	// timings.prompt_n, which is CACHE-MISS ONLY.
	BasisChat = "chat"
	// BasisEmbed is the OpenAI `usage` object on /v1/embeddings and the
	// rerank family: input_tokens is the FULL prompt, cache already
	// included.
	BasisEmbed = "embed"
	// BasisOther is every other model-dispatched endpoint llama-swap
	// meters (audio, images, /props, count_tokens). Priced like embed —
	// whatever prompt figure arrives is taken as complete.
	BasisOther = "other"
)

// chatPaths are the endpoints llama.cpp answers with a `timings` block.
// The /v/ spellings are llama-swap's versionless routes (its issue #728),
// stripped before forwarding upstream — same semantics, different path
// string in the log.
var chatPaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/messages":         true,
	"/v1/responses":        true,
	"/infill":              true,
	"/completion":          true,
	"/v/chat/completions":  true,
	"/v/completions":       true,
	"/v/messages":          true,
	"/v/responses":         true,
}

// embedPaths are the endpoints llama.cpp answers with an OpenAI `usage`
// object and NO `timings` block.
var embedPaths = map[string]bool{
	"/v1/embeddings": true,
	"/v1/rerank":     true,
	"/v1/reranking":  true,
	"/rerank":        true,
	"/reranking":     true,
	"/v/embeddings":  true,
	"/v/rerank":      true,
	"/v/reranking":   true,
}

// BasisFor classifies a request path. It branches on req_path and NEVER
// on backend kind, because the same cell yields both semantics: a
// llama.cpp cell serving an embedding model answers /v1/embeddings from
// the `usage` object and /v1/chat/completions from `timings`, and adding
// cache_tokens to the former double-counts the cached prompt.
//
// An mlx cell needs no branch of its own: it answers chat paths from
// `usage` too, so input_tokens is already the full prompt — but it also
// reports no cache figure, so cache clamps to 0 and the chat arithmetic
// (fresh + cached) degenerates to exactly the right answer.
func BasisFor(reqPath string) string {
	p := reqPath
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	// llama-swap meters /upstream/<model>/<path> against the same route
	// table; the row records the full URL path.
	if rest, ok := strings.CutPrefix(p, "/upstream/"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			p = rest[i:]
		}
	}
	switch {
	case chatPaths[p]:
		return BasisChat
	case embedPaths[p]:
		return BasisEmbed
	default:
		return BasisOther
	}
}

// Counters is one (model, basis) counter set. Every field is a raw
// count; no field is ever a price, a rate, or an estimate.
type Counters struct {
	Req           int64 `json:"req"`
	InFresh       int64 `json:"in_fresh"`
	InCached      int64 `json:"in_cached"`
	Out           int64 `json:"out"`
	PokeReq       int64 `json:"poke_req"`
	ErrReq        int64 `json:"err_req"`
	UnmeasuredReq int64 `json:"unmeasured_req"`
	BusyMS        int64 `json:"busy_ms"`
}

func (c *Counters) add(o Counters) {
	c.Req += o.Req
	c.InFresh += o.InFresh
	c.InCached += o.InCached
	c.Out += o.Out
	c.PokeReq += o.PokeReq
	c.ErrReq += o.ErrReq
	c.UnmeasuredReq += o.UnmeasuredReq
	c.BusyMS += o.BusyMS
}

// clampNonNeg folds llama-swap's -1 "not reported" sentinel to 0.
// Summing it raw subtracts a phantom token per row.
func clampNonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// Classify turns one activity row into its basis and its contribution.
// The three corrections (C7a §3) are all visible counters, never silent
// absorption:
//
//   - non-200 → ErrReq. llama-swap's early branch never populates tokens
//     on these, so they already contribute zero correctly. Said out loud
//     so nobody "fixes" it into an estimate.
//   - 200 with no tokens at all → UnmeasuredReq. This is mlx streaming
//     and every client-cancelled stream (`timings` rides only the final
//     chunk, so a Ctrl-C yields a 200 with nothing parseable). Counted,
//     NEVER summed as zero. It is deliberately NOT estimated from
//     duration_ms x tokens_per_second: that field is -1 or 0 on exactly
//     these rows, so an estimate would borrow another row's rate, which
//     manufactures numbers instead of measuring them. The loss is
//     structural and concentrated on the longest generations; label it,
//     don't launder it.
//   - a chat-family 200 that generated <= 1 token → PokeReq. C4's warm
//     targets, warm schedules and warm_model all POST a real 1-token
//     completion, and llama-swap's Metadata is populated only by its own
//     internal handlers, so there is no header to tag them with. A 15s
//     warm loop across three cells can issue more requests than the human
//     does; leaving them in destroys every per-request average. The only
//     false positives are genuine one-token generations.
func Classify(row ActivityRow) (string, Counters) {
	basis := BasisFor(row.ReqPath)
	if row.RespStatusCode != http.StatusOK {
		return basis, Counters{ErrReq: 1}
	}

	in := clampNonNeg(row.Tokens.InputTokens)
	out := clampNonNeg(row.Tokens.OutputTokens)
	var cached int64
	if basis == BasisChat {
		// timings.prompt_n counts only the tokens actually processed, so
		// billable input is prompt_n + cache_n.
		cached = clampNonNeg(row.Tokens.CacheTokens)
	}
	// On embed/other the prompt figure already includes anything served
	// from cache; adding cache_tokens would count it twice. Not a
	// simplification — the double-count is the 1.8x-5x error.

	if in == 0 && cached == 0 && out == 0 {
		return basis, Counters{UnmeasuredReq: 1}
	}
	if basis == BasisChat && out <= 1 {
		return basis, Counters{PokeReq: 1}
	}
	return basis, Counters{
		Req:      1,
		InFresh:  in,
		InCached: cached,
		Out:      out,
		BusyMS:   clampNonNeg(row.DurationMs),
	}
	// draft_tokens and draft_acc_tokens are read by nothing here and
	// never will be: speculative decoding changes how output tokens were
	// PRODUCED, not how many the model emitted. predicted_n already
	// counts accepted drafts. Adding a draft column to a billable figure
	// would invent tokens nobody was served.
}

// entryKey identifies one cumulative counter set.
type entryKey struct {
	Model string
	Basis string
}

// state is the on-disk cursor + cumulative counters. last_row_id and the
// counters live in ONE file written atomically, so a crash cannot leave
// the cursor ahead of the counters (which would silently drop a burst).
type state struct {
	Epoch     string     `json:"epoch"`
	LastRowID int64      `json:"last_row_id"`
	LostRows  int64      `json:"lost_rows"`
	Anchor    *rowAnchor `json:"anchor,omitempty"`
	Models    []Entry    `json:"models"`
}

// rowAnchor fingerprints the row the cursor points AT: the newest row in
// the log at the moment the previous poll pinned its window. It exists
// because the id-reset rule below only answers the DOWNWARD question. A
// store restored, copied or swapped for another whose ids sit ABOVE the
// cursor presents exactly as a cell that served a lot while nobody was
// reading — same epoch, ids jumped forward — and the collector folds the
// whole log a second time into an APPEND-ONLY ledger that can never be
// corrected. Ids alone cannot tell those apart. The anchor can: a log
// that is the one the cursor came from still holds that row.
type rowAnchor struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	Model   string    `json:"model"`
	ReqPath string    `json:"req_path"`
	Status  int       `json:"status"`
}

func anchorOf(r ActivityRow) *rowAnchor {
	return &rowAnchor{ID: r.ID, TS: r.Timestamp, Model: r.Model, ReqPath: r.ReqPath, Status: r.RespStatusCode}
}

func (a *rowAnchor) sameRow(r ActivityRow) bool {
	return a.ID == r.ID && a.TS.Equal(r.Timestamp) && a.Model == r.Model &&
		a.ReqPath == r.ReqPath && a.Status == r.RespStatusCode
}

// maxRowClockSkew is how far a row's timestamp may sit BEHIND the
// anchor's before it stops being an ordering wobble and becomes evidence
// of a different log. llama-swap stamps ts_created at request COMPLETION
// and inserts the row then — verified on real traffic 2026-08-05: a
// 7.18s request issued at 09:57:12 landed as a row stamped 09:57:19, and
// 999 consecutive production rows carried zero timestamp inversions — so
// within ONE store id order and timestamp order agree. The tolerance is
// for a backwards clock correction on the cell, not for request overlap.
const maxRowClockSkew = 5 * time.Minute

// continuityBreak reports the evidence that the log being read is not the
// log the cursor came from, or "" when nothing contradicts continuity.
// Two free checks, one per shape the swap can take:
//
//   - the new store's ids OVERLAP the cursor: the row at the cursor id is
//     still reachable in the walk-back and is a different row;
//   - the new store's ids sit entirely ABOVE the cursor (the shape seen
//     live): nothing at the cursor id is reachable, but rows claiming to
//     be newer than it were recorded BEFORE it, which no single
//     append-ordered log can produce.
//
// Deliberately not a check on "the anchor is unreachable" alone. On an
// in-memory ring the anchor legitimately ages out, and refusing there
// would drop genuinely new traffic on every cell that outruns its ring.
// This is a test for CONTRADICTED continuity, not for unproven
// continuity — a row count cross-check against /api/metrics/stats has the
// same defect from the other side, since a ring's aged-out span and a
// swapped store's id hole are numerically identical.
//
// Two identity changes it deliberately does NOT catch, both named so the
// next reader does not assume otherwise: a store COPIED from a busier box
// whose rows are all stamped after this cell's anchor (nothing in the
// window contradicts anything), and two llama-swap instances sharing one
// `store.path` (each reader sees one continuous log — there is no id
// evidence to find, only a per-instance marker llama-swap does not
// write). The first is rare; the second is a config error whose fix is
// one path per cell.
func continuityBreak(a *rowAnchor, atCursor *ActivityRow, rows []ActivityRow) string {
	if a == nil {
		// No anchor: a state file written before this rule existed, or a
		// cell that has never folded a row. Nothing to contradict.
		return ""
	}
	if atCursor != nil {
		if a.sameRow(*atCursor) {
			// The row the cursor was set from is still sitting at the
			// cursor id, which PROVES this is the log the cursor came
			// from. The clock scan below is the weaker test for when that
			// proof is unavailable; running it anyway would let one
			// out-of-order row — a backwards clock step larger than the
			// tolerance is exactly what maxRowClockSkew is documented to
			// absorb — discard a whole window of real traffic from a log
			// whose identity is settled. These counters are cumulative and
			// the ledger append-only, so that loss is permanent.
			return ""
		}
		return fmt.Sprintf("id %d now holds %s %s at %s; the cursor was set from %s %s at %s",
			a.ID, atCursor.Model, atCursor.ReqPath, atCursor.Timestamp.Format(time.RFC3339),
			a.Model, a.ReqPath, a.TS.Format(time.RFC3339))
	}
	if a.TS.IsZero() {
		return ""
	}
	floor := a.TS.Add(-maxRowClockSkew)
	for _, r := range rows {
		if !r.Timestamp.IsZero() && r.Timestamp.Before(floor) {
			return fmt.Sprintf("row %d sits above cursor %d but was recorded at %s, before the cursor row at %s",
				r.ID, a.ID, r.Timestamp.Format(time.RFC3339), a.TS.Format(time.RFC3339))
		}
	}
	return ""
}

// Entry is one persisted (model, basis) cumulative counter set.
type Entry struct {
	Model string `json:"model"`
	Basis string `json:"basis"`
	Counters
}

// Config wires a Collector.
type Config struct {
	// LlamaSwapURL is the LOCAL llama-swap base. Always localhost: this
	// collector reads the cell it runs on, never a peer.
	LlamaSwapURL string
	// StatePath backs the cursor + cumulative counters.
	StatePath string
	// APIKeyFile is the path to the llama-swap API key this collector must
	// present (fleet-control C15). Empty means the llama-swap runs without
	// `apiKeys:`, which is the reference posture. Read per poll rather than
	// cached, so rotating the key needs no restart, and a declared file
	// that will not read FAILS the poll rather than sending the request
	// unauthenticated: /api/metrics/activity answers 401 without a key
	// (verified on v239), and a silent 401 renders as "not measured",
	// which is indistinguishable from a fleet nobody used.
	APIKeyFile string
	// HTTPClient is the transport seam (tests inject a stub server's
	// client); nil gets a defaultHTTPTimeout client.
	HTTPClient *http.Client
	// Logger; nil → slog.Default.
	Logger *slog.Logger
	// NewEpoch mints an epoch id; nil uses crypto/rand. Test seam.
	NewEpoch func() string
	// PollTimeout overrides the deadline PollAndSnapshot puts on one whole
	// poll (0 → pollTimeout). A seam rather than a knob: 20 seconds is not
	// something an operator should be tuning, but a test that never makes
	// the deadline ELAPSE is not testing it at all, and the whole class of
	// "unreachable" this repo had covered was the microsecond kind —
	// ECONNREFUSED and DNS failure — neither of which reaches a timer.
	PollTimeout time.Duration
	// Now is the clock. Test seam for the freshness bookkeeping below,
	// which is measured in minutes and cannot be waited out. nil →
	// time.Now.
	Now func() time.Time
	// MaxPages bounds one poll's walk back through the activity log
	// (0 → defaultMaxPages). Each page is up to activityLimit rows.
	MaxPages int
	// ModelFilter, when set, keeps only the rows whose model it accepts.
	// It exists for exactly one caller: fleetd tailing the FRONT's
	// activity log for cloud_peer model ids (C7b §6), where the point is
	// to reconstruct a real cloud bill and every local model's row on
	// that log is a duplicate of a cell's own count. The cursor still
	// advances past rejected rows — they were read, they just are not
	// this collector's business.
	ModelFilter func(model string) bool
}

// activityLimit is llama-swap's per-page maximum: parseActivityLimit
// accepts 1..999 inclusive and rejects 1000.
const activityLimit = 999

// defaultMaxPages bounds a single poll at ~10k rows. Beyond that the
// shortfall is reported as lost_rows rather than pinning the announce
// loop to an unbounded backfill.
const defaultMaxPages = 10

// pollTimeout bounds one whole Poll, backfill pages included. The
// announce loop calls Poll inline, and the heartbeat is the cell's only
// evidence of life: a first-boot backfill walking ten pages against a
// slow store must not be able to hold presence hostage. An interrupted
// backfill costs nothing — the cursor only advances on a completed poll,
// so the next heartbeat resumes from the same place.
const pollTimeout = 20 * time.Second

// defaultHTTPTimeout bounds ONE page request when the caller injects no
// client. It is deliberately shorter than pollTimeout: the poll budget is
// the outer bound over up to defaultMaxPages requests, so a per-request
// bound at or above it would let a single stalled page consume the whole
// poll and leave the walk-back with nothing. A llama-swap answering
// /api/metrics/activity is reading its own SQLite over localhost; ten
// seconds is already an enormous allowance.
const defaultHTTPTimeout = 10 * time.Second

// frozenAfter is how long the announced totals may stay frozen by failing
// polls before the failure stops being a blip and starts being a lie.
//
// It is the hazard this collector owns. PollAndSnapshot returns the
// PREVIOUS cumulative totals whether the poll refused, timed out or
// succeeded — correct about the VALUE, since those totals are still true
// — but fleetd derives per-interval deltas from them, and a cumulative
// total that stops moving produces a delta of zero. On fleetd's side "this
// cell is idle" and "this cell's usage collector has been timing out for
// six hours" are then the SAME picture, and the announce block carries no
// field that separates them. Absent evidence read as a healthy value is
// this repo's most recurring defect class; see internal/vibe/observed.
//
// Twenty announce intervals at the DefaultAnnounceIntervalS cadence — long
// enough that a llama-swap restart or a slow page does not page anybody,
// short enough that the log says so long before a human reads a flat usage
// row as a quiet week.
const frozenAfter = 5 * time.Minute

// Collector tails one cell's llama-swap activity log.
type Collector struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger

	// pollMu serializes whole polls. mu alone is not enough: Poll reads
	// the cursor, releases the lock for the HTTP round trip, then folds,
	// so two overlapping polls would both ingest the same rows and double
	// every count in the window. That is a logic race, invisible to
	// -race.
	pollMu sync.Mutex

	mu     sync.Mutex
	epoch  string
	cursor int64
	anchor *rowAnchor
	lost   int64
	totals map[entryKey]Counters

	// The freshness bookkeeping behind Health. `started` is the floor a
	// collector that has NEVER completed a poll is measured from: without
	// it, "no poll has ever finished" would compute a staleness of zero,
	// which is the exact confusion frozenAfter exists to name.
	started  time.Time
	lastPoll observed.Value[time.Time]
	fails    int
	lastErr  string
	lastWarn time.Time
}

// PollHealth is the collector's report on ITSELF: are the counters it
// announces still being refreshed?
//
// It exists because Snapshot cannot answer that. Snapshot returns
// cumulative totals, and totals that stopped moving because every poll
// times out are byte-identical to totals that stopped moving because
// nobody used the cell. This is the pair that separates them.
//
// The announce block (fleetapi.AnnounceUsage) carries no field for it
// yet, so today this reaches a human through the WARN line
// PollAndSnapshot emits once the totals have been frozen for frozenAfter,
// and through this accessor for any surface that wants to render it.
type PollHealth struct {
	// LastPoll is when a poll last completed cleanly. UNKNOWN — not the
	// zero time, and not "now" — when none ever has: a collector that has
	// been refused since boot has no last poll, and spelling that as an
	// instant would hand every reader a measurement nobody took.
	LastPoll observed.Value[time.Time]
	// Failures counts polls that have failed CONSECUTIVELY since the last
	// clean one. Zero with a known LastPoll is the healthy state.
	Failures int
	// LastErr is the most recent poll error, verbatim. A poll error names
	// the local llama-swap and, at worst, the path of an API key file;
	// this package never puts key bytes in an error (see authorize).
	LastErr string
	// StaleFor is how long the announced totals have been frozen: since the
	// last clean poll, or since the collector was built when there has
	// never been one. Zero means the last poll succeeded.
	//
	// A durability failure counts as staleness too. Poll returns an error
	// when the fold succeeded but the state file would not write, and that
	// is not "the totals are old" — it is worse, because the next restart
	// re-ingests rows already counted into an APPEND-ONLY ledger. Both
	// failures break the same claim (the totals this cell announces can be
	// trusted), so both are reported.
	StaleFor time.Duration
}

// String renders the pair for a status line. An unknown LastPoll renders
// as "never", never as a date and never as an implied "just now".
func (h PollHealth) String() string {
	last := "never"
	if t, ok := h.LastPoll.Observed(); ok {
		last = t.Format(time.RFC3339)
	}
	switch {
	case h.Failures == 0 && h.LastPoll.IsKnown():
		return "ok (last poll " + last + ")"
	case h.Failures == 0:
		return "no poll has completed yet (last poll never)"
	}
	msg := fmt.Sprintf("stale for %s after %d consecutive failures (last clean poll %s)",
		h.StaleFor.Round(time.Second), h.Failures, last)
	if h.LastErr != "" {
		msg += ": " + h.LastErr
	}
	return msg
}

// Health reports whether the totals this collector announces are fresh.
func (c *Collector) Health() PollHealth {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthLocked(now)
}

func (c *Collector) healthLocked(now time.Time) PollHealth {
	h := PollHealth{LastPoll: c.lastPoll, Failures: c.fails, LastErr: c.lastErr}
	if c.fails == 0 && c.lastPoll.IsKnown() {
		return h
	}
	// The fallback is NAMED here rather than defaulted: absence of a clean
	// poll means the totals are as old as this collector, not as old as the
	// epoch.
	from := c.lastPoll.OrElse(c.started)
	if d := now.Sub(from); d > 0 {
		h.StaleFor = d
	}
	return h
}

func (c *Collector) now() time.Time {
	if c.cfg.Now != nil {
		return c.cfg.Now()
	}
	return time.Now()
}

// pollBudget is the deadline PollAndSnapshot puts on one whole poll.
func (c *Collector) pollBudget() time.Duration {
	if c.cfg.PollTimeout > 0 {
		return c.cfg.PollTimeout
	}
	return pollTimeout
}

// New builds a Collector and loads (or mints) its state. A missing state
// file is a fresh cell: new epoch, cursor 0, and the FIRST poll ingests
// whatever llama-swap still holds. That is deliberate — on a persistent
// store that is the box's real history, and on an in-memory one it is at
// most the last 1000 rows.
func New(cfg Config) (*Collector, error) {
	if cfg.LlamaSwapURL == "" {
		return nil, errors.New("usagemeter: llama_swap_url is required")
	}
	c := &Collector{
		cfg:    cfg,
		http:   cfg.HTTPClient,
		logger: cfg.Logger,
		totals: map[entryKey]Counters{},
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	c.started = c.now()
	c.load()
	return c, nil
}

func (c *Collector) newEpoch() string {
	if c.cfg.NewEpoch != nil {
		return c.cfg.NewEpoch()
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Time is a weaker id than randomness but a duplicate epoch only
		// merges two ledger rows; refusing to count is worse.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (c *Collector) load() {
	if c.cfg.StatePath == "" {
		c.epoch = c.newEpoch()
		return
	}
	data, err := os.ReadFile(c.cfg.StatePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// A cursor we cannot read is not a cursor of zero: re-ingesting
			// from 0 against a persistent store would double-count months.
			// Mint a fresh epoch so fleetd starts a new row instead.
			c.logger.Warn("usage state unreadable; starting a new epoch", "path", c.cfg.StatePath, "err", err)
		}
		c.epoch = c.newEpoch()
		return
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		c.logger.Warn("usage state corrupt; starting a new epoch", "path", c.cfg.StatePath, "err", err)
		c.epoch = c.newEpoch()
		return
	}
	c.epoch = st.Epoch
	if c.epoch == "" {
		c.epoch = c.newEpoch()
	}
	c.cursor = st.LastRowID
	c.anchor = st.Anchor
	c.lost = st.LostRows
	for _, e := range st.Models {
		c.totals[entryKey{Model: e.Model, Basis: e.Basis}] = e.Counters
	}
}

func (c *Collector) save() error {
	if c.cfg.StatePath == "" {
		return nil
	}
	st := state{Epoch: c.epoch, LastRowID: c.cursor, LostRows: c.lost, Anchor: c.anchor}
	for k, v := range c.totals {
		st.Models = append(st.Models, Entry{Model: k.Model, Basis: k.Basis, Counters: v})
	}
	sortEntries(st.Models)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.cfg.StatePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cell-usage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.cfg.StatePath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Poll reads every activity row newer than the cursor and folds it into
// the cumulative counters, then persists. Safe to call from the announce
// loop on every heartbeat: an idle cell costs one 1-row GET.
func (c *Collector) Poll(ctx context.Context) error {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	err := c.poll(ctx)
	c.recordPoll(err, c.now())
	return err
}

// recordPoll updates the freshness pair. Every exit from a poll passes
// through it — a poll that fails silently is the whole hazard.
func (c *Collector) recordPoll(err error, now time.Time) {
	c.mu.Lock()
	if err != nil {
		c.fails++
		c.lastErr = err.Error()
		c.mu.Unlock()
		return
	}
	frozen := c.healthLocked(now).StaleFor
	warned := !c.lastWarn.IsZero()
	c.lastPoll = observed.Known(now)
	c.fails = 0
	c.lastErr = ""
	c.lastWarn = time.Time{}
	c.mu.Unlock()
	if warned {
		// Only if the freeze was ever announced. A recovery line for an
		// outage nobody was told about is noise, and an outage a human WAS
		// told about needs its all-clear or the log leaves them believing
		// the numbers are still frozen.
		c.logger.Info("usage collector recovered; the totals this cell announces are moving again",
			"frozen_for", frozen.Round(time.Second))
	}
}

// poll is Poll's body. Called with pollMu held.
func (c *Collector) poll(ctx context.Context) error {
	// Probe the head of the log first. It resolves the id-reset question
	// BEFORE any walk-back, which matters: resetting the cursor after a
	// walk would leave the shortfall arithmetic comparing rows read
	// against the OLD cursor with a span measured from the new one, and
	// report a phantom loss of every id on the box.
	head, err := c.fetchPage(ctx, 1, 1)
	if err != nil {
		return err
	}
	var maxID int64
	var headRow ActivityRow
	for _, row := range head.Data {
		if row.ID > maxID {
			maxID = row.ID
			headRow = row
		}
	}

	c.mu.Lock()
	// The id-reset rule. SQLite AUTOINCREMENT restarts at 1 on a fresh
	// in-memory store, so a llama-swap restart makes max_id fall BELOW
	// the cursor. Without this a cell whose cursor sits at 47,000 reads
	// zero for seven months and looks exactly like an idle cell.
	if maxID > 0 && maxID < c.cursor {
		old := c.epoch
		c.epoch = c.newEpoch()
		c.cursor = 0
		c.anchor = nil
		c.lost = 0
		c.totals = map[entryKey]Counters{}
		c.logger.Info("llama-swap activity ids restarted; minted a new usage epoch",
			"old_epoch", old, "new_epoch", c.epoch, "max_id", maxID)
	}
	cursor := c.cursor
	anchor := c.anchor
	c.mu.Unlock()

	// Nothing new. (A reset above always leaves cursor at 0 and maxID at
	// or above 1, so this branch never swallows one.)
	if maxID <= cursor {
		return nil
	}

	rows, seen, atCursor, err := c.fetchSince(ctx, cursor, maxID)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// A window whose own contents contradict the cursor is not folded at
	// all. Under-counting a window and saying so is recoverable; a double
	// count is not, because the ledger fleetd keeps is append-only and
	// these totals are CUMULATIVE — a doubled total is a doubled delta on
	// the next heartbeat and every heartbeat after it. Same honesty as
	// the shortfall below: adopt the new head, report the rows that went
	// uncounted, name the evidence.
	if why := continuityBreak(anchor, atCursor, rows); why != "" {
		c.lost += int64(seen)
		c.cursor = maxID
		c.anchor = anchorOf(headRow)
		// Not the id span (max_id - cursor): on a swapped store the ids
		// between the two never existed on this cell, and claiming
		// thousands of phantom lost rows is a lie in the other direction.
		c.logger.Warn("activity log is not the one this cursor came from; window skipped rather than double-counted",
			"why", why, "cursor", cursor, "max_id", maxID, "rows_skipped", seen, "epoch", c.epoch)
		if err := c.save(); err != nil {
			c.logger.Warn("usage cursor not persisted; a restart will re-ingest rows already counted",
				"path", c.cfg.StatePath, "err", err)
			return err
		}
		return nil
	}

	for _, row := range rows {
		if c.cfg.ModelFilter != nil && !c.cfg.ModelFilter(row.Model) {
			continue
		}
		basis, delta := Classify(row)
		k := entryKey{Model: row.Model, Basis: basis}
		t := c.totals[k]
		t.add(delta)
		c.totals[k] = t
	}

	// Shortfall detection: on an in-memory store a burst larger than the
	// walk-back window genuinely ages rows out. Report it as lost_rows
	// rather than absorbing it — a silent gap is indistinguishable from
	// an idle cell.
	if expected := maxID - cursor; int64(seen) < expected {
		c.lost += expected - int64(seen)
		c.logger.Warn("activity rows aged out before they could be read",
			"lost", expected-int64(seen), "cursor", cursor, "max_id", maxID)
	}
	c.cursor = maxID
	c.anchor = anchorOf(headRow)
	if err := c.save(); err != nil {
		// The poll SUCCEEDED and only the durability failed, so this
		// cannot ride PollAndSnapshot's debug-level "poll failed" line: a
		// read-only or full state dir leaves the cursor advanced in memory
		// and stale on disk, and the next restart re-ingests rows already
		// counted — which fleetd folds as new traffic, because the cell's
		// cumulative total is all it has to go on.
		c.logger.Warn("usage cursor not persisted; a restart will re-ingest rows already counted",
			"path", c.cfg.StatePath, "err", err)
		return err
	}
	return nil
}

// fetchSince walks the activity log back from maxID to cursor,
// newest-first, and returns the rows, how many DISTINCT ids in
// (cursor, maxID] it actually saw — the shortfall denominator — and the
// row currently sitting AT the cursor id, when the walk reached it. That
// last one is the continuity anchor's counterpart: it is free here (the
// walk already reads past the cursor to know it is done) and it is what
// catches a swapped store whose ids overlap the old one's.
//
// maxID is pinned by the caller rather than derived here: rows inserted
// mid-walk shift OFFSET pagination and can re-serve a row on a later
// page. Ids are deduped for the same reason.
func (c *Collector) fetchSince(ctx context.Context, cursor, maxID int64) ([]ActivityRow, int, *ActivityRow, error) {
	maxPages := c.cfg.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	var out []ActivityRow
	var atCursor *ActivityRow
	seen := map[int64]bool{}
	for page := 1; page <= maxPages; page++ {
		p, err := c.fetchPage(ctx, page, activityLimit)
		if err != nil {
			return nil, 0, nil, err
		}
		if len(p.Data) == 0 {
			break
		}
		done := false
		for _, row := range p.Data {
			if row.ID > maxID {
				// Landed after the pin: next poll's window.
				continue
			}
			if row.ID <= cursor {
				done = true
				if row.ID == cursor && atCursor == nil {
					atCursor = &row
				}
				continue
			}
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			out = append(out, row)
		}
		if done || page >= p.TotalPages {
			break
		}
	}
	return out, len(seen), atCursor, nil
}

// authorize stamps the llama-swap API key onto an activity-log request.
// Read per call so a rotated key needs no restart; a declared file that
// will not resolve is an error, never a request sent without the header.
func (c *Collector) authorize(req *http.Request) error {
	if c.cfg.APIKeyFile == "" {
		return nil
	}
	data, err := os.ReadFile(c.cfg.APIKeyFile)
	if err != nil {
		return fmt.Errorf("read api_key_file %s: %w", c.cfg.APIKeyFile, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return fmt.Errorf("api_key_file %s is empty", c.cfg.APIKeyFile)
	}
	// Same check fleetcfg's resolver makes, for the same reason: net/http
	// refuses a header value with an embedded newline or tab, and its
	// "invalid header field value" names no configuration at all. The
	// offending byte is reported by POSITION — printing it would print
	// part of the key. Without this the collector's failure mode is a Go
	// error the operator cannot map back to a file.
	if i := strings.IndexFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return fmt.Errorf("api_key_file %s holds a control character at byte %d: an API key must be one line of printable text", c.cfg.APIKeyFile, i)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return nil
}

func (c *Collector) fetchPage(ctx context.Context, page, limit int) (activityPage, error) {
	u := fmt.Sprintf("%s/api/metrics/activity?sort=id&order=desc&limit=%d&page=%d",
		strings.TrimRight(c.cfg.LlamaSwapURL, "/"), limit, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return activityPage{}, err
	}
	if err := c.authorize(req); err != nil {
		return activityPage{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return activityPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return activityPage{}, fmt.Errorf("GET %s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var p activityPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&p); err != nil {
		return activityPage{}, fmt.Errorf("decode activity page: %w", err)
	}
	return p, nil
}

// Snapshot returns the cumulative announce block. Nil when the collector
// has counted nothing at all — an empty usage block on the wire would be
// indistinguishable from a cell that served zero, and "no data" is the
// honest reading for a cell whose llama-swap has never answered. A cell
// that lost every row it tried to read is NOT nothing: it reports the
// loss with no models.
func (c *Collector) Snapshot() *fleetapi.AnnounceUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.totals) == 0 && c.lost == 0 {
		return nil
	}
	u := &fleetapi.AnnounceUsage{Epoch: c.epoch, LostRows: c.lost}
	for k, v := range c.totals {
		u.Models = append(u.Models, fleetapi.AnnounceUsageModel{
			Model:         k.Model,
			Basis:         k.Basis,
			Req:           v.Req,
			InFresh:       v.InFresh,
			InCached:      v.InCached,
			Out:           v.Out,
			PokeReq:       v.PokeReq,
			ErrReq:        v.ErrReq,
			UnmeasuredReq: v.UnmeasuredReq,
			BusyMS:        v.BusyMS,
		})
	}
	sortAnnounceModels(u.Models)
	return u
}

// PollAndSnapshot is the announce loop's entry point: refresh, then hand
// back cumulative totals. A poll failure is NOT fatal — the previous
// cumulative totals are still true and still worth announcing, and an
// unreachable local llama-swap must never cost the cell its heartbeat.
//
// What it must not do is fail QUIETLY forever. Returning the last known
// totals is right, and it is also exactly what an idle cell returns, so
// the failure is reported at Debug while it is plausibly a blip and at
// WARN once the totals have been frozen for frozenAfter. See PollHealth.
func (c *Collector) PollAndSnapshot(ctx context.Context) *fleetapi.AnnounceUsage {
	pollCtx, cancel := context.WithTimeout(ctx, c.pollBudget())
	defer cancel()
	if err := c.Poll(pollCtx); err != nil {
		c.reportPollFailure(err)
	}
	return c.Snapshot()
}

// reportPollFailure logs one failed poll at the level its AGE earns.
// Rate-limited to one warning per frozenAfter: the announce loop calls
// this every heartbeat, and a line every 15 seconds for six hours is a
// log nobody reads, which fails the same way silence does.
func (c *Collector) reportPollFailure(err error) {
	now := c.now()
	c.mu.Lock()
	h := c.healthLocked(now)
	loud := h.StaleFor >= frozenAfter && (c.lastWarn.IsZero() || now.Sub(c.lastWarn) >= frozenAfter)
	if loud {
		c.lastWarn = now
	}
	c.mu.Unlock()
	if !loud {
		c.logger.Debug("usage poll failed; announcing the last known totals",
			"err", err, "consecutive_failures", h.Failures)
		return
	}
	c.logger.Warn("usage collector has not completed a poll; the token counts this cell announces are FROZEN, not idle",
		"stale_for", h.StaleFor.Round(time.Second),
		"consecutive_failures", h.Failures,
		"last_poll", h.LastPoll.String(),
		"llama_swap_url", c.cfg.LlamaSwapURL,
		"err", err)
}

// Deterministic (model, basis) order keeps the state file and the wire
// bytes stable across polls — map iteration order otherwise rewrites the
// file on every heartbeat and makes a diff meaningless.
// Snapshotter is the announce loop's one-liner wiring, shared by the
// daemon's loop and `vibe fleet announce` so the two can't drift. A
// collector that cannot even be constructed degrades to "no usage
// block" — fleetd then renders the cell as unmeasured, which is the
// truth, instead of costing it its heartbeat.
//
// The Collector is not returned, so a caller wired this way reads its
// freshness from the WARN line PollAndSnapshot emits rather than from
// PollHealth. Take the collector directly (New + PollAndSnapshot) if you
// want to render Health somewhere.
func Snapshotter(llamaSwapURL, statePath string) func(context.Context) *fleetapi.AnnounceUsage {
	c, err := New(Config{LlamaSwapURL: llamaSwapURL, StatePath: statePath})
	if err != nil {
		slog.Warn("usage collector not started; this cell announces no token counts", "err", err)
		return nil
	}
	return c.PollAndSnapshot
}

func sortEntries(es []Entry) {
	slices.SortFunc(es, func(a, b Entry) int {
		if c := strings.Compare(a.Model, b.Model); c != 0 {
			return c
		}
		return strings.Compare(a.Basis, b.Basis)
	})
}

func sortAnnounceModels(ms []fleetapi.AnnounceUsageModel) {
	slices.SortFunc(ms, func(a, b fleetapi.AnnounceUsageModel) int {
		if c := strings.Compare(a.Model, b.Model); c != 0 {
			return c
		}
		return strings.Compare(a.Basis, b.Basis)
	})
}
