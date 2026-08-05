package usagemeter

// C7a cell-side coverage: the req_path token-semantics branch (the
// 1.8x-5x error), the -1 sentinels, the three visible corrections
// (poke / unmeasured / error), the id-reset epoch rule, and cursor
// durability across restarts.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSwap serves llama-swap v239's GET /api/metrics/activity contract:
// newest-id-first, paginated, limit capped at 999.
type fakeSwap struct {
	mu   sync.Mutex
	rows []ActivityRow // ascending by id
}

func (f *fakeSwap) set(rows []ActivityRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

func (f *fakeSwap) start(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/metrics/activity" {
			http.NotFound(w, r)
			return
		}
		limit := 25
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)

		f.mu.Lock()
		desc := make([]ActivityRow, len(f.rows))
		for i, row := range f.rows {
			desc[len(f.rows)-1-i] = row
		}
		total := len(f.rows)
		f.mu.Unlock()

		totalPages := (total + limit - 1) / limit
		lo := (page - 1) * limit
		hi := min(lo+limit, total)
		var out []ActivityRow
		if lo < total {
			out = desc[lo:hi]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(activityPage{Data: out, Page: page, Limit: limit, Total: total, TotalPages: totalPages})
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func chatRow(id int64, model string, in, cache, out int64) ActivityRow {
	return ActivityRow{
		ID: id, Model: model, ReqPath: "/v1/chat/completions", RespStatusCode: 200,
		DurationMs: 100,
		Tokens: Tokens{
			InputTokens: in, CacheTokens: cache, OutputTokens: out,
			DraftTokens: -1, DraftAccTokens: -1, TokensPerSecond: 42, PromptPerSecond: 900,
		},
	}
}

func newCollector(t *testing.T, url string, opts ...func(*Config)) *Collector {
	t.Helper()
	cfg := Config{
		LlamaSwapURL: url,
		StatePath:    filepath.Join(t.TempDir(), "cell-usage.json"),
		NewEpoch:     func() string { return "epoch1" },
	}
	for _, o := range opts {
		o(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func totalsFor(t *testing.T, c *Collector, model, basis string) Counters {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totals[entryKey{Model: model, Basis: basis}]
}

// A chat row's input_tokens is llama.cpp's timings.prompt_n, which is
// cache-MISS only; an embed row's is the OpenAI usage prompt figure,
// which already includes anything cached. Adding cache_tokens on the
// second is the double-count the whole phase turns on.
func TestClassify_ChatAddsCacheAndEmbedDoesNot(t *testing.T) {
	_, chat := Classify(chatRow(1, "m", 300, 19700, 500))
	if chat.InFresh != 300 || chat.InCached != 19700 {
		t.Errorf("chat basis: in_fresh=%d in_cached=%d, want 300/19700", chat.InFresh, chat.InCached)
	}

	embedBasis, embed := Classify(ActivityRow{
		ID: 2, Model: "bge", ReqPath: "/v1/embeddings", RespStatusCode: 200,
		Tokens: Tokens{InputTokens: 20000, CacheTokens: 19700, OutputTokens: 0, DraftTokens: -1},
	})
	if embedBasis != BasisEmbed {
		t.Fatalf("basis = %q, want %q", embedBasis, BasisEmbed)
	}
	if embed.InFresh != 20000 {
		t.Errorf("embed in_fresh = %d, want 20000", embed.InFresh)
	}
	if embed.InCached != 0 {
		t.Errorf("embed in_cached = %d, want 0 (adding it double-counts the cached prompt)", embed.InCached)
	}
}

// -1 is llama-swap's not-reported sentinel for cache_tokens. Summed raw
// it subtracts a phantom token per row.
func TestClassify_NegativeCacheSentinelClampsToZero(t *testing.T) {
	_, c := Classify(chatRow(1, "m", 500, -1, 200))
	if c.InCached != 0 {
		t.Errorf("in_cached = %d, want 0 (the -1 sentinel must never enter a sum)", c.InCached)
	}
	if c.InFresh != 500 {
		t.Errorf("in_fresh = %d, want 500", c.InFresh)
	}
}

// mlx answers chat paths from the OpenAI usage object: the prompt figure
// is already complete and no cache figure is reported, so the chat
// arithmetic degenerates to the right answer without an mlx branch.
func TestClassify_MLXStyleChatRowNeedsNoBackendBranch(t *testing.T) {
	_, c := Classify(ActivityRow{
		ID: 1, Model: "qwen-mlx", ReqPath: "/v1/chat/completions", RespStatusCode: 200,
		Tokens: Tokens{InputTokens: 1234, CacheTokens: -1, OutputTokens: 77, DraftTokens: -1},
	})
	if c.InFresh != 1234 || c.InCached != 0 {
		t.Errorf("in_fresh=%d in_cached=%d, want 1234/0", c.InFresh, c.InCached)
	}
}

// Speculative decoding changes how output tokens were produced, not how
// many the model emitted: predicted_n already counts accepted drafts.
func TestClassify_DraftTokensNeverEnterAnySum(t *testing.T) {
	plain := chatRow(1, "m", 400, 100, 250)
	spec := chatRow(2, "m", 400, 100, 250)
	spec.Tokens.DraftTokens = 900
	spec.Tokens.DraftAccTokens = 700

	_, a := Classify(plain)
	_, b := Classify(spec)
	if a != b {
		t.Errorf("speculative row accumulated differently:\n plain=%+v\n  spec=%+v", a, b)
	}

	sentinel := chatRow(3, "m", 400, 100, 250)
	sentinel.Tokens.DraftTokens = -1
	sentinel.Tokens.DraftAccTokens = -1
	_, cS := Classify(sentinel)
	if cS != a {
		t.Errorf("draft -1 sentinel changed the totals: %+v vs %+v", cS, a)
	}
}

// A 200 that reported no tokens is unmeasured: mlx streaming and every
// client-cancelled stream. It is counted and never summed as zero, and
// it is NOT estimated from duration_ms x tokens_per_second.
func TestClassify_ZeroTokenTwoHundredIsUnmeasuredNotZero(t *testing.T) {
	row := ActivityRow{
		ID: 1, Model: "m", ReqPath: "/v1/chat/completions", RespStatusCode: 200,
		DurationMs: 45000,
		Tokens:     Tokens{TokensPerSecond: -1, PromptPerSecond: -1, CacheTokens: -1, DraftTokens: -1},
	}
	_, c := Classify(row)
	if c.UnmeasuredReq != 1 {
		t.Errorf("unmeasured_req = %d, want 1", c.UnmeasuredReq)
	}
	if c.Req != 0 {
		t.Errorf("req = %d, want 0 (an unmeasured row must not enter the requests figure)", c.Req)
	}
	if c.InFresh != 0 || c.Out != 0 || c.BusyMS != 0 {
		t.Errorf("unmeasured row contributed magnitudes: %+v", c)
	}
}

func TestClassify_NonTwoHundredIsErrorAndContributesNothing(t *testing.T) {
	row := chatRow(1, "m", 500, 100, 200)
	row.RespStatusCode = 503
	_, c := Classify(row)
	if c.ErrReq != 1 {
		t.Errorf("err_req = %d, want 1", c.ErrReq)
	}
	if c.Req != 0 || c.InFresh != 0 || c.Out != 0 || c.UnmeasuredReq != 0 {
		t.Errorf("error row contributed to a measured figure: %+v", c)
	}
}

// C4's warm targets, warm schedules and warm_model all POST a real
// 1-token completion. llama-swap tags them with nothing, so output <= 1
// on a chat row is the discriminator.
func TestClassify_OneTokenChatRowIsAPokeExcludedFromBillableSums(t *testing.T) {
	_, poke := Classify(chatRow(1, "m", 12, 0, 1))
	if poke.PokeReq != 1 {
		t.Errorf("poke_req = %d, want 1", poke.PokeReq)
	}
	if poke.Req != 0 || poke.InFresh != 0 || poke.Out != 0 {
		t.Errorf("poke contributed to a billable figure: %+v", poke)
	}
	// An embedding that emits no output tokens is NOT a poke.
	basis, embed := Classify(ActivityRow{
		ID: 2, Model: "bge", ReqPath: "/v1/embeddings", RespStatusCode: 200,
		Tokens: Tokens{InputTokens: 800, CacheTokens: -1, DraftTokens: -1},
	})
	if basis != BasisEmbed || embed.PokeReq != 0 || embed.Req != 1 || embed.InFresh != 800 {
		t.Errorf("embed row misclassified: basis=%q %+v", basis, embed)
	}
}

func TestBasisFor_BranchesOnPathIncludingUpstreamAndVersionlessRoutes(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": BasisChat,
		"/v1/completions":      BasisChat,
		"/v1/messages":         BasisChat,
		"/infill":              BasisChat,
		"/v/chat/completions":  BasisChat,
		"/upstream/qwen3.6-27b/v1/chat/completions": BasisChat,
		"/v1/embeddings":            BasisEmbed,
		"/v1/rerank":                BasisEmbed,
		"/reranking":                BasisEmbed,
		"/v/embeddings":             BasisEmbed,
		"/v1/audio/speech":          BasisOther,
		"/props":                    BasisOther,
		"/v1/messages/count_tokens": BasisOther,
	}
	for path, want := range cases {
		if got := BasisFor(path); got != want {
			t.Errorf("BasisFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPoll_AccumulatesCumulativelyAndAdvancesTheCursor(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	swap.set([]ActivityRow{chatRow(1, "qwen", 100, 900, 50), chatRow(2, "qwen", 200, 800, 60)})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	got := totalsFor(t, c, "qwen", BasisChat)
	if got.Req != 2 || got.InFresh != 300 || got.InCached != 1700 || got.Out != 110 {
		t.Fatalf("after poll 1: %+v", got)
	}

	// A second poll over the same rows must be a no-op: the cursor, not
	// the row set, is what decides.
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if again := totalsFor(t, c, "qwen", BasisChat); again != got {
		t.Fatalf("re-poll double-counted: %+v then %+v", got, again)
	}

	swap.set([]ActivityRow{chatRow(1, "qwen", 100, 900, 50), chatRow(2, "qwen", 200, 800, 60), chatRow(3, "qwen", 10, 0, 5)})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	got = totalsFor(t, c, "qwen", BasisChat)
	if got.Req != 3 || got.InFresh != 310 || got.Out != 115 {
		t.Fatalf("after poll 3: %+v", got)
	}
}

// SQLite AUTOINCREMENT restarts at 1 on a fresh in-memory store. Without
// the reset rule a cell whose cursor sits high reads zero forever and
// looks exactly like an idle cell.
func TestPoll_IDResetMintsANewEpochAndReingests(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	epochs := []string{"e1", "e2"}
	i := 0
	c := newCollector(t, url, func(cfg *Config) {
		cfg.NewEpoch = func() string {
			e := epochs[min(i, len(epochs)-1)]
			i++
			return e
		}
	})

	swap.set([]ActivityRow{chatRow(500, "qwen", 100, 0, 50), chatRow(501, "qwen", 100, 0, 50)})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if snap := c.Snapshot(); snap.Epoch != "e1" {
		t.Fatalf("epoch = %q, want e1", snap.Epoch)
	}

	// llama-swap restarted on an in-memory store: ids begin again at 1.
	swap.set([]ActivityRow{chatRow(1, "qwen", 7, 0, 3)})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	snap := c.Snapshot()
	if snap.Epoch != "e2" {
		t.Fatalf("epoch = %q, want e2 (a new epoch on id reset)", snap.Epoch)
	}
	if len(snap.Models) != 1 || snap.Models[0].InFresh != 7 || snap.Models[0].Req != 1 {
		t.Fatalf("post-reset totals restart from the new epoch's rows: %+v", snap.Models)
	}
}

func TestPoll_ShortfallIsReportedAsLostRowsNotAbsorbed(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	// One page of 3 rows max: ids 1..2 exist, but the log claims a max id
	// of 40 — rows 3..40 aged out of the in-memory ring.
	c := newCollector(t, url, func(cfg *Config) { cfg.MaxPages = 1 })

	rows := []ActivityRow{chatRow(39, "qwen", 5, 0, 5), chatRow(40, "qwen", 5, 0, 5)}
	swap.set(rows)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	snap := c.Snapshot()
	if snap.LostRows != 38 {
		t.Errorf("lost_rows = %d, want 38 (max_id 40 - cursor 0 - 2 rows read)", snap.LostRows)
	}
}

func TestCollector_CursorAndTotalsSurviveARestart(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	statePath := filepath.Join(t.TempDir(), "cell-usage.json")

	swap.set([]ActivityRow{chatRow(1, "qwen", 100, 0, 50)})
	c1, err := New(Config{LlamaSwapURL: url, StatePath: statePath, NewEpoch: func() string { return "e1" }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c1.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	c2, err := New(Config{LlamaSwapURL: url, StatePath: statePath, NewEpoch: func() string { return "e-should-not-be-used" }})
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	snap := c2.Snapshot()
	if snap == nil || snap.Epoch != "e1" {
		t.Fatalf("epoch lost across restart: %+v", snap)
	}
	if len(snap.Models) != 1 || snap.Models[0].InFresh != 100 {
		t.Fatalf("totals lost across restart: %+v", snap.Models)
	}
	// The cursor came back too: re-polling the same rows adds nothing.
	if err := c2.Poll(context.Background()); err != nil {
		t.Fatalf("poll after restart: %v", err)
	}
	if after := c2.Snapshot(); after.Models[0].InFresh != 100 {
		t.Fatalf("restart re-ingested rows already counted: %+v", after.Models)
	}
}

func TestSnapshot_NilWhenNothingHasBeenCounted(t *testing.T) {
	swap := &fakeSwap{}
	c := newCollector(t, swap.start(t))
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if snap := c.Snapshot(); snap != nil {
		t.Errorf("Snapshot() = %+v, want nil (a cell that counted nothing is unmeasured, not zero)", snap)
	}
}

func TestPollAndSnapshot_SurvivesAnUnreachableLlamaSwap(t *testing.T) {
	c := newCollector(t, "http://127.0.0.1:1")
	if snap := c.PollAndSnapshot(context.Background()); snap != nil {
		t.Errorf("unreachable llama-swap must not invent totals, got %+v", snap)
	}
}

// ─── adversarial review pass ────────────────────────────────────────────

// Poll reads the cursor, releases the lock for the HTTP round trip, then
// folds. Without a whole-poll lock two overlapping polls both ingest the
// same rows and double every count in the window — a logic race that
// -race cannot see.
func TestPoll_ConcurrentPollsDoNotDoubleCount(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	var rows []ActivityRow
	for id := int64(1); id <= 20; id++ {
		rows = append(rows, chatRow(id, "qwen", 10, 0, 5))
	}
	swap.set(rows)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Poll(context.Background())
		}()
	}
	wg.Wait()

	got := totalsFor(t, c, "qwen", BasisChat)
	if got.Req != 20 || got.InFresh != 200 || got.Out != 100 {
		t.Fatalf("concurrent polls double-counted: %+v (want req=20 in_fresh=200 out=100)", got)
	}
}

// A cell that lost every row it tried to read is not a cell that counted
// nothing: the loss has to reach fleetd.
func TestSnapshot_ReportsALossWithNoModels(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url, func(cfg *Config) { cfg.MaxPages = 1 })

	// One readable row at id 900: ids 1..899 aged out.
	swap.set([]ActivityRow{{ID: 900, Model: "qwen", ReqPath: "/v1/chat/completions", RespStatusCode: 200}})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	snap := c.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot() = nil despite a reported loss")
	}
	if snap.LostRows != 899 {
		t.Errorf("lost_rows = %d, want 899", snap.LostRows)
	}
}

// ─── second adversarial pass ────────────────────────────────────────────

// A state-dir write failure is NOT a poll failure: the rows were read and
// folded, only the cursor did not reach disk. PollAndSnapshot logs poll
// failures at debug (an llama-swap that is simply stopped is expected and
// noisy), so a durability failure logged the same way is invisible in
// production — and it is the one that makes a restart re-ingest rows
// already counted.
func TestPoll_StateWriteFailureIsWarnedNotSwallowed(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)

	// StatePath's parent is a FILE, so MkdirAll fails on every save.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c, err := New(Config{
		LlamaSwapURL: url,
		StatePath:    filepath.Join(blocked, "cell-usage.json"),
		Logger:       logger,
		NewEpoch:     func() string { return "e1" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	swap.set([]ActivityRow{chatRow(1, "qwen", 100, 0, 50)})
	if err := c.Poll(context.Background()); err == nil {
		t.Fatal("Poll returned nil despite an unwritable state path")
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "not persisted") {
		t.Errorf("a durability failure was not warned about; log was:\n%s", out)
	}

	// Fail-open: the counts are still true and still worth announcing.
	if snap := c.Snapshot(); snap == nil || len(snap.Models) != 1 || snap.Models[0].InFresh != 100 {
		t.Errorf("an unwritable state path cost the cell its counts: %+v", snap)
	}
}

// TestPoll_ModelFilterKeepsOnlyTheAskedForModels pins C7b §6's one new
// knob: fleetd tails the FRONT's activity log for cloud_peer ids only,
// because every other row there is a request some cell also counted. The
// cursor still advances past rejected rows — they were read, they just
// are not this collector's business.
func TestPoll_ModelFilterKeepsOnlyTheAskedForModels(t *testing.T) {
	swap := &fakeSwap{}
	swap.set([]ActivityRow{
		chatRow(1, "vendor-chat-1", 500, -1, 100),
		chatRow(2, "qwen-coder", 9_000_000, -1, 500_000),
		chatRow(3, "vendor-chat-1", 1000, -1, 200),
	})
	c := newCollector(t, swap.start(t), func(cfg *Config) {
		cfg.ModelFilter = func(m string) bool { return m == "vendor-chat-1" }
	})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := totalsFor(t, c, "qwen-coder", BasisChat); got.Req != 0 || got.InFresh != 0 {
		t.Errorf("a filtered-out model was counted: %+v", got)
	}
	cloud := totalsFor(t, c, "vendor-chat-1", BasisChat)
	if cloud.Req != 2 || cloud.InFresh != 1500 || cloud.Out != 300 {
		t.Errorf("cloud counters = %+v, want 2 req / 1500 in / 300 out", cloud)
	}
	c.mu.Lock()
	cursor, lost := c.cursor, c.lost
	c.mu.Unlock()
	if cursor != 3 {
		t.Errorf("cursor = %d, want 3 — filtered rows were still READ", cursor)
	}
	if lost != 0 {
		t.Errorf("lost = %d, want 0 — a filtered row is not a lost row", lost)
	}
}

// ─── the upward id jump (live gate, 2026-08-05) ─────────────────────────
//
// The epoch rule answers max_id < cursor. Its mirror — a store restored,
// copied or swapped for one whose ids sit ABOVE the cursor — was found by
// standing up a real 4-cell fleet: alpha had served 111 rows against a
// persistent store, the store was replaced by a copy with ids shifted up,
// and the collector folded every counter a second time (req 9 -> 18,
// 84 -> 168, in_fresh 878 -> 1756) into fleetd's APPEND-ONLY ledger.

func stamped(r ActivityRow, ts time.Time) ActivityRow {
	r.Timestamp = ts
	return r
}

// A log holding the same requests under higher ids is not traffic that
// arrived while nobody was reading: llama-swap stamps a row at request
// COMPLETION and inserts it then, so no single log can carry a row that
// is both newer by id and older by clock. The window is skipped whole and
// the skipped rows are reported, because an under-count is recoverable
// and a double count into an append-only ledger is not.
func TestPoll_LogWhoseRowsPredateTheCursorIsSkippedNotFoldedAgain(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	var rows []ActivityRow
	for i := range 40 {
		rows = append(rows, stamped(chatRow(int64(i+1), "qwen", 100, 0, 50), base.Add(time.Duration(i)*time.Minute)))
	}
	swap.set(rows)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	want := totalsFor(t, c, "qwen", BasisChat)
	if want.Req != 40 {
		t.Fatalf("setup: req = %d, want 40", want.Req)
	}

	// The store is swapped for another holding the SAME requests with ids
	// above the cursor.
	shifted := make([]ActivityRow, 0, len(rows))
	for _, r := range rows {
		r.ID += 10_000
		shifted = append(shifted, r)
	}
	swap.set(shifted)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	if got := totalsFor(t, c, "qwen", BasisChat); got != want {
		t.Fatalf("the swapped store was folded a second time:\n before %+v\n  after %+v", want, got)
	}
	snap := c.Snapshot()
	if snap.LostRows != 40 {
		t.Errorf("lost_rows = %d, want 40 — the skipped rows have to be reported, not absorbed", snap.LostRows)
	}
	if snap.Epoch != "epoch1" {
		t.Errorf("epoch = %q, want epoch1 — the cell's cumulative totals stay true, only the window is refused", snap.Epoch)
	}
	c.mu.Lock()
	cursor := c.cursor
	c.mu.Unlock()
	if cursor != 10_040 {
		t.Fatalf("cursor = %d, want 10040 — refusing a window must not park the collector on a dead cursor", cursor)
	}

	// And traffic that genuinely arrives on the new store is counted from
	// there: the refusal is one window, not a stuck cell.
	swap.set(append(shifted, stamped(chatRow(10_041, "qwen", 7, 0, 3), base.Add(200*time.Minute))))
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	got := totalsFor(t, c, "qwen", BasisChat)
	if got.Req != want.Req+1 || got.InFresh != want.InFresh+7 {
		t.Fatalf("new rows on the new store were not counted: %+v (was %+v)", got, want)
	}
}

// The other shape the same swap takes: the replacement log's ids OVERLAP
// the cursor. Every timestamp here is at or after the anchor's, so the
// clock check cannot fire — what catches it is that the row at the cursor
// id is no longer the row the cursor was set from.
func TestPoll_LogWhoseCursorRowIsADifferentRequestIsSkipped(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	var rows []ActivityRow
	for i := range 5 {
		rows = append(rows, stamped(chatRow(int64(i+1), "qwen", 100, 0, 50), base.Add(time.Duration(i)*time.Minute)))
	}
	swap.set(rows)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	want := totalsFor(t, c, "qwen", BasisChat)

	// A different log occupying the same id space, all of it stamped
	// AFTER the anchor so only the identity check can object.
	var other []ActivityRow
	for i := range 8 {
		other = append(other, stamped(chatRow(int64(i+1), "bge-rewrite", 20, 0, 10), base.Add(time.Duration(10+i)*time.Minute)))
	}
	swap.set(other)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	if got := totalsFor(t, c, "bge-rewrite", BasisChat); got.Req != 0 {
		t.Fatalf("a log that replaced the cursor row was folded anyway: %+v", got)
	}
	if got := totalsFor(t, c, "qwen", BasisChat); got != want {
		t.Fatalf("the original totals moved: %+v, want %+v", got, want)
	}
	if snap := c.Snapshot(); snap.LostRows != 3 {
		t.Errorf("lost_rows = %d, want 3 (ids 6..8 were read and refused)", snap.LostRows)
	}
}

// The deliberate NON-strictness: on an in-memory ring the anchor ages out
// legitimately, and every surviving row is newer than it. That is
// unproven continuity, not contradicted continuity, and refusing it would
// drop real traffic on every cell that outruns its ring — so it folds and
// reports the aged-out span exactly as it did before.
func TestPoll_AgedOutAnchorStillFoldsAndReportsTheShortfall(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	swap.set([]ActivityRow{
		stamped(chatRow(1, "qwen", 100, 0, 50), base),
		stamped(chatRow(2, "qwen", 100, 0, 50), base.Add(time.Minute)),
		stamped(chatRow(3, "qwen", 100, 0, 50), base.Add(2*time.Minute)),
	})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}

	swap.set([]ActivityRow{
		stamped(chatRow(900, "qwen", 10, 0, 5), base.Add(3*time.Hour)),
		stamped(chatRow(901, "qwen", 10, 0, 5), base.Add(4*time.Hour)),
		stamped(chatRow(902, "qwen", 10, 0, 5), base.Add(5*time.Hour)),
	})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	got := totalsFor(t, c, "qwen", BasisChat)
	if got.Req != 6 || got.InFresh != 330 {
		t.Fatalf("a ring that aged the anchor out lost its readable rows: %+v", got)
	}
	if snap := c.Snapshot(); snap.LostRows != 896 {
		t.Errorf("lost_rows = %d, want 896 (max_id 902 - cursor 3 - 3 rows read)", snap.LostRows)
	}
}

// A state file written before the anchor existed carries none, and the
// upgrade must not cost the cell a window: with nothing to contradict,
// the first poll folds and re-establishes the anchor.
func TestPoll_StateFileWithoutAnAnchorStillFolds(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	statePath := filepath.Join(t.TempDir(), "cell-usage.json")
	if err := os.WriteFile(statePath, []byte(`{"epoch":"e1","last_row_id":2,"lost_rows":0,"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{LlamaSwapURL: url, StatePath: statePath, NewEpoch: func() string { return "unused" }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	swap.set([]ActivityRow{
		stamped(chatRow(1, "qwen", 100, 0, 50), base),
		stamped(chatRow(2, "qwen", 100, 0, 50), base.Add(time.Minute)),
		stamped(chatRow(3, "qwen", 40, 0, 20), base.Add(2*time.Minute)),
	})
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := totalsFor(t, c, "qwen", BasisChat); got.Req != 1 || got.InFresh != 40 {
		t.Fatalf("an anchorless state file cost the cell its window: %+v", got)
	}
	// The anchor is now recorded, so the very next swap is caught.
	c.mu.Lock()
	a := c.anchor
	c.mu.Unlock()
	if a == nil || a.ID != 3 {
		t.Fatalf("anchor = %+v, want the row at id 3", a)
	}
}

// Ordinary growth with real timestamps must never trip either check —
// this is the false-positive guard on a cell that is simply busy.
func TestPoll_OrdinaryGrowthWithTimestampsNeverTripsTheContinuityCheck(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	base := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	var rows []ActivityRow
	for i := range 6 {
		// Second granularity: llama-swap's timestamps repeat within a
		// second, so equal stamps across ids must not read as backwards.
		rows = append(rows, stamped(chatRow(int64(i+1), "qwen", 10, 0, 5), base.Add(time.Duration(i/2)*time.Second)))
		swap.set(rows)
		if err := c.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	got := totalsFor(t, c, "qwen", BasisChat)
	if got.Req != 6 {
		t.Fatalf("req = %d, want 6", got.Req)
	}
	if snap := c.Snapshot(); snap.LostRows != 0 {
		t.Errorf("lost_rows = %d on an ordinary growing log", snap.LostRows)
	}
}

// TestPoll_ProvenContinuityIsNotDiscardedByABackwardsClockStep.
//
// The two checks are a ladder, not a conjunction. When the row still
// sitting at the cursor id IS the row the anchor was taken from, the log
// is demonstrably the log the cursor came from and there is nothing left
// to test; the clock scan exists only for the case where that proof is
// unavailable (ids jumped clear of the cursor). Running it anyway made a
// single backwards clock step larger than maxRowClockSkew — the one thing
// that constant is documented to absorb — discard a whole window of REAL
// traffic. These counters are cumulative and fleetd's ledger is
// append-only, so the shortfall never comes back.
func TestPoll_ProvenContinuityIsNotDiscardedByABackwardsClockStep(t *testing.T) {
	swap := &fakeSwap{}
	url := swap.start(t)
	c := newCollector(t, url)

	base := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	rows := []ActivityRow{
		stamped(chatRow(1, "qwen", 100, 0, 50), base),
		stamped(chatRow(2, "qwen", 100, 0, 50), base.Add(time.Minute)),
	}
	swap.set(rows)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	before := totalsFor(t, c, "qwen", BasisChat)
	if before.Req != 2 {
		t.Fatalf("setup: req = %d, want 2", before.Req)
	}

	// Same log, same rows 1 and 2 untouched — then NTP walks the cell's
	// clock back an hour and it keeps serving.
	stepped := base.Add(-time.Hour)
	rows = append(rows,
		stamped(chatRow(3, "qwen", 30, 0, 10), stepped),
		stamped(chatRow(4, "qwen", 30, 0, 10), stepped.Add(time.Minute)),
	)
	swap.set(rows)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll 2: %v", err)
	}

	got := totalsFor(t, c, "qwen", BasisChat)
	if got.Req != 4 || got.InFresh != before.InFresh+60 {
		t.Fatalf("a window on a log whose identity is PROVEN was refused: %+v (was %+v)", got, before)
	}
	if snap := c.Snapshot(); snap.LostRows != 0 {
		t.Errorf("lost_rows = %d — nothing was lost; the anchor row was right there at the cursor", snap.LostRows)
	}
}
