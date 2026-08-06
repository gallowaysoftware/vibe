package usagemeter

// The poll budget, and the hazard it leaves behind.
//
// Two halves, and the second is the one that matters.
//
// FIRST: pollTimeout and the default HTTP client bound a poll against a
// llama-swap that ACCEPTS the connection and then says nothing. Every
// unreachable-store case this package had covered was an httptest server
// that was already Closed — an immediate ECONNREFUSED, resolved in
// microseconds, which never reaches a timer. A first-boot backfill
// walking ten pages against a slow SQLite is the realistic shape, and the
// announce loop calls Poll INLINE: the heartbeat is the cell's only
// evidence of life, so an unbounded poll costs the cell its presence.
//
// SECOND: a poll that times out is INVISIBLE downstream. PollAndSnapshot
// returns the previous cumulative totals, which are still true, and
// fleetd derives deltas from them — so a collector that has been timing
// out for six hours announces exactly what an idle cell announces. These
// tests pin the freshness pair that separates them, and pin that the
// interrupted backfill costs the append-only ledger nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// ─── a llama-swap that answers slowly, or not at all ───────────────────────

// slowSwap serves the same GET /api/metrics/activity contract fakeSwap
// does, with one addition: requests for page >= blockFrom park until the
// test ends or the client hangs up. Blocking on a PAGE rather than on a
// duration is what makes the backfill test deterministic — "the deadline
// fired somewhere in the walk" is not the claim; "the deadline fired on
// page two, and page one's rows were not kept" is.
type slowSwap struct {
	mu        sync.Mutex
	rows      []ActivityRow // ascending by id
	blockFrom int           // 0 = never block
	reqs      int
	release   chan struct{}
}

func (s *slowSwap) setBlockFrom(page int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockFrom = page
}

func (s *slowSwap) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs
}

func (s *slowSwap) start(t *testing.T) string {
	t.Helper()
	s.release = make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	// LIFO: release the parked handlers, THEN close. httptest.Close waits
	// on live connections, so a handler parked forever would wedge the
	// binary instead of failing a named test.
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(s.release) })
	return ts.URL
}

func (s *slowSwap) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/metrics/activity" {
		http.NotFound(w, r)
		return
	}
	limit := 25
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	page := 1
	fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)

	s.mu.Lock()
	s.reqs++
	blockFrom := s.blockFrom
	rows := slices.Clone(s.rows)
	s.mu.Unlock()

	if blockFrom > 0 && page >= blockFrom {
		select {
		case <-s.release:
		case <-r.Context().Done():
		}
		return
	}

	desc := make([]ActivityRow, len(rows))
	for i, row := range rows {
		desc[len(rows)-1-i] = row
	}
	total := len(rows)
	totalPages := (total + limit - 1) / limit
	lo := (page - 1) * limit
	hi := min(lo+limit, total)
	var out []ActivityRow
	if lo < total {
		out = desc[lo:hi]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(activityPage{Data: out, Page: page, Limit: limit, Total: total, TotalPages: totalPages})
}

