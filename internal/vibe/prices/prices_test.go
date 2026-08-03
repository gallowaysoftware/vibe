package prices

import (
	"fmt"
	"testing"
	"time"
)

// goldenTable is the FROZEN 3-row fixture C7b gate 1 specifies: one chat
// row with 4x in/out asymmetry and a cache_read, one embedding row, one
// per-query rerank row. It is written out here rather than read from the
// vendored artifact on purpose — a table refresh must not be able to
// change the arithmetic without failing this test.
func goldenTable() *Table {
	return &Table{
		Schema: SchemaVersion,
		Notice: "fixture",
		Snapshots: []Snapshot{{
			EffectiveFrom: "2026-01-01",
			Base:          true,
			Rows: []Row{
				{Provider: "host-a", Model: "acme/chat-30b", Key: "chat30b", In: 0.20, Out: 0.80, CacheRead: 0.05, Open: true},
				{Provider: "host-a", Model: "acme/embed-1", Key: "embed1", In: 0.02, Open: true, Mode: ModeEmbedding},
				{Provider: "host-a", Model: "acme/rerank-1", Key: "rerank1", PerQuery: 0.002, Open: true, Mode: ModeRerank},
			},
		}},
	}
}

func cents(t *testing.T, v float64) string {
	t.Helper()
	return fmt.Sprintf("%.2f", v)
}

// TestGoldenPricing_ExactToTheCent pins gate 1: fixed counts against the
// frozen fixture produce exact dollar amounts, so a vendored-table
// refresh cannot silently change the arithmetic.
func TestGoldenPricing_ExactToTheCent(t *testing.T) {
	res := goldenTable().At("2026-06-01")

	// chat: 1M fresh @ 0.20 + 12M cached @ 0.05 + 0.5M out @ 0.80
	//     = 0.20 + 0.60 + 0.40 = 1.20
	chat, ok := PriceAcross(res.Hosts("acme/chat-30b", true), Counts{
		Basis: BasisChat, Req: 100, InFresh: 1_000_000, InCached: 12_000_000, Out: 500_000,
	})
	if !ok {
		t.Fatalf("chat row did not price: %s", chat.Reason)
	}
	if got := cents(t, chat.Median); got != "1.20" {
		t.Errorf("chat cost = $%s, want $1.20", got)
	}

	// embedding: 3M prompt @ 0.02 = 0.06, and NO output charge.
	embed, ok := PriceAcross(res.Hosts("acme/embed-1", true), Counts{
		Basis: BasisEmbed, Req: 50, InFresh: 3_000_000, Out: 0,
	})
	if !ok {
		t.Fatalf("embedding row did not price: %s", embed.Reason)
	}
	if got := cents(t, embed.Median); got != "0.06" {
		t.Errorf("embedding cost = $%s, want $0.06", got)
	}

	// rerank: billed per search, 250 searches @ 0.002 = 0.50. Its token
	// price upstream is literally 0, so pricing its tokens would price it
	// free.
	rerank, ok := PriceAcross(res.Hosts("acme/rerank-1", true), Counts{
		Basis: BasisEmbed, Req: 250, InFresh: 9_000_000,
	})
	if !ok {
		t.Fatalf("rerank row did not price: %s", rerank.Reason)
	}
	if got := cents(t, rerank.Median); got != "0.50" {
		t.Errorf("rerank cost = $%s, want $0.50", got)
	}
}

