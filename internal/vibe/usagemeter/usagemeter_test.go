package usagemeter

// C7a cell-side coverage: the req_path token-semantics branch (the
// 1.8x-5x error), the -1 sentinels, the three visible corrections
// (poke / unmeasured / error), the id-reset epoch rule, and cursor
// durability across restarts.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
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