// fakeClock drives the freshness bookkeeping, which is measured in
// minutes and cannot be waited out in a test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// pollWithin runs one PollAndSnapshot and fails the test if it has not
// returned within limit. Bounded on purpose: a disarmed deadline must
// produce a NAMED failure, not a package-level timeout that reads as
// infrastructure trouble.
func pollWithin(t *testing.T, limit time.Duration, c *Collector) *fleetapi.AnnounceUsage {
	t.Helper()
	done := make(chan *fleetapi.AnnounceUsage, 1)
	go func() { done <- c.PollAndSnapshot(context.Background()) }()
	select {
	case u := <-done:
		return u
	case <-time.After(limit):
		t.Fatalf("PollAndSnapshot did not return within %s against a llama-swap that accepted the connection "+
			"and said nothing — the poll budget is not being applied, and the announce loop calls this INLINE", limit)
		return nil
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ─── the budgets ───────────────────────────────────────────────────────────

// The injected budgets below are generous on purpose. A budget so tight
// that a SUCCESSFUL poll can miss it turns these into load-sensitive
// flakes, and the property under test is "the deadline fires against a
// far side that never answers" — which 200ms demonstrates exactly as well
// as 20ms, and which a refused connection (microseconds) cannot
// demonstrate at all.

// TestPollBudget_ElapsesAgainstAnActivityLogThatNeverAnswers is the gap:
// a store that accepts the GET and stalls. The heartbeat must survive it.
func TestPollBudget_ElapsesAgainstAnActivityLogThatNeverAnswers(t *testing.T) {
	const budget = 200 * time.Millisecond
	swap := &slowSwap{rows: []ActivityRow{chatRow(1, "qwen", 100, 0, 10), chatRow(2, "qwen", 200, 0, 20)}}
	url := swap.start(t)
	clock := newClock()
	c := newCollector(t, url, func(cfg *Config) {
		cfg.PollTimeout = budget
		cfg.Now = clock.now
	})

	// One clean poll first, so the collector has real totals to freeze.
	healthy := pollWithin(t, 5*time.Second, c)
	if healthy == nil || len(healthy.Models) != 1 || healthy.Models[0].Req != 2 {
		t.Fatalf("the seeding poll did not fold: %+v", healthy)
	}
	if h := c.Health(); h.Failures != 0 || h.StaleFor != 0 || !h.LastPoll.IsKnown() {
		t.Fatalf("health after a clean poll = %+v", h)
	}

	swap.setBlockFrom(1)
	start := time.Now()
	frozen := pollWithin(t, 5*time.Second, c)
	elapsed := time.Since(start)

	if elapsed < budget {
		t.Fatalf("the poll returned after %s, less than the %s budget: this did not come from the deadline "+
			"(a refused connection also returns, in microseconds, and proves nothing about it)", elapsed, budget)
	}
	// The value claim the production comment makes: the previous cumulative
	// totals are still true, so they are still announced. A cell must never
	// lose its heartbeat to its own local llama-swap.
	if mustJSON(t, frozen) != mustJSON(t, healthy) {
		t.Fatalf("a timed-out poll changed the announced totals:\n got %s\nwant %s", mustJSON(t, frozen), mustJSON(t, healthy))
	}
}

// TestPollBudget_BoundsTheWholeBackfillIncludingPages: the budget is
// documented as covering one WHOLE poll, "backfill pages included". Page
// one answers, page two stalls — and the interrupted backfill must cost
// the ledger nothing, in either direction.
func TestPollBudget_BoundsTheWholeBackfillIncludingPages(t *testing.T) {
	const budget = 400 * time.Millisecond
	// More than one page: fetchSince pages at activityLimit (999).
	rows := make([]ActivityRow, 0, 1200)
	for id := int64(1); id <= 1200; id++ {
		rows = append(rows, chatRow(id, "qwen", 100, 0, 10))
	}
	swap := &slowSwap{rows: rows, blockFrom: 2}
	url := swap.start(t)
	c := newCollector(t, url, func(cfg *Config) { cfg.PollTimeout = budget })

	start := time.Now()
	if u := pollWithin(t, 5*time.Second, c); u != nil {
		t.Fatalf("a poll that never read page two announced a usage block: %+v", u)
	}
	if elapsed := time.Since(start); elapsed < budget {
		t.Fatalf("the backfill aborted after %s, less than the %s budget", elapsed, budget)
	}
	if got := swap.requests(); got < 3 {
		t.Fatalf("the store saw %d requests; want at least 3 (head probe, page one, the stalled page two) — "+
			"the deadline fired before the walk even started, so this proves nothing about backfill pages", got)
	}

	// The cursor may not have moved. The production comment stakes the
	// whole design on it: "An interrupted backfill costs nothing — the
	// cursor only advances on a completed poll."
	c.mu.Lock()
	cursor, lost := c.cursor, c.lost
	c.mu.Unlock()
	if cursor != 0 {
		t.Fatalf("cursor advanced to %d on an aborted backfill; every row below it is now permanently uncounted "+
			"in an append-only ledger", cursor)
	}
	if lost != 0 {
		t.Fatalf("lost_rows = %d after an aborted poll: nothing aged out, the read simply did not finish", lost)
	}

	// And the resumed poll counts every row EXACTLY once. Under-counting is
	// recoverable; a double count is not, because these totals are
	// cumulative and fleetd's ledger is append-only.
	//
	// Driven through Poll rather than PollAndSnapshot so the resume is not
	// racing the same short budget the abort needed: what is under test
	// here is the ledger arithmetic, not the deadline.
	swap.setBlockFrom(0)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("the resumed poll failed: %v", err)
	}
	u := c.Snapshot()
	if u == nil {
		t.Fatal("the resumed poll announced nothing")
	}
	if len(u.Models) != 1 {
		t.Fatalf("models = %+v", u.Models)
	}
	m := u.Models[0]
	if m.Req != 1200 || m.InFresh != 120000 || m.Out != 12000 {
		t.Fatalf("resumed totals = req %d in_fresh %d out %d, want 1200/120000/12000 exactly once", m.Req, m.InFresh, m.Out)
	}
	if u.LostRows != 0 {
		t.Fatalf("lost_rows = %d; the aborted walk read no rows it then dropped", u.LostRows)
	}
}

// TestPollBudget_DefaultsToPollTimeoutAndCannotOutlastThePresenceHorizon
// pins both halves. The seam must not become the production value, and
// the production value must stay under the horizon fleetd uses to declare
// a cell absent — because the announce loop calls Poll INLINE, so a poll
// budget above that horizon is a slow llama-swap taking the cell's
// presence with it, which is the exact thing pollTimeout's comment says it
// exists to prevent.
func TestPollBudget_DefaultsToPollTimeoutAndCannotOutlastThePresenceHorizon(t *testing.T) {
	c := newCollector(t, "http://127.0.0.1:1")
	if got := c.pollBudget(); got != pollTimeout {
		t.Fatalf("default poll budget = %s, want %s", got, pollTimeout)
	}
	c2 := newCollector(t, "http://127.0.0.1:1", func(cfg *Config) { cfg.PollTimeout = 7 * time.Millisecond })
	if got := c2.pollBudget(); got != 7*time.Millisecond {
		t.Fatalf("declared poll budget = %s, want 7ms", got)
	}

	// fleetapi marks a cell stale at 3 intervals plus five seconds of
	// grace. The grace is left out deliberately: this is the conservative
	// form of the bound, and one that does not have to be re-derived if the
	// grace changes.
	horizon := 3 * time.Duration(fleetapi.DefaultAnnounceIntervalS) * time.Second
	if pollTimeout >= horizon {
		t.Fatalf("pollTimeout (%s) is not under fleetd's %s absence horizon: one stalled activity-log read now "+
			"costs the cell its heartbeat, and presence is what the whole fleet is derived from", pollTimeout, horizon)
	}
	if pollTimeout <= 0 {
		t.Fatal("pollTimeout is not a bound at all")
	}
}

// TestDefaultHTTPClient_IsBoundedAndTighterThanThePollBudget pins the
// number that actually runs: daemon and `vibe fleet announce` both build
// this collector through Snapshotter, which injects no client.
//
// The RELATIONSHIP is the load-bearing half. defaultHTTPTimeout bounds one
// page and pollTimeout bounds up to defaultMaxPages of them, so a
// per-request bound at or above the poll budget would let a single stalled
// page consume the whole thing and leave the walk-back with nothing.
func TestDefaultHTTPClient_IsBoundedAndTighterThanThePollBudget(t *testing.T) {
	c, err := New(Config{LlamaSwapURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.Timeout != defaultHTTPTimeout {
		t.Fatalf("default client budget = %s, want %s", c.http.Timeout, defaultHTTPTimeout)
	}
	if c.http.Timeout <= 0 {
		t.Fatal("the default HTTP client is unbounded: one stalled page would hold the announce loop open forever")
	}
	if defaultHTTPTimeout >= pollTimeout {
		t.Fatalf("defaultHTTPTimeout (%s) is not tighter than pollTimeout (%s): one stalled page can now eat "+
			"the entire poll budget and the backfill gets no requests at all", defaultHTTPTimeout, pollTimeout)
	}
}

// TestPollBudget_ATightHTTPClientStillFailsWithoutLeakingTheKey drives the
// per-request bound end to end: an injected client with a short deadline
// against a stalled store fails the poll, and the error is a deadline
// rather than a refusal.
func TestPollBudget_ATightHTTPClientStillFailsWithoutLeakingTheKey(t *testing.T) {
	swap := &slowSwap{blockFrom: 1}
	url := swap.start(t)
	const key = "swap-key-SECRET"
	keyPath := filepath.Join(t.TempDir(), "swap.key")
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newCollector(t, url, func(cfg *Config) {
		cfg.APIKeyFile = keyPath
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Millisecond}
	})

	done := make(chan error, 1)
	go func() { done <- c.Poll(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a poll against a stalled store reported success")
		}
		if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "Timeout") {
			t.Fatalf("error does not read as a deadline: %v", err)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("the poll error leaked the llama-swap API key: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not return; the injected client's deadline was never applied")
	}
}

// realActivityRow is one row copied VERBATIM off this box's llama-swap on
// 2026-08-06 (GET /api/metrics/activity?limit=1, read-only). It carries
// three fields this package does not model — resp_content_type,
// has_capture, metadata — and a speculative-decoding pair, and it is here
// so the budget work is exercised against the bytes a real store emits
// rather than against a struct this package built for itself.
const realActivityRow = `{"id":96948,"timestamp":"2026-08-06T06:50:12-03:00","model":"qwen3.6-27b",` +
	`"req_path":"/v1/chat/completions","resp_content_type":"text/event-stream","resp_status_code":200,` +
	`"tokens":{"cache_tokens":243,"draft_tokens":626,"draft_acc_tokens":600,"input_tokens":1870,` +
	`"output_tokens":793,"prompt_per_second":2555.346481752503,"tokens_per_second":142.1594322514397},` +
	`"duration_ms":6522,"has_capture":true,"metadata":{"fifo_priority":"0"}}`

// TestPollBudget_ReplaysARealActivityRowAndThenStallsOnIt: the same
// endpoint answers correctly and then stops answering. Both halves matter
// — a deadline that fires against a synthetic row and a fold that is
// exercised only against synthetic rows are each half a test.
func TestPollBudget_ReplaysARealActivityRowAndThenStallsOnIt(t *testing.T) {
	stall := make(chan struct{})
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-blocked:
			select {
			case <-stall:
			case <-r.Context().Done():
			}
			return
		default:
		}
		limit := 25
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[%s],"page":1,"limit":%d,"total":1,"total_pages":1}`, realActivityRow, limit)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stall) })

	c := newCollector(t, srv.URL, func(cfg *Config) { cfg.PollTimeout = 200 * time.Millisecond })
	// The replay serves ONE row out of a store whose ids are in the
	// ninety-thousands, so park the cursor just below it. Without this the
	// collector is right to report 96,947 rows it could not read, and this
	// test would be measuring the shortfall rule rather than the fold.
	c.mu.Lock()
	c.cursor = 96947
	c.mu.Unlock()

	u := pollWithin(t, 5*time.Second, c)
	if u == nil || len(u.Models) != 1 {
		t.Fatalf("a real activity row did not fold: %+v", u)
	}
	m := u.Models[0]
	// Chat basis: input_tokens is timings.prompt_n (cache-MISS only), so
	// billable input is 1870 fresh + 243 cached. The draft pair is read by
	// nothing — accepted drafts are already inside output_tokens, and
	// adding them would invent tokens nobody was served.
	if m.Model != "qwen3.6-27b" || m.Basis != BasisChat {
		t.Fatalf("model/basis = %q/%q", m.Model, m.Basis)
	}
	if m.Req != 1 || m.InFresh != 1870 || m.InCached != 243 || m.Out != 793 || m.BusyMS != 6522 {
		t.Fatalf("folded = %+v, want req 1 / in_fresh 1870 / in_cached 243 / out 793 / busy_ms 6522", m)
	}
	if u.LostRows != 0 {
		t.Fatalf("lost_rows = %d for a window of exactly one row", u.LostRows)
	}

	// Now the same store stops answering.
	close(blocked)
	start := time.Now()
	frozen := pollWithin(t, 5*time.Second, c)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("the stalled poll returned in %s; the budget did not fire", elapsed)
	}
	if mustJSON(t, frozen) != mustJSON(t, u) {
		t.Fatalf("the stalled poll changed the totals: %s vs %s", mustJSON(t, frozen), mustJSON(t, u))
	}
	if h := c.Health(); h.Failures != 1 || !h.LastPoll.IsKnown() {
		t.Fatalf("health after the stall = %+v", h)
	}
}

// ─── the hazard: frozen is not idle ────────────────────────────────────────

// TestPollHealth_FrozenTotalsAreIndistinguishableOnTheWireAndVisibleInHealth
// is the finding this file exists for. The two announce blocks below are
// byte-identical: one from a cell nobody used, one from a cell whose
// collector has been timing out for six hours. That is the whole problem,
// and PollHealth is the only thing that tells them apart.
func TestPollHealth_FrozenTotalsAreIndistinguishableOnTheWireAndVisibleInHealth(t *testing.T) {
	swap := &slowSwap{rows: []ActivityRow{chatRow(1, "qwen", 100, 0, 10)}}
	url := swap.start(t)
	clock := newClock()
	var logs logBuffer
	c := newCollector(t, url, func(cfg *Config) {
		cfg.PollTimeout = 200 * time.Millisecond
		cfg.Now = clock.now
		cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})

	// An idle cell: one clean poll, then nothing new ever happens.
	idle := pollWithin(t, 5*time.Second, c)
	clock.advance(6 * time.Hour)
	stillIdle := pollWithin(t, 5*time.Second, c)
	lastCleanPoll := clock.now()
	if mustJSON(t, idle) != mustJSON(t, stillIdle) {
		t.Fatalf("an idle cell's totals moved: %s vs %s", mustJSON(t, idle), mustJSON(t, stillIdle))
	}
	if h := c.Health(); h.Failures != 0 || h.StaleFor != 0 {
		t.Fatalf("an idle cell that polls fine is not stale: %+v", h)
	}

	// A broken cell: same totals, every poll times out.
	swap.setBlockFrom(1)
	clock.advance(6 * time.Hour)
	broken := pollWithin(t, 5*time.Second, c)

	if mustJSON(t, broken) != mustJSON(t, stillIdle) {
		t.Fatalf("the test's own premise is wrong — the two announce blocks differ:\n%s\n%s",
			mustJSON(t, broken), mustJSON(t, stillIdle))
	}
	h := c.Health()
	if h.Failures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", h.Failures)
	}
	if h.StaleFor < 6*time.Hour {
		t.Fatalf("StaleFor = %s, want at least the 6 hours since the last clean poll — a frozen collector that "+
			"reports itself fresh is the same defect one layer down", h.StaleFor)
	}
	last, ok := h.LastPoll.Observed()
	if !ok {
		t.Fatal("LastPoll is unknown, but a poll DID complete earlier: the evidence was dropped")
	}
	if !last.Equal(lastCleanPoll) {
		t.Fatalf("LastPoll = %s, want the last CLEAN poll at %s: a failing poll must not refresh the evidence "+
			"that a poll succeeded", last, lastCleanPoll)
	}
	if h.LastErr == "" {
		t.Fatal("LastErr is empty after a failed poll")
	}
	if !strings.Contains(h.String(), "stale for") {
		t.Fatalf("PollHealth.String() does not read as a problem: %q", h.String())
	}
}

// TestPollHealth_NeverCompletedIsUnknownNotAnInstant: a collector that has
// never finished a poll has no last poll. Spelling that as the zero time —
// or as "now" — hands every reader a measurement nobody took, which is the
// shape observed.Value exists to remove.
func TestPollHealth_NeverCompletedIsUnknownNotAnInstant(t *testing.T) {
	clock := newClock()
	c := newCollector(t, "http://127.0.0.1:1", func(cfg *Config) { cfg.Now = clock.now })

	h := c.Health()
	if h.LastPoll.IsKnown() {
		t.Fatalf("a collector that never polled reports a last poll: %v", h.LastPoll)
	}
	if h.StaleFor != 0 {
		t.Fatalf("StaleFor = %s at construction, want 0", h.StaleFor)
	}
	if !strings.Contains(h.String(), "never") {
		t.Fatalf("String() = %q, want it to say the poll never happened", h.String())
	}

	// Staleness is measured from construction, not from the epoch: a
	// collector whose store has been unreachable since boot is as old as
	// the collector.
	clock.advance(3 * time.Minute)
	if got := c.Health().StaleFor; got != 3*time.Minute {
		t.Fatalf("StaleFor = %s three minutes after construction, want 3m", got)
	}
}

// TestPollHealth_WarnsOnceTheTotalsAreFrozenAndSaysSoOnlyOnce: the log is
// where this reaches a human today, so the level matters. Debug while it
// is plausibly a blip, WARN once "idle" and "broken" have become the same
// picture, and rate-limited afterwards — a line every 15 seconds for six
// hours is a log nobody reads, which fails the same way silence does.
func TestPollHealth_WarnsOnceTheTotalsAreFrozenAndSaysSoOnlyOnce(t *testing.T) {
	var buf logBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	swap := &slowSwap{rows: []ActivityRow{chatRow(1, "qwen", 100, 0, 10)}}
	url := swap.start(t)
	clock := newClock()
	c := newCollector(t, url, func(cfg *Config) {
		cfg.PollTimeout = 200 * time.Millisecond
		cfg.Now = clock.now
		cfg.Logger = logger
	})
	pollWithin(t, 5*time.Second, c) // seed real totals

	// A blip: one failed poll, seconds after the last good one.
	swap.setBlockFrom(1)
	clock.advance(20 * time.Second)
	pollWithin(t, 5*time.Second, c)
	if n := strings.Count(buf.String(), "FROZEN"); n != 0 {
		t.Fatalf("a 20-second-old failure warned %d times; that is a blip, and a warning per blip trains "+
			"everyone to ignore the one that matters", n)
	}
	if !strings.Contains(buf.String(), "level=DEBUG") {
		t.Fatalf("the blip was not logged at all:\n%s", buf.String())
	}

	// Past the threshold: the totals are now frozen, not idle.
	clock.advance(frozenAfter)
	pollWithin(t, 5*time.Second, c)
	if n := strings.Count(buf.String(), "FROZEN"); n != 1 {
		t.Fatalf("warnings = %d after %s of frozen totals, want exactly 1:\n%s", n, frozenAfter, buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatal("the frozen-totals report is not at WARN; nobody reads DEBUG for six hours")
	}

	// Still broken, but only a minute later: no new line.
	clock.advance(time.Minute)
	pollWithin(t, 5*time.Second, c)
	if n := strings.Count(buf.String(), "FROZEN"); n != 1 {
		t.Fatalf("warnings = %d one minute later; the report must be rate-limited to one per %s", n, frozenAfter)
	}

	// Another full interval: say it again.
	clock.advance(frozenAfter)
	pollWithin(t, 5*time.Second, c)
	if n := strings.Count(buf.String(), "FROZEN"); n != 2 {
		t.Fatalf("warnings = %d after a second %s, want 2 — a freeze that stops being reported reads as fixed", n, frozenAfter)
	}

	// Recovery is announced, because a human was told about the freeze.
	swap.setBlockFrom(0)
	clock.advance(time.Minute)
	pollWithin(t, 5*time.Second, c)
	if !strings.Contains(buf.String(), "recovered") {
		t.Fatalf("no all-clear after recovery; the log leaves a human believing the numbers are still frozen:\n%s", buf.String())
	}
	if h := c.Health(); h.Failures != 0 || h.StaleFor != 0 || !h.LastPoll.IsKnown() {
		t.Fatalf("health after recovery = %+v", h)
	}

	// And the freeze report starts over rather than staying rate-limited by
	// the outage that already ended.
	swap.setBlockFrom(1)
	clock.advance(frozenAfter + time.Second)
	pollWithin(t, 5*time.Second, c)
	if n := strings.Count(buf.String(), "FROZEN"); n != 3 {
		t.Fatalf("warnings = %d for a NEW freeze after a recovery, want 3", n)
	}
}

// TestPollHealth_ADurabilityFailureIsStalenessToo: Poll also returns an
// error when the fold succeeded but the state file would not write. The
// totals are current in memory and WRONG on the next restart, which
// re-ingests rows already counted into an append-only ledger. Different
// failure, same broken claim — the totals this cell announces cannot be
// trusted — so it is reported the same way.
func TestPollHealth_ADurabilityFailureIsStalenessToo(t *testing.T) {
	swap := &slowSwap{rows: []ActivityRow{chatRow(1, "qwen", 100, 0, 10)}}
	url := swap.start(t)

	// A state path whose parent is a regular file: MkdirAll refuses.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "blocked")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := newClock()
	c := newCollector(t, url, func(cfg *Config) {
		cfg.StatePath = filepath.Join(notADir, "cell-usage.json")
		cfg.Now = clock.now
		cfg.Logger = slog.New(slog.NewTextHandler(&logBuffer{}, nil))
	})

	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("a poll whose state file could not be written reported success")
	}
	// The fold DID happen: this is not a lost window, and the distinction is
	// deliberate.
	if u := c.Snapshot(); u == nil || len(u.Models) != 1 || u.Models[0].Req != 1 {
		t.Fatalf("the rows were not folded: %+v", u)
	}
	clock.advance(time.Minute)
	h := c.Health()
	if h.Failures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", h.Failures)
	}
	if h.LastPoll.IsKnown() {
		t.Fatal("a poll that could not persist its cursor is not a clean poll")
	}
	if h.StaleFor < time.Minute {
		t.Fatalf("StaleFor = %s, want at least the minute since construction", h.StaleFor)
	}
	if !strings.Contains(h.LastErr, "blocked") {
		t.Fatalf("LastErr does not name the path that would not write: %q", h.LastErr)
	}
}

// logBuffer is a race-safe slog sink.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