// TestZeroRateIsUnpricedNotFree pins gate 1's second assertion: a row
// carrying a price of exactly 0 renders "unpriced", never "$0.00 saved".
func TestZeroRateIsUnpricedNotFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  Row
		c    Counts
	}{
		{"chat with no input rate", Row{Provider: "h", Model: "m", Key: "m", In: 0, Out: 0.8, Open: true}, Counts{Basis: BasisChat, InFresh: 1e6, Out: 1e5}},
		{"chat with no output rate", Row{Provider: "h", Model: "m", Key: "m", In: 0.2, Out: 0, Open: true}, Counts{Basis: BasisChat, InFresh: 1e6, Out: 1e5}},
		{"embedding with no rate", Row{Provider: "h", Model: "m", Key: "m", Mode: ModeEmbedding, Open: true}, Counts{Basis: BasisEmbed, InFresh: 1e6}},
		{"rerank with no per-query rate", Row{Provider: "h", Model: "m", Key: "m", Mode: ModeRerank, Open: true}, Counts{Basis: BasisEmbed, Req: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, ok, why := tc.row.Price(tc.c)
			if ok {
				t.Fatalf("priced a zero rate as $%.2f; zero means unknown, not free", q.Cost)
			}
			if why != ReasonZeroRate {
				t.Errorf("reason = %q, want %q", why, ReasonZeroRate)
			}
			sp, ok := PriceAcross([]Row{tc.row}, tc.c)
			if ok {
				t.Fatalf("spread priced a zero rate as $%.2f", sp.Median)
			}
			if sp.Reason == "" {
				t.Error("unpriced spread carries no reason")
			}
		})
	}
}

// TestCacheTier_SplitVsSingleRate pins gate 2's first half: pricing the
// same counts with and without the fresh/cache-read split differs by the
// expected order of magnitude (~5x on a realistic 92%-cached corpus).
func TestCacheTier_SplitVsSingleRate(t *testing.T) {
	row := Row{Provider: "h", Model: "m", Key: "m", In: 0.20, CacheRead: 0.02, Out: 0.80, Open: true}
	c := Counts{Basis: BasisChat, InFresh: 1_000_000, InCached: 11_500_000, Out: 200_000}

	split, ok, _ := row.Price(c)
	if !ok {
		t.Fatal("split pricing failed")
	}
	// 0.20 + 0.23 + 0.16
	if got := cents(t, split.Cost); got != "0.59" {
		t.Errorf("split cost = $%s, want $0.59", got)
	}

	// The common real case AND the wrong way at once: a host that
	// publishes no cache rate bills every prompt token at the full input
	// rate, and must say so.
	naiveRow := row
	naiveRow.CacheRead = 0
	naive, ok, _ := naiveRow.Price(c)
	if !ok {
		t.Fatal("single-rate pricing failed")
	}
	// 2.50 + 0.16
	if got := cents(t, naive.Cost); got != "2.66" {
		t.Errorf("single-rate cost = $%s, want $2.66", got)
	}
	ratio := naive.Cost / split.Cost
	if ratio < 4 || ratio > 6 {
		t.Errorf("single-rate/split ratio = %.2fx, want the documented ~5x band (4x-6x)", ratio)
	}
	if !naive.NoCacheDiscount {
		t.Error("a row with no published cache rate must be flagged NoCacheDiscount")
	}
	if split.NoCacheDiscount {
		t.Error("a row WITH a published cache rate must not be flagged NoCacheDiscount")
	}
}

// TestBasisChangesTheBill pins gate 2's second half: identical raw counts
// price DIFFERENTLY depending on the basis, because the meaning of
// in_fresh inverts by endpoint (chat: cache-miss only; embed/other: the
// whole prompt, cache included).
func TestBasisChangesTheBill(t *testing.T) {
	row := Row{Provider: "h", Model: "m", Key: "m", In: 0.20, CacheRead: 0.02, Out: 0.80, Open: true}
	c := Counts{InFresh: 1_000_000, InCached: 12_000_000, Out: 0}

	chat := c
	chat.Basis = BasisChat
	chatQ, ok, why := row.Price(chat)
	if !ok {
		t.Fatalf("chat basis did not price: %s", why)
	}
	// 1M fresh @ 0.20 + 12M cached @ 0.02
	if got := cents(t, chatQ.Cost); got != "0.44" {
		t.Errorf("chat-basis cost = $%s, want $0.44", got)
	}

	other := c
	other.Basis = BasisOther
	otherQ, ok, why := row.Price(other)
	if !ok {
		t.Fatalf("other basis did not price: %s", why)
	}
	// The prompt figure already includes the cache; adding it would
	// double-count.
	if got := cents(t, otherQ.Cost); got != "0.20" {
		t.Errorf("other-basis cost = $%s, want $0.20", got)
	}
	if chatQ.Cost <= otherQ.Cost {
		t.Error("identical counts priced the same across bases; the basis branch is not doing anything")
	}
	if CachedIsBillable(BasisEmbed) || CachedIsBillable(BasisOther) {
		t.Error("embed/other bases must not bill cached tokens a second time")
	}
	if !CachedIsBillable(BasisChat) || !CachedIsBillable(BasisCloud) || !CachedIsBillable("something-newer") {
		t.Error("chat, cloud and unknown-newer bases bill fresh + cached")
	}
}

