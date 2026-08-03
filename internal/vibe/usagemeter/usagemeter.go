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
	Epoch     string  `json:"epoch"`
	LastRowID int64   `json:"last_row_id"`
	LostRows  int64   `json:"lost_rows"`
	Models    []Entry `json:"models"`
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
	// HTTPClient is the transport seam (tests inject a stub server's
	// client); nil gets a 10s-timeout client.
	HTTPClient *http.Client
	// Logger; nil → slog.Default.
	Logger *slog.Logger
	// NewEpoch mints an epoch id; nil uses crypto/rand. Test seam.
	NewEpoch func() string
	// MaxPages bounds one poll's walk back through the activity log
	// (0 → defaultMaxPages). Each page is up to activityLimit rows.
	MaxPages int
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
	lost   int64
	totals map[entryKey]Counters
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
		c.http = &http.Client{Timeout: 10 * time.Second}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
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
	c.lost = st.LostRows
	for _, e := range st.Models {
		c.totals[entryKey{Model: e.Model, Basis: e.Basis}] = e.Counters
	}
}

func (c *Collector) save() error {
	if c.cfg.StatePath == "" {
		return nil
	}
	st := state{Epoch: c.epoch, LastRowID: c.cursor, LostRows: c.lost}
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
	for _, row := range head.Data {
		if row.ID > maxID {
			maxID = row.ID
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
		c.lost = 0
		c.totals = map[entryKey]Counters{}
		c.logger.Info("llama-swap activity ids restarted; minted a new usage epoch",
			"old_epoch", old, "new_epoch", c.epoch, "max_id", maxID)
	}
	cursor := c.cursor
	c.mu.Unlock()

	// Nothing new. (A reset above always leaves cursor at 0 and maxID at
	// or above 1, so this branch never swallows one.)
	if maxID <= cursor {
		return nil
	}

	rows, seen, err := c.fetchSince(ctx, cursor, maxID)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range rows {
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
	return c.save()
}

// fetchSince walks the activity log back from maxID to cursor,
// newest-first, and returns the rows plus how many DISTINCT ids in
// (cursor, maxID] it actually saw — the shortfall denominator.
//
// maxID is pinned by the caller rather than derived here: rows inserted
// mid-walk shift OFFSET pagination and can re-serve a row on a later
// page. Ids are deduped for the same reason.
func (c *Collector) fetchSince(ctx context.Context, cursor, maxID int64) ([]ActivityRow, int, error) {
	maxPages := c.cfg.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	var out []ActivityRow
	seen := map[int64]bool{}
	for page := 1; page <= maxPages; page++ {
		p, err := c.fetchPage(ctx, page, activityLimit)
		if err != nil {
			return nil, 0, err
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
	return out, len(seen), nil
}

func (c *Collector) fetchPage(ctx context.Context, page, limit int) (activityPage, error) {
	u := fmt.Sprintf("%s/api/metrics/activity?sort=id&order=desc&limit=%d&page=%d",
		strings.TrimRight(c.cfg.LlamaSwapURL, "/"), limit, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
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
func (c *Collector) PollAndSnapshot(ctx context.Context) *fleetapi.AnnounceUsage {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	if err := c.Poll(pollCtx); err != nil {
		c.logger.Debug("usage poll failed; announcing the last known totals", "err", err)
	}
	return c.Snapshot()
}

// Deterministic (model, basis) order keeps the state file and the wire
// bytes stable across polls — map iteration order otherwise rewrites the
// file on every heartbeat and makes a diff meaningless.
// Snapshotter is the announce loop's one-liner wiring, shared by the
// daemon's loop and `vibe fleet announce` so the two can't drift. A
// collector that cannot even be constructed degrades to "no usage
// block" — fleetd then renders the cell as unmeasured, which is the
// truth, instead of costing it its heartbeat.
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
