package usagemeter

import (
	"context"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
)

// llama-swap's DEFAULT activity store is an in-memory RING capped at 1000
// rows while ids keep climbing — id 93,447 with total 1000 was the live
// reading on the box these fixtures came from. A persistent store needs a
// `store:` block with a path, which the fleet's private config supplies and
// which nothing in this repo can assume.
//
// The existing tests in this package hand-build their pages by marshalling
// activityPage and ActivityRow — the production types — so a json tag typo
// cancels out and the ring's own boundaries have never executed. These
// drive the real Collector against a store that behaves like the ring.

func ringCollector(t *testing.T, cell *swaptest.Cell, maxPages int) *Collector {
	t.Helper()
	c, err := New(Config{
		LlamaSwapURL: cell.URL(),
		StatePath:    t.TempDir() + "/usage.json",
		MaxPages:     maxPages,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func fill(cell *swaptest.Cell, n int) {
	for range n {
		cell.Request("qwen3.6-27b", "/v1/chat/completions",
			swaptest.Tokens{Cache: 100, Draft: -1, DraftAcc: -1, Input: 10, Output: 5}, 900*time.Millisecond)
	}
}

// TestFullRingPaginationBoundary walks a FULL 1000-row ring at
// llama-swap's own per-page maximum of 999. The second page holds exactly
// one row, and both pages have to be read for the fold to be complete —
// the off-by-one that drops it is invisible on any store smaller than the
// cap, which is every store this package has ever been tested against.
func TestFullRingPaginationBoundary(t *testing.T) {
	cell := swaptest.NewCell(t,
		swaptest.WithRingCap(swaptest.DefaultRingCap),
		swaptest.WithFirstID(93000),
		swaptest.WithModels(swaptest.Model{ID: "qwen3.6-27b", State: "ready"}))
	fill(cell, swaptest.DefaultRingCap)

	if got := len(cell.Rows()); got != swaptest.DefaultRingCap {
		t.Fatalf("ring holds %d rows, want %d", got, swaptest.DefaultRingCap)
	}

	c := ringCollector(t, cell, 0)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	snap := c.Snapshot()
	if snap == nil {
		t.Fatal("no usage snapshot after folding a full ring")
	}
	var req int64
	for _, m := range snap.Models {
		req += m.Req + m.PokeReq
	}
	if req != int64(swaptest.DefaultRingCap) {
		t.Errorf("folded %d requests off a full ring, want %d — the 999/1 page boundary is the only place this can go missing",
			req, swaptest.DefaultRingCap)
	}
}

// TestEvictionDuringPaginatedWalk is the race a ring cannot avoid and
// nothing but a fake can schedule: rows aging out BETWEEN page 1 and page 2
// of the same walk. OFFSET pagination shifts under the reader, so the walk
// sees fewer distinct ids than the (cursor, maxID] span promises.
//
// The requirement is not that the count survives — it cannot. It is that
// the shortfall is REPORTED. C7a's ledger is append-only, so a silently
// under-counted window can never be corrected, and a silent gap is
// indistinguishable from an idle cell.
func TestEvictionDuringPaginatedWalk(t *testing.T) {
	cell := swaptest.NewCell(t,
		swaptest.WithRingCap(swaptest.DefaultRingCap),
		swaptest.WithFirstID(93000),
		swaptest.WithModels(swaptest.Model{ID: "qwen3.6-27b", State: "ready"}))
	fill(cell, swaptest.DefaultRingCap)

	// Page 1 of Poll is the 1-row head probe; page 2 is the first real
	// page. Evict after two reads, i.e. between the real pages.
	cell.EvictDuringWalk(2, 400)

	c := ringCollector(t, cell, 0)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	snap := c.Snapshot()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	var req int64
	for _, m := range snap.Models {
		req += m.Req + m.PokeReq
	}
	if req == int64(swaptest.DefaultRingCap) {
		t.Skip("the walk completed before the eviction landed; nothing to assert")
	}
	if snap.LostRows <= 0 {
		t.Fatalf("folded %d of %d rows and reported lost_rows=0 — an under-count with no evidence is indistinguishable from an idle cell, and the ledger is append-only",
			req, swaptest.DefaultRingCap)
	}
	if req+snap.LostRows < int64(swaptest.DefaultRingCap) {
		t.Errorf("folded %d + lost %d = %d, short of the %d ids the span covers",
			req, snap.LostRows, req+snap.LostRows, swaptest.DefaultRingCap)
	}
}

// TestIdsRestartMintsANewEpoch drives the DOWNWARD discontinuity through
// the real collector against a store that actually restarts: a llama-swap
// dropped back onto a fresh in-memory ring. Without the epoch rule a cell
// whose cursor sits at 93,000 reads zero forever and looks idle.
func TestIdsRestartMintsANewEpoch(t *testing.T) {
	cell := swaptest.NewCell(t,
		swaptest.WithFirstID(93000),
		swaptest.WithModels(swaptest.Model{ID: "qwen3.6-27b", State: "ready"}))
	fill(cell, 5)

	c := ringCollector(t, cell, 0)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	first := c.Snapshot()
	if first == nil || first.Epoch == "" {
		t.Fatal("no epoch after the first poll")
	}

	// The restart: same cell, empty store, ids from 1.
	cell.ClearRows()
	cell.SetNextID(1)
	fill(cell, 3)

	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	second := c.Snapshot()
	if second.Epoch == first.Epoch {
		t.Fatal("the id restart did not mint a new epoch; the cell now reads as idle until its ids climb past 93,000 again")
	}
	var req int64
	for _, m := range second.Models {
		req += m.Req + m.PokeReq
	}
	if req != 3 {
		t.Errorf("after the restart the collector folded %d requests, want the 3 on the new store", req)
	}
}

// TestRealisticRowShapeClassifies drives the classifier over rows the
// double produced, including the -1 sentinels and the 4xx row. It is the
// same assertion the conformance suite makes, restated where this package's
// own maintainers will see it.
func TestRealisticRowShapeClassifies(t *testing.T) {
	cell := swaptest.NewCell(t,
		swaptest.WithFirstID(93000),
		swaptest.WithModels(
			swaptest.Model{ID: "qwen3.6-27b", State: "ready"},
			swaptest.Model{ID: "bge-small", State: "ready"}))

	cell.Request("qwen3.6-27b", "/v1/chat/completions",
		swaptest.Tokens{Cache: 243, Draft: 1634, DraftAcc: 1562, Input: 2811, Output: 1984,
			PromptPerSecond: 2451.17, TokensPerSecond: 174.82}, 12691*time.Millisecond)
	cell.Request("bge-small", "/v1/embeddings",
		swaptest.Tokens{Cache: swaptest.NotReported, Draft: swaptest.NotReported,
			DraftAcc: swaptest.NotReported, Input: 24}, 30*time.Millisecond)
	cell.Fail("swaptest-no-such-model", "/v1/chat/completions", 404, "no router for requested model")

	c := ringCollector(t, cell, 0)
	if err := c.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	snap := c.Snapshot()
	byBasis := map[string]int64{}
	var errReq int64
	for _, m := range snap.Models {
		byBasis[m.Basis] += m.InFresh + m.InCached
		errReq += m.ErrReq
	}
	// The chat row's billable input is input + max(cache,0) = 2811 + 243.
	if byBasis[BasisChat] != 3054 {
		t.Errorf("chat basis billed %d input tokens, want 3054 (input 2811 + cache 243)", byBasis[BasisChat])
	}
	// The embed row's figure already includes cache; adding the -1
	// sentinel, or the cache figure at all, is the 1.8x-5x over-count.
	if byBasis[BasisEmbed] != 24 {
		t.Errorf("embed basis billed %d input tokens, want 24 — the -1 sentinel must never enter a sum", byBasis[BasisEmbed])
	}
	if errReq != 1 {
		t.Errorf("err_req = %d, want 1: a refused request still logs a row and it must be counted as an error, not as zero traffic", errReq)
	}
}