// TestModeMismatchRefusesToPrice: a chat workload priced against an
// embedding row (or the reverse) is a config error, and inventing a
// number for it would be worse than saying so.
func TestModeMismatchRefusesToPrice(t *testing.T) {
	embedRow := Row{Provider: "h", Model: "m", Key: "m", In: 0.02, Mode: ModeEmbedding, Open: true}
	if _, ok, why := embedRow.Price(Counts{Basis: BasisChat, InFresh: 1e6, Out: 1e5}); ok || why != ReasonModeMismatch {
		t.Errorf("chat counts against an embedding row: ok=%v why=%q", ok, why)
	}
	chatRow := Row{Provider: "h", Model: "m", Key: "m", In: 0.2, Out: 0.8, Open: true}
	if _, ok, why := chatRow.Price(Counts{Basis: BasisEmbed, InFresh: 1e6}); ok || why != ReasonModeMismatch {
		t.Errorf("embed counts against a chat row: ok=%v why=%q", ok, why)
	}
}

// TestDatedSnapshots_PriceTheDayNotToday pins gate 4: a price cut does
// not retroactively rewrite history.
func TestDatedSnapshots_PriceTheDayNotToday(t *testing.T) {
	tbl := &Table{
		Schema: SchemaVersion,
		Snapshots: []Snapshot{
			{EffectiveFrom: "2026-01-01", Base: true, Rows: []Row{
				{Provider: "h", Model: "acme/chat", Key: "chat", In: 3.00, Out: 15.00, Open: true},
			}},
			{EffectiveFrom: "2026-06-01", Rows: []Row{
				{Provider: "h", Model: "acme/chat", Key: "chat", In: 1.00, Out: 5.00, Open: true},
			}},
		},
	}
	c := Counts{Basis: BasisChat, InFresh: 1_000_000, Out: 1_000_000}

	before, ok := PriceAcross(tbl.At("2026-05-31").Hosts("acme/chat", true), c)
	if !ok {
		t.Fatal("pre-cut day did not price")
	}
	if got := cents(t, before.Median); got != "18.00" {
		t.Errorf("2026-05-31 cost = $%s, want $18.00 (the rate in effect that day)", got)
	}
	after, ok := PriceAcross(tbl.At("2026-06-01").Hosts("acme/chat", true), c)
	if !ok {
		t.Fatal("post-cut day did not price")
	}
	if got := cents(t, after.Median); got != "6.00" {
		t.Errorf("2026-06-01 cost = $%s, want $6.00", got)
	}
	if got := tbl.At("2026-05-31").EffectiveFrom; got != "2026-01-01" {
		t.Errorf("resolved snapshot = %s, want the base", got)
	}
	// A day older than the base still prices, at the base, and says so.
	old := tbl.At("2025-01-01")
	if !old.BeforeBase {
		t.Error("a day older than the base must be flagged BeforeBase")
	}
	if len(old.Hosts("acme/chat", true)) != 1 {
		t.Error("a pre-base day must still price at the base rather than reporting zero savings")
	}
}

// TestOverlayRemovesRows: an overlay's `removed` list drops a host, and
// only from that day forward.
func TestOverlayRemovesRows(t *testing.T) {
	tbl := &Table{
		Schema: SchemaVersion,
		Snapshots: []Snapshot{
			{EffectiveFrom: "2026-01-01", Base: true, Rows: []Row{
				{Provider: "a", Model: "m", Key: "m", In: 1, Out: 2, Open: true},
				{Provider: "b", Model: "m", Key: "m", In: 3, Out: 4, Open: true},
			}},
			{EffectiveFrom: "2026-02-01", Removed: []string{"b\x00m"}},
		},
	}
	if n := len(tbl.At("2026-01-15").Hosts("m", true)); n != 2 {
		t.Errorf("hosts before the removal = %d, want 2", n)
	}
	if n := len(tbl.At("2026-02-15").Hosts("m", true)); n != 1 {
		t.Errorf("hosts after the removal = %d, want 1", n)
	}
}

// TestSpread_MedianMinMaxAcrossHosts: the headline is the median across
// real hosts and the range is real, because fake precision on a number
// this soft is the actual failure mode.
func TestSpread_MedianMinMaxAcrossHosts(t *testing.T) {
	rows := []Row{
		{Provider: "a", Model: "m", Key: "m", In: 0.10, Out: 0.10, Open: true},
		{Provider: "b", Model: "m", Key: "m", In: 0.20, Out: 0.20, Open: true},
		{Provider: "c", Model: "m", Key: "m", In: 0.30, Out: 0.30, Open: true},
	}
	sp, ok := PriceAcross(rows, Counts{Basis: BasisChat, InFresh: 1_000_000})
	if !ok {
		t.Fatal("spread did not price")
	}
	if sp.N != 3 {
		t.Errorf("n = %d, want 3", sp.N)
	}
	if cents(t, sp.Median) != "0.20" || cents(t, sp.Min) != "0.10" || cents(t, sp.Max) != "0.30" {
		t.Errorf("median/min/max = %.2f/%.2f/%.2f, want 0.20/0.10/0.30", sp.Median, sp.Min, sp.Max)
	}
	if sp.InRateMin != 0.10 || sp.InRateMax != 0.30 {
		t.Errorf("rate range = %g-%g, want 0.10-0.30", sp.InRateMin, sp.InRateMax)
	}
	// An even host count averages the two middle prices rather than
	// silently taking whichever sorted first.
	even, _ := PriceAcross(rows[:2], Counts{Basis: BasisChat, InFresh: 1_000_000})
	if got := cents(t, even.Median); got != "0.15" {
		t.Errorf("even-count median = $%s, want $0.15", got)
	}
}

// TestOpenOnlyFilter: the twin median never quotes a closed-weight row.
// That filter IS the 72x.
func TestOpenOnlyFilter(t *testing.T) {
	res := (&Table{Schema: SchemaVersion, Snapshots: []Snapshot{{EffectiveFrom: "2026-01-01", Base: true, Rows: []Row{
		{Provider: "open-host", Model: "m", Key: "m", In: 0.2, Out: 0.8, Open: true},
		{Provider: "closed-host", Model: "m", Key: "m", In: 15, Out: 75},
	}}}}).At("2026-02-01")
	if n := len(res.Hosts("m", true)); n != 1 {
		t.Errorf("open-only hosts = %d, want 1", n)
	}
	if n := len(res.Hosts("m", false)); n != 2 {
		t.Errorf("all hosts = %d, want 2", n)
	}
}

// TestNormalizeJoinsTheSameModelAcrossHosts: every host spells an
// open-weight model differently, and the median depends on them joining.
func TestNormalizeJoinsTheSameModelAcrossHosts(t *testing.T) {
	want := Normalize("Qwen/Qwen3-Coder-30B-A3B-Instruct")
	for _, spelling := range []string{
		"accounts/fireworks/models/qwen3-coder-30b-a3b-instruct",
		"Qwen3 Coder 30B A3B Instruct",
		"qwen3-coder-30b-a3b-instruct",
	} {
		if got := Normalize(spelling); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", spelling, got, want)
		}
	}
	if Normalize("gpt-oss-120b") == Normalize("gpt-oss-20b") {
		t.Error("Normalize collapsed two different models")
	}
}

// TestDeprecatedRowsNeverPrice: a deprecated row stays in the artifact so
// an older day can still be priced, but it never quotes today.
func TestDeprecatedRowsNeverPrice(t *testing.T) {
	res := (&Table{Schema: SchemaVersion, Snapshots: []Snapshot{{EffectiveFrom: "2026-01-01", Base: true, Rows: []Row{
		{Provider: "h", Model: "m", Key: "m", In: 1, Out: 2, Open: true, Deprecated: true},
	}}}}).At("2026-02-01")
	if n := len(res.Hosts("m", true)); n != 0 {
		t.Errorf("deprecated hosts quoted = %d, want 0", n)
	}
}

// TestEmbeddedTableIsReal guards the vendored artifact: it parses, it
// carries its licences and upstream commits, and it is not the synthetic
// example table. (If a future run ever has no network and ships the
// example instead, this test is where that becomes visible.)
func TestEmbeddedTableIsReal(t *testing.T) {
	tbl, err := Embedded()
	if err != nil {
		t.Fatalf("embedded price table does not parse: %v", err)
	}
	if len(tbl.Snapshots) == 0 || len(tbl.Snapshots[0].Rows) == 0 {
		t.Fatal("embedded price table is empty")
	}
	if tbl.Example {
		t.Error("the vendored table is EXAMPLE DATA — re-run `vibe fleet prices vendor` on a networked box")
	}
	if len(tbl.Licences) == 0 {
		t.Error("the vendored table carries no upstream licences")
	}
	for _, s := range tbl.Snapshots[0].Sources {
		if s.Licence == "" {
			t.Errorf("source %s carries no licence", s.Name)
		}
		if s.Commit == "" {
			t.Errorf("source %s carries no upstream commit", s.Name)
		}
	}
	// Rows a fleet actually needs: open-weight chat hosts to take a
	// median over, and the embedding/rerank modes that only LiteLLM
	// carries.
	res := tbl.At(tbl.AsOf())
	open, embed, rerank := 0, 0, 0
	for _, rows := range res.byKey {
		for _, r := range rows {
			if r.Open {
				open++
			}
			switch r.Mode {
			case ModeEmbedding:
				embed++
			case ModeRerank:
				rerank++
			}
		}
	}
	if open == 0 || embed == 0 || rerank == 0 {
		t.Errorf("vendored table coverage: open=%d embedding=%d rerank=%d, want all non-zero", open, embed, rerank)
	}
	if tbl.Stale(mustDay(t, tbl.AsOf())) {
		t.Error("the table reports itself stale on its own as_of date")
	}
}

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestStaleAfterNinetyDays: a stale table must be VISIBLE rather than
// silently wrong.
func TestStaleAfterNinetyDays(t *testing.T) {
	tbl := &Table{Schema: SchemaVersion, Snapshots: []Snapshot{{EffectiveFrom: "2026-01-01", Base: true}}}
	if tbl.Stale(mustDay(t, "2026-03-01")) {
		t.Error("59 days old must not be stale")
	}
	if !tbl.Stale(mustDay(t, "2026-06-01")) {
		t.Error("151 days old must be stale")
	}
}

// TestParseRejectsUnknownSchemaAndBadOrder: the loader refuses tables it
// cannot read correctly rather than mis-reading prices.
func TestParseRejectsUnknownSchemaAndBadOrder(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":99,"snapshots":[]}`)); err == nil {
		t.Error("accepted an unknown schema version")
	}
	if _, err := Parse([]byte(`{"schema":1,"snapshots":[{"effective_from":"2026-01-01","rows":[]}]}`)); err == nil {
		t.Error("accepted a table whose first snapshot is not the base")
	}
	outOfOrder := `{"schema":1,"snapshots":[{"effective_from":"2026-02-01","base":true,"rows":[]},{"effective_from":"2026-01-01","rows":[]}]}`
	if _, err := Parse([]byte(outOfOrder)); err == nil {
		t.Error("accepted out-of-order snapshots")
	}
}
