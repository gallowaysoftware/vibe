package fleetapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/prices"
)

// C7b — the savings screen. The gates these tests pin are the ones that
// decide whether the number is honest: the equivalence choice (72x), the
// cache tier (5x), dated prices, and every state where the screen is
// supposed to say "I don't know" instead of "$0.00".

// savingsFixture is the price table these tests price against: a chat
// twin at three hosts, an embedding twin, and a frontier model that
// costs two orders of magnitude more. Frozen here, never read from the
// vendored artifact — a table refresh must not move these numbers.
func savingsFixture() *prices.Table {
	return &prices.Table{
		Schema: prices.SchemaVersion,
		Snapshots: []prices.Snapshot{{
			EffectiveFrom: "2026-01-01",
			Base:          true,
			Rows: []prices.Row{
				{Provider: "host-a", Model: "acme/chat-30b", Key: "chat30b", In: 0.10, Out: 0.40, CacheRead: 0.01, Open: true},
				{Provider: "host-b", Model: "acme/chat-30b", Key: "chat30b", In: 0.20, Out: 0.80, CacheRead: 0.02, Open: true},
				{Provider: "host-c", Model: "acme/chat-30b", Key: "chat30b", In: 0.30, Out: 1.20, CacheRead: 0.03, Open: true},
				{Provider: "host-a", Model: "acme/embed-1", Key: "embed1", In: 0.02, Open: true, Mode: prices.ModeEmbedding},
				{Provider: "frontier-inc", Model: "frontier-1", Key: "frontier1", In: 15, Out: 75, CacheRead: 1.5},
				{Provider: "frontier-inc", Model: "cloud-chat", Key: "cloudchat", In: 3, Out: 15, CacheRead: 0.3},
			},
		}},
	}
}

func savingsHosts() *fleetcfg.File {
	return &fleetcfg.File{
		Cells: map[string]fleetcfg.Cell{
			fleetcfg.FrontCell: {URL: "http://front:9000", Class: fleetcfg.ClassAlwaysOn},
			"gpu-cell": {
				URL: "http://gpu:9000", Class: fleetcfg.ClassOpportunistic,
				Power:       &fleetcfg.Power{Source: fleetcfg.PowerDeclared, WattsIdle: 100, WattsBusy: 400},
				CapitalCost: 3100,
				CapitalNote: "example: dual-use GPU, upgrade delta over a gaming-adequate card",
			},
			"laptop": {URL: "http://laptop:9000", Class: fleetcfg.ClassRoaming},
		},
		Pricing: &fleetcfg.Pricing{
			ElectricityPricePerKWh: 0.15,
			Models: map[string]fleetcfg.ModelPricing{
				"qwen-coder": {Twin: "acme/chat-30b"},
				"bge-embed":  {Twin: "acme/embed-1"},
			},
		},
	}
}

// newSavingsServer builds a fleetd-shaped server with an empty ledger.
func newSavingsServer(t *testing.T, hosts *fleetcfg.File) *Server {
	t.Helper()
	dir := t.TempDir()
	cells := []Cell{}
	for name, c := range hosts.Cells {
		cells = append(cells, Cell{Name: name, URL: c.URL, Class: string(c.Class)})
	}
	s := New(cells, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{
		UsagePath: filepath.Join(dir, "usage.jsonl"),
		Timezone:  time.UTC,
		Hosts:     hosts,
		Prices:    savingsFixture(),
	})
	t.Cleanup(s.Close)
	return s
}

// seedTokens writes one token bucket straight into the ledger. The fold
// path is C7a's and is tested there; here the ledger is the INPUT.
func seedTokens(s *Server, day, cell, model, basis string, c counts) {
	b := s.usage.bucketLocked(usageKey{Day: day, Cell: cell, Model: model, Basis: basis}, "UTC")
	b.Req += c.Req
	b.InFresh += c.InFresh
	b.InCached += c.InCached
	b.Out += c.Out
	b.UnmeasuredReq += c.Unmeasured
	b.ErrReq += c.ErrReq
	b.BusyMS += c.BusyMS
}

func seedResidency(s *Server, day, cell, model string, secs int64) {
	b := s.usage.bucketLocked(usageKey{Day: day, Cell: cell, Model: model, Basis: usageBasisResident}, "UTC")
	b.ResidentS += secs
}

func mustSavings(t *testing.T, s *Server, window string) SavingsReport {
	t.Helper()
	rep, err := s.Savings(context.Background(), window)
	if err != nil {
		t.Fatalf("Savings(%q): %v", window, err)
	}
	return rep
}

func cellRow(t *testing.T, rep SavingsReport, name string) CellSavings {
	t.Helper()
	for _, c := range rep.Cells {
		if c.Cell == name {
			return c
		}
	}
	t.Fatalf("cell %q not in the report (%d rows)", name, len(rep.Cells))
	return CellSavings{}
}

func money2(t *testing.T, v *float64) string {
	t.Helper()
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

func today() string { return time.Now().In(time.UTC).Format("2006-01-02") }

// TestSavings_TwinPricedHeadlineToTheCent walks the whole engine: twin
// median across three hosts, the cache split, declared-wattage
// electricity, and the net.
func TestSavings_TwinPricedHeadlineToTheCent(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	// 1M fresh, 12M cached, 0.5M out. At the MEDIAN host (host-b):
	//   1.0 * 0.20 + 12 * 0.02 + 0.5 * 0.80 = 0.20 + 0.24 + 0.40 = $0.84
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{
		Req: 100, InFresh: 1_000_000, InCached: 12_000_000, Out: 500_000, BusyMS: 3_600_000,
	})
	// 20h resident, 1h busy:
	//   100W * 72000s + 300W * 3600s = 2000Wh + 300Wh = 2.3kWh * $0.15 = $0.345
	seedResidency(s, day, "gpu-cell", "", 72_000)

	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "gpu-cell")
	if !row.Measured {
		t.Fatalf("gpu-cell unmeasured: %s", row.Reason)
	}
	if got := money2(t, row.Gross); got != "0.84" {
		t.Errorf("gross = $%s, want $0.84 (median of three hosts)", got)
	}
	if got := money2(t, row.GrossLow); got != "0.42" {
		t.Errorf("gross low = $%s, want $0.42 (cheapest host)", got)
	}
	if got := money2(t, row.GrossHigh); got != "1.26" {
		t.Errorf("gross high = $%s, want $1.26 (dearest host)", got)
	}
	if got := money2(t, row.Power); got != "0.34" {
		t.Errorf("power = $%s, want $0.34 (idle and busy billed separately)", got)
	}
	if got := money2(t, row.Net); got != "0.49" {
		t.Errorf("net = $%s, want $0.49", got)
	}
	if row.NetLabel != "net" {
		t.Errorf("net label = %q, want %q", row.NetLabel, "net")
	}
	if !row.PowerDeclared {
		t.Error("declared wattage must be marked declared so the page can render it with a ~")
	}
	if rep.Totals.CachedPct == nil || fmt.Sprintf("%.0f", *rep.Totals.CachedPct) != "92" {
		t.Errorf("cached pct = %v, want 92", rep.Totals.CachedPct)
	}
	if rep.Totals.TokensPricedPct != 100 {
		t.Errorf("tokens priced = %.1f%%, want 100", rep.Totals.TokensPricedPct)
	}
	if len(row.Models) != 1 || row.Models[0].Hosts != 3 {
		t.Fatalf("model row = %+v, want one row quoting 3 hosts", row.Models)
	}
	if m := row.Models[0]; m.RateLow != 0.10 || m.RateHigh != 0.30 {
		t.Errorf("rate range = %g-%g /MTok, want 0.10-0.30", m.RateLow, m.RateHigh)
	}
	if rep.Caveat == "" || !strings.Contains(rep.Caveat, "upper bound") {
		t.Error("the caveat must travel with the number, in the payload")
	}
}

// TestSavings_FrontierComparableIsOrdersOfMagnitudeAboveTheTwin pins gate 3: the same window
// priced against a frontier model differs from the twin by an order of
// magnitude, which is exactly why the twin is the default and the
// frontier row is a config-declared claim with a written rationale.
func TestSavings_FrontierComparableIsOrdersOfMagnitudeAboveTheTwin(t *testing.T) {
	hosts := savingsHosts()
	twinOnly := mustSavingsFor(t, hosts)

	hosts.Pricing.Frontier = &fleetcfg.Frontier{
		Model:     "frontier-1",
		Rationale: "example: the agentic refactors I would actually have paid for",
	}
	withFrontier := mustSavingsFor(t, hosts)

	if twinOnly.Frontier != nil {
		t.Error("a frontier line rendered with no frontier configured")
	}
	fr := withFrontier.Frontier
	if fr == nil || fr.Cost == nil {
		t.Fatalf("frontier line missing: %+v", fr)
	}
	if fr.Rationale == "" {
		t.Error("the frontier line must carry its rationale to the screen")
	}
	ratio := *fr.Cost / *withFrontier.Totals.Gross
	if ratio < 20 {
		t.Errorf("frontier/twin ratio = %.1fx, want the documented order of magnitude (>=20x)", ratio)
	}
	// And the headline itself is the TWIN price, not the frontier one.
	if money2(t, withFrontier.Totals.Gross) != money2(t, twinOnly.Totals.Gross) {
		t.Error("declaring a frontier comparable changed the headline; the headline is always the twin")
	}
}

func mustSavingsFor(t *testing.T, hosts *fleetcfg.File) SavingsReport {
	t.Helper()
	s := newSavingsServer(t, hosts)
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{
		Req: 100, InFresh: 1_000_000, InCached: 12_000_000, Out: 500_000,
	})
	return mustSavings(t, s, "30d")
}

// TestNoDefaultFrontierMappingInConfigOrShippedExamples pins gate 3's third clause: the repo
// ships no frontier mapping of its own. A shipped mapping would be an
// unearned claim BY THE REPO; a config field with a written rationale is
// the owner's claim, which is a claim someone can argue with.
func TestNoDefaultFrontierMappingInConfigOrShippedExamples(t *testing.T) {
	// Nothing in the zero config names a frontier model.
	if (&fleetcfg.Pricing{}).Frontier != nil {
		t.Error("fleetcfg.Pricing has a default frontier")
	}
	var empty fleetcfg.File
	if empty.Pricing != nil {
		t.Error("an empty hosts.yaml carries pricing config")
	}
	// And no shipped example config sets one outside a comment.
	root := repoRoot(t)
	for _, rel := range []string{
		"deploy/fleetd/README.md",
		"README.md",
		"AGENTS.md",
	} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			if regexp.MustCompile(`^frontier:\s*\S`).MatchString(trimmed) {
				t.Errorf("%s:%d ships a frontier mapping: %q", rel, i+1, trimmed)
			}
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root not found")
	return ""
}

// TestSavings_RepricesFromTheSameLedger pins gate 5: bumping the
// vendored snapshot re-prices the whole history from the same raw
// counts, with no re-collection anywhere.
func TestSavings_RepricesFromTheSameLedger(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{
		Req: 10, InFresh: 1_000_000, InCached: 0, Out: 0,
	})
	before := mustSavings(t, s, "all")
	if got := money2(t, before.Totals.Gross); got != "0.20" {
		t.Fatalf("gross = $%s, want $0.20", got)
	}
	ledgerBefore := len(s.UsageReport("").Buckets)

	// A new snapshot halves every host's input rate, effective today.
	cut := savingsFixture()
	cutRows := []prices.Row{}
	for _, r := range cut.Snapshots[0].Rows {
		r.In /= 2
		r.Out /= 2
		r.CacheRead /= 2
		cutRows = append(cutRows, r)
	}
	cut.Snapshots = append(cut.Snapshots, prices.Snapshot{EffectiveFrom: today(), Rows: cutRows})
	s.prices = cut

	after := mustSavings(t, s, "all")
	if got := money2(t, after.Totals.Gross); got != "0.10" {
		t.Errorf("re-priced gross = $%s, want $0.10", got)
	}
	if got := len(s.UsageReport("").Buckets); got != ledgerBefore {
		t.Errorf("ledger buckets changed on re-price: %d → %d; re-pricing must not touch the counts", ledgerBefore, got)
	}
}

// TestSavings_DatedPricesDoNotRewriteHistory is gate 4 through the whole
// report: yesterday keeps yesterday's rate even after a price cut.
func TestSavings_DatedPricesDoNotRewriteHistory(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	now := time.Now().UTC()
	yesterday := dayKey(now.AddDate(0, 0, -1), time.UTC)
	seedTokens(s, yesterday, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})

	tbl := savingsFixture()
	cheap := []prices.Row{}
	for _, r := range tbl.Snapshots[0].Rows {
		r.In /= 10
		r.Out /= 10
		cheap = append(cheap, r)
	}
	tbl.Snapshots = append(tbl.Snapshots, prices.Snapshot{EffectiveFrom: today(), Rows: cheap})
	s.prices = tbl

	rep := mustSavings(t, s, "all")
	// yesterday at 0.20/MTok + today at 0.02/MTok = 0.22
	if got := money2(t, rep.Totals.Gross); got != "0.22" {
		t.Errorf("gross = $%s, want $0.22 (each day at the rate in effect that day)", got)
	}
}

// TestSavings_EmptyLedgerRendersTheInstallPanel pins gate 6's
// fresh-install case: NOT a $0.00 hero.
func TestSavings_EmptyLedgerRendersTheInstallPanel(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	rep := mustSavings(t, s, "30d")
	if !rep.Empty {
		t.Fatal("an empty ledger must report empty")
	}
	if rep.Totals.Gross != nil || rep.Totals.Net != nil {
		t.Errorf("empty ledger produced money: gross=%v net=%v", rep.Totals.Gross, rep.Totals.Net)
	}
	if len(rep.Payback) != 0 {
		t.Error("empty ledger produced a payback bar")
	}
}

// TestSavings_UnpricedModelKeepsItsTokens pins gate 6: an unmapped model
// keeps its tokens in the token column and leaves the money column, and
// the header says what fraction was priced.
func TestSavings_UnpricedModelKeepsItsTokens(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000, Out: 0})
	seedTokens(s, day, "gpu-cell", "mystery-model", prices.BasisChat, counts{Req: 1, InFresh: 3_000_000, Out: 0})

	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "gpu-cell")
	if row.InFresh != 4_000_000 {
		t.Errorf("in_fresh = %d, want 4,000,000 (unpriced tokens stay in the token column)", row.InFresh)
	}
	if got := money2(t, row.Gross); got != "0.20" {
		t.Errorf("gross = $%s, want $0.20 (only the priced model)", got)
	}
	var unpriced *ModelSavings
	for i := range row.Models {
		if row.Models[i].Model == "mystery-model" {
			unpriced = &row.Models[i]
		}
	}
	if unpriced == nil {
		t.Fatal("the unpriced model is missing from the table entirely")
	}
	if unpriced.Priced || unpriced.Cost != nil {
		t.Errorf("unpriced model carries money: %+v", unpriced)
	}
	if unpriced.Unpriced == "" {
		t.Error("an unpriced model must say WHY")
	}
	if got := fmt.Sprintf("%.0f", rep.Totals.TokensPricedPct); got != "25" {
		t.Errorf("tokens priced = %s%%, want 25%%", got)
	}
}

// TestSavings_NoWattageMeansEmDashNotZero pins gate 6: a cell with no
// declared power renders POWER as an em dash (nil) and labels its net
// "net (power not counted)".
func TestSavings_NoWattageMeansEmDashNotZero(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "laptop", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000, BusyMS: 60_000})
	seedResidency(s, day, "laptop", "", 3600)

	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "laptop")
	if row.Power != nil {
		t.Errorf("power = %v, want nil (no declared wattage is not zero watts)", *row.Power)
	}
	if row.NetLabel != "net (power not counted)" {
		t.Errorf("net label = %q, want %q", row.NetLabel, "net (power not counted)")
	}
	if row.Net == nil || money2(t, row.Net) != money2(t, row.Gross) {
		t.Error("with no power counted the net equals the gross")
	}
}

// TestSavings_NoCapitalCostNoPaybackBar pins gate 6: not 0%, not
// infinity, not an invented denominator — no bar.
func TestSavings_NoCapitalCostNoPaybackBar(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "laptop", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})

	rep := mustSavings(t, s, "all")
	for _, p := range rep.Payback {
		if p.Cell == "laptop" {
			t.Errorf("laptop has no capital_cost but got a payback bar: %+v", p)
		}
	}
	if len(rep.Payback) != 1 || rep.Payback[0].Cell != "gpu-cell" {
		t.Fatalf("payback = %+v, want exactly the cell with a capital_cost", rep.Payback)
	}
	if rep.Payback[0].CapitalNote == "" {
		t.Error("the capital note is required and renders beside the bar")
	}
}

// TestSavings_PaybackIsAllowedToBeEmbarrassing pins gate 6's clamp: on
// honest arithmetic a real cell reads "3% of $3,100 · >10 years at this
// rate", and the screen must be able to say so.
func TestSavings_PaybackIsAllowedToBeEmbarrassing(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	now := time.Now().UTC()
	// 40 days of very small savings: enough days to project, nowhere near
	// enough money.
	for i := 0; i < 40; i++ {
		day := dayKey(now.AddDate(0, 0, -i), time.UTC)
		seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 5, InFresh: 1_000_000, Out: 100_000})
	}
	rep := mustSavings(t, s, "all")
	if len(rep.Payback) != 1 {
		t.Fatalf("payback rows = %d, want 1", len(rep.Payback))
	}
	p := rep.Payback[0]
	if p.RecoveredPct > 5 {
		t.Fatalf("fixture recovered %.1f%%; this test needs an unflattering one", p.RecoveredPct)
	}
	if p.Projection != ">10 years at this rate" {
		t.Errorf("projection = %q, want %q", p.Projection, ">10 years at this rate")
	}
	if p.BreakEvenDay != "" {
		t.Errorf("break-even day = %q, want none", p.BreakEvenDay)
	}
}

// TestSavings_TooEarlyToProject pins gate 6: under two weeks of covered
// days, the screen refuses to extrapolate.
func TestSavings_TooEarlyToProject(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		seedTokens(s, dayKey(now.AddDate(0, 0, -i), time.UTC), "gpu-cell", "qwen-coder", prices.BasisChat,
			counts{Req: 5, InFresh: 1_000_000})
	}
	rep := mustSavings(t, s, "all")
	if len(rep.Payback) != 1 || rep.Payback[0].Projection != "too early to project" {
		t.Errorf("projection = %+v, want \"too early to project\"", rep.Payback)
	}
}

// TestSavings_BreakEvenIsADateFromTheDailySeries: the scoreboard reports
// the exact day the running total crossed the capital number — which is
// the second argument for daily buckets (a rolled-up total can give the
// percentage but never the date).
func TestSavings_BreakEvenIsADateFromTheDailySeries(t *testing.T) {
	hosts := savingsHosts()
	c := hosts.Cells["gpu-cell"]
	c.CapitalCost = 1
	c.Power = nil
	hosts.Cells["gpu-cell"] = c
	s := newSavingsServer(t, hosts)
	now := time.Now().UTC()
	// $0.20/day; crosses $1 on the fifth day.
	for i := 0; i < 8; i++ {
		seedTokens(s, dayKey(now.AddDate(0, 0, -(7-i)), time.UTC), "gpu-cell", "qwen-coder", prices.BasisChat,
			counts{Req: 1, InFresh: 1_000_000})
	}
	rep := mustSavings(t, s, "all")
	want := dayKey(now.AddDate(0, 0, -3), time.UTC)
	if len(rep.Payback) != 1 || rep.Payback[0].BreakEvenDay != want {
		t.Errorf("break-even = %+v, want %s", rep.Payback, want)
	}
	if rep.Payback[0].Projection != "paid for itself" {
		t.Errorf("projection = %q, want %q", rep.Payback[0].Projection, "paid for itself")
	}
}

// TestSavings_UnmeasuredCellIsExcludedFromTheTotals pins gate 6: a cell
// with no measured requests renders a reason and contributes nothing —
// it is not a zero.
func TestSavings_UnmeasuredCellIsExcludedFromTheTotals(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})

	rep := mustSavings(t, s, "30d")
	laptop := cellRow(t, rep, "laptop")
	if laptop.Measured || laptop.Gross != nil || laptop.Net != nil {
		t.Errorf("unmeasured cell carries figures: %+v", laptop)
	}
	if laptop.Reason == "" {
		t.Error("an unmeasured cell must say why")
	}
	if rep.Totals.CellsMeasured != 1 || rep.Totals.CellsTotal != 2 {
		t.Errorf("cells measured = %d of %d, want 1 of 2 (the front is not a serving cell)",
			rep.Totals.CellsMeasured, rep.Totals.CellsTotal)
	}
}

// TestSavings_PartialMeasurementIsStatedNotEstimated: 200s that reported
// no tokens are counted and named, never estimated from duration.
func TestSavings_PartialMeasurementIsStatedNotEstimated(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{
		Req: 41, InFresh: 1_000_000, Unmeasured: 75, ErrReq: 3,
	})
	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "gpu-cell")
	if !strings.Contains(row.Partial, "41 of 116 requests reported tokens") {
		t.Errorf("partial note = %q, want the measured/total request counts", row.Partial)
	}
	if !strings.Contains(row.Partial, "3 errored requests") {
		t.Errorf("partial note = %q, want the error count", row.Partial)
	}
}

// TestSavings_EnergyBillsIdleAndBusySeparately pins §4: a cell holding a
// warm model all day is mostly reporting its idle constant, and C4's
// warm targets deliberately increase exactly that.
func TestSavings_EnergyBillsIdleAndBusySeparately(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000, BusyMS: 0})
	seedResidency(s, day, "gpu-cell", "", 86_400)
	idleOnly := mustSavings(t, s, "30d")
	// 100W * 86400s = 2.4kWh * 0.15 = $0.36
	if got := money2(t, cellRow(t, idleOnly, "gpu-cell").Power); got != "0.36" {
		t.Errorf("idle-only power = $%s, want $0.36", got)
	}

	s2 := newSavingsServer(t, savingsHosts())
	seedTokens(s2, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000, BusyMS: 86_400_000})
	seedResidency(s2, day, "gpu-cell", "", 86_400)
	busy := mustSavings(t, s2, "30d")
	// (100W + 300W delta) * 86400s = 9.6kWh * 0.15 = $1.44
	if got := money2(t, cellRow(t, busy, "gpu-cell").Power); got != "1.44" {
		t.Errorf("fully-busy power = $%s, want $1.44", got)
	}
}

// TestSavings_BusyTimeCannotExceedResidency: concurrent requests each
// report their own wall time, so the sum can exceed the day. A cell
// cannot be busy longer than it was on.
func TestSavings_BusyTimeCannotExceedResidency(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 8, InFresh: 1_000_000, BusyMS: 8 * 86_400_000})
	seedResidency(s, day, "gpu-cell", "", 86_400)
	rep := mustSavings(t, s, "30d")
	if got := money2(t, cellRow(t, rep, "gpu-cell").Power); got != "1.44" {
		t.Errorf("power with 8x-oversubscribed busy time = $%s, want the capped $1.44", got)
	}
}

// TestSavings_CloudSpendIsBesideTheSavingsNotInside pins §6: actual
// cloud spend is a bill reconstruction and never enters the savings sum.
func TestSavings_CloudSpendIsBesideTheSavingsNotInside(t *testing.T) {
	hosts := savingsHosts()
	hosts.Pricing.Models["cloud-chat"] = fleetcfg.ModelPricing{PricedAs: "cloud-chat"}
	s := newSavingsServer(t, hosts)
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedTokens(s, day, fleetcfg.FrontCell, "cloud-chat", usageBasisCloud, counts{
		Req: 20, InFresh: 2_000_000, InCached: 1_000_000, Out: 100_000,
	})

	rep := mustSavings(t, s, "30d")
	if !rep.Cloud.Measured || rep.Cloud.Cost == nil {
		t.Fatalf("cloud spend not measured: %+v", rep.Cloud)
	}
	// 2M * 3 + 1M * 0.3 + 0.1M * 15 = 6.00 + 0.30 + 1.50 = $7.80
	if got := money2(t, rep.Cloud.Cost); got != "7.80" {
		t.Errorf("cloud spend = $%s, want $7.80", got)
	}
	if got := money2(t, rep.Totals.Gross); got != "0.20" {
		t.Errorf("savings total = $%s, want $0.20 — cloud spend must not enter it", got)
	}
	for _, c := range rep.Cells {
		if c.Cell == fleetcfg.FrontCell {
			t.Error("the front must not appear as a serving cell row")
		}
	}
}

// TestFoldCloudUsage_IsCumulativeAndFrontKeyed: the same delta discipline
// C7a's announce fold uses — a duplicate poll adds nothing.
func TestFoldCloudUsage_IsCumulativeAndFrontKeyed(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	at := time.Now().UTC()
	u := &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{
		{Model: "cloud-chat", Basis: "chat", Req: 5, InFresh: 1000, Out: 100},
		{Model: "cloud-chat", Basis: "other", Req: 1, InFresh: 10},
	}}
	s.FoldCloudUsage(u, at)
	s.FoldCloudUsage(u, at) // duplicate poll

	day := dayKey(at, time.UTC)
	b := bucketFor(s.usage, usageKey{Day: day, Cell: fleetcfg.FrontCell, Model: "cloud-chat", Basis: usageBasisCloud, Epoch: "e1"})
	if b.Req != 6 || b.InFresh != 1010 || b.Out != 100 {
		t.Errorf("folded bucket = %+v, want the merged totals counted exactly once", b)
	}

	u2 := &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{
		{Model: "cloud-chat", Basis: "chat", Req: 9, InFresh: 2000, Out: 100},
		{Model: "cloud-chat", Basis: "other", Req: 1, InFresh: 10},
	}}
	s.FoldCloudUsage(u2, at)
	b = bucketFor(s.usage, usageKey{Day: day, Cell: fleetcfg.FrontCell, Model: "cloud-chat", Basis: usageBasisCloud, Epoch: "e1"})
	if b.Req != 10 || b.InFresh != 2010 {
		t.Errorf("second fold = %+v, want the delta against the cumulative cursor", b)
	}
}

// TestCloudBasisIsClosedToCells: a cell announcing "cloud" rows would
// key onto exactly the bucket fleetd writes itself.
func TestCloudBasisIsClosedToCells(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	at := time.Now().UTC()
	s.usage.fold("gpu-cell", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{
		{Model: "cloud-chat", Basis: usageBasisCloud, Req: 99, InFresh: 9_000_000},
		{Model: "qwen-coder", Basis: "chat", Req: 1, InFresh: 10},
	}}, at)
	day := dayKey(at, time.UTC)
	if b := bucketFor(s.usage, usageKey{Day: day, Cell: "gpu-cell", Model: "cloud-chat", Basis: usageBasisCloud, Epoch: "e1"}); b.Req != 0 {
		t.Errorf("a cell-announced cloud row landed in the ledger: %+v", b)
	}
	if b := bucketFor(s.usage, usageKey{Day: day, Cell: "gpu-cell", Model: "qwen-coder", Basis: "chat", Epoch: "e1"}); b.Req != 1 {
		t.Error("one rejected entry cost the announce its other rows")
	}
}

// TestCellLevelResidencyIsTheEnergyDenominator: per-model residency rows
// cannot serve as one (summing bills a multi-resident box twice), so
// foldResidency writes a cell-level row.
func TestCellLevelResidencyIsTheEnergyDenominator(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	at := time.Now().UTC()
	s.usage.foldResidency("gpu-cell", []AnnounceModel{{ID: "a", State: "ready"}, {ID: "b", State: "ready"}}, 15, at.Add(-30*time.Second))
	s.usage.foldResidency("gpu-cell", []AnnounceModel{{ID: "a", State: "ready"}, {ID: "b", State: "ready"}}, 15, at)

	day := dayKey(at, time.UTC)
	cellRow := bucketFor(s.usage, usageKey{Day: day, Cell: "gpu-cell", Basis: usageBasisResident})
	perModel := bucketFor(s.usage, usageKey{Day: day, Cell: "gpu-cell", Model: "a", Basis: usageBasisResident})
	if cellRow.ResidentS != perModel.ResidentS {
		t.Errorf("cell-level residency %ds != per-model %ds; two resident models must not double the box's wall clock",
			cellRow.ResidentS, perModel.ResidentS)
	}
	if cellRow.ResidentS == 0 {
		t.Fatal("no cell-level residency row written")
	}
}

// TestSavings_WindowValidation: a malformed window is a 400, not a
// silently empty document.
func TestSavings_WindowValidation(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	for _, w := range []string{"7", "sevendays", "-3d", "0d", "12h"} {
		if _, err := s.Savings(context.Background(), w); err == nil {
			t.Errorf("window %q accepted", w)
		}
	}
	for _, w := range []string{"", "7d", "30d", "all"} {
		if _, err := s.Savings(context.Background(), w); err != nil {
			t.Errorf("window %q rejected: %v", w, err)
		}
	}
}

// TestSavings_WindowExcludesOlderDaysButPaybackIsLifetime.
func TestSavings_WindowExcludesOlderDaysButPaybackIsLifetime(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	now := time.Now().UTC()
	seedTokens(s, dayKey(now.AddDate(0, 0, -20), time.UTC), "gpu-cell", "qwen-coder", prices.BasisChat,
		counts{Req: 1, InFresh: 1_000_000})
	seedTokens(s, dayKey(now, time.UTC), "gpu-cell", "qwen-coder", prices.BasisChat,
		counts{Req: 1, InFresh: 1_000_000})

	week := mustSavings(t, s, "7d")
	if got := money2(t, week.Totals.Gross); got != "0.20" {
		t.Errorf("7d gross = $%s, want $0.20 (one day in the window)", got)
	}
	month := mustSavings(t, s, "30d")
	if got := money2(t, month.Totals.Gross); got != "0.40" {
		t.Errorf("30d gross = $%s, want $0.40", got)
	}
	// Payback is lifetime regardless of the window: a scoreboard is a
	// running total against a fixed threshold.
	if week.Payback[0].Recovered != month.Payback[0].Recovered {
		t.Errorf("payback moved with the window: %v vs %v", week.Payback[0].Recovered, month.Payback[0].Recovered)
	}
}

// TestSavings_HTTPEndpointIsReadOnlyJSON.
func TestSavings_HTTPEndpointIsReadOnlyJSON(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/fleet/savings?window=7d")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	var rep SavingsReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Gross == nil {
		t.Error("the served document has no money in it")
	}
	bad, err := http.Get(srv.URL + "/api/fleet/savings?window=lol")
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed window: HTTP %d, want 400", bad.StatusCode)
	}
	// POST is not a method this route has.
	post, err := http.Post(srv.URL+"/api/fleet/savings", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode == http.StatusOK {
		t.Error("POST /api/fleet/savings succeeded; the savings surface is read-only")
	}
}

// ─── gate 7: page invariants ────────────────────────────────────────────────

// TestFleetPage_SavingsIsAViewNotARoute pins gate 7: the page still
// registers exactly ONE route, and the savings screen is a hash-routed
// view inside it. A second server route would force the bearer
// middleware's exact-match exemption to widen to a prefix, which C5 §7
// forbids.
func TestFleetPage_SavingsIsAViewNotARoute(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	mux := http.NewServeMux()
	s.registerFleetPage(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ok, err := http.Get(srv.URL + "/ui/fleet")
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("/ui/fleet: HTTP %d", ok.StatusCode)
	}
	for _, path := range []string{"/ui/fleet/savings", "/ui/savings", "/ui/fleet/"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404 — the page adds exactly one route", path, resp.StatusCode)
		}
	}
}

func fleetPageSource(t *testing.T) string {
	t.Helper()
	data, err := fleetPageFS.ReadFile("fleet.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestFleetPage_SavingsViewInvariants pins the rest of gate 7: hash
// routing, no external assets, no build step, bar widths set on the
// element, and every figure coming from the authed GET.
func TestFleetPage_SavingsViewInvariants(t *testing.T) {
	page := fleetPageSource(t)

	for _, want := range []string{
		`id="view-savings"`,
		`id="view-fleet"`,
		`function showView(`,
		`"hashchange"`,
		`location.hash === "#savings"`,
		`api("/api/fleet/savings`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Bar widths come from el.style.width, never an interpolated
	// style="…" attribute (which would need attr() and is the shape C5's
	// PAGE-1 test forbids).
	if !strings.Contains(page, ".style.width =") {
		t.Error("payback bars must set width via el.style.width")
	}
	if regexp.MustCompile(`style\s*=\s*["'][^"']*\$\{`).MatchString(page) {
		t.Error("an interpolated style=\"…\" attribute appeared on the page")
	}
	// No external asset, CDN or build step.
	for _, forbidden := range []string{"<script src", "<link ", "http://", "https://", "import(", "require("} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page references %q; it must load on a LAN with no internet", forbidden)
		}
	}
	// The savings view has no action buttons — that is what keeps "no new
	// mutation surface" trivially true.
	start := strings.Index(page, `<section id="view-savings"`)
	if start < 0 {
		t.Fatal("savings section not found")
	}
	end := strings.Index(page[start:], "</section>")
	if end < 0 {
		t.Fatal("savings section is not closed")
	}
	section := page[start : start+end]
	if strings.Contains(section, "<button") || strings.Contains(section, "onclick") {
		t.Error("the savings view has action buttons; it is a read-only screen")
	}
	if strings.Contains(page, `rpc("`) && strings.Contains(section, "rpc(") {
		t.Error("the savings view calls an MCP tool; it must not mutate anything")
	}
}

// TestFleetPage_SavingsTimerIsClearedInBoot pins gate 8: re-entering the
// token must not stack a fourth timer.
func TestFleetPage_SavingsTimerIsClearedInBoot(t *testing.T) {
	page := fleetPageSource(t)
	start := strings.Index(page, "async function boot()")
	if start < 0 {
		t.Fatal("boot() not found")
	}
	body := page[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	for _, want := range []string{"streamAbort.abort()", "clearInterval(pollTimer)", "clearInterval(savingsTimer)"} {
		if !strings.Contains(body, want) {
			t.Errorf("boot() does not %s; a token rotation would leave the old one running", want)
		}
	}
	if strings.Index(body, "clearInterval(savingsTimer)") > strings.Index(body, "savingsTimer = setInterval") {
		t.Error("savingsTimer is assigned before it is cleared")
	}
}

// TestSavingsCaveatIsRenderedFromThePayload: the page shows the caveat
// text the server sends, so the two can never drift.
func TestSavingsCaveatIsRenderedFromThePayload(t *testing.T) {
	page := fleetPageSource(t)
	if !strings.Contains(page, `$("sv-caveat-text").textContent = sv.caveat`) {
		t.Error("the page does not render the server's caveat")
	}
	if !strings.Contains(savingsCaveat, "same open-weight model rented from a real host") {
		t.Error("the caveat no longer states the equivalence choice")
	}
	if !strings.Contains(savingsCaveat, "fresh and cache-read") {
		t.Error("the caveat no longer states the cache split")
	}
}

// ─── review pass (ground rule 9) ────────────────────────────────────────────

// TestSavings_MeasuredButUnpricedCloudSaysSo: cloud traffic that was
// measured and could not be priced must not read "not measured". The
// requests happened and the bill exists; what is missing is a rate, and
// saying otherwise is a lie in the flattering direction.
func TestSavings_MeasuredButUnpricedCloudSaysSo(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), fleetcfg.FrontCell, "some-vendor-model", usageBasisCloud, counts{
		Req: 12, InFresh: 500_000, Out: 50_000,
	})
	rep := mustSavings(t, s, "30d")
	if !rep.Cloud.Measured {
		t.Fatal("cloud traffic was recorded but reads as unmeasured")
	}
	if rep.Cloud.Cost != nil {
		t.Errorf("cloud cost = %v, want nil (no rate for that model)", *rep.Cloud.Cost)
	}
	if !strings.Contains(rep.Cloud.Reason, "some-vendor-model") {
		t.Errorf("reason = %q, want the unpriced model named", rep.Cloud.Reason)
	}
	if len(rep.Cloud.Unpriced) != 1 {
		t.Errorf("unpriced = %v, want the one model", rep.Cloud.Unpriced)
	}
	if rep.Cloud.Req != 12 {
		t.Errorf("req = %d, want 12 — unpriced cloud traffic still counts as traffic", rep.Cloud.Req)
	}
}

// TestSavings_ElectricityThatLooksFreeIsFlagged pins §4's inverted rule:
// against an honest same-model-rented comparable, energy lands around
// 11-16% of the figure. A power line that looks like a rounding error is
// evidence the COMPARABLE is wrong.
func TestSavings_ElectricityThatLooksFreeIsFlagged(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	// A huge notional saving against one minute of residency.
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{
		Req: 10, InFresh: 500_000_000, Out: 10_000_000,
	})
	seedResidency(s, day, "gpu-cell", "", 60)

	rep := mustSavings(t, s, "30d")
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "comparable is too expensive") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the inverted electricity rule", rep.Notes)
	}
}

// TestSavings_ContextCancellationIsAnErrorNotAShortReport: a savings
// document missing three days would be indistinguishable from a fleet
// that served nothing those days.
func TestSavings_ContextCancellationIsAnErrorNotAShortReport(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Savings(ctx, "30d"); err == nil {
		t.Error("a cancelled context produced a report instead of an error")
	}
}

// TestFleetPage_DeepLinkPicksTheViewBeforeTheFirstFetch: a #savings deep
// link must land on the savings view even when the first state fetch
// fails, or one unreachable cell sends the reader to the wrong screen.
func TestFleetPage_DeepLinkPicksTheViewBeforeTheFirstFetch(t *testing.T) {
	page := fleetPageSource(t)
	start := strings.Index(page, "async function boot()")
	if start < 0 {
		t.Fatal("boot() not found")
	}
	body := page[start:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	showAt := strings.Index(body, "showView()")
	awaitAt := strings.Index(body, "await refresh()")
	if showAt < 0 || awaitAt < 0 {
		t.Fatalf("boot() no longer both awaits and picks a view: %q", body)
	}
	if showAt > awaitAt {
		t.Error("boot() picks the view only after the first fetch; a failed fetch strands a #savings deep link on the fleet view")
	}
	// And a tab with no token still resolves its view (the gate then asks
	// for the token).
	if !strings.Contains(page, "showView();\nif (token) boot()") {
		t.Error("a bare tab with no token does not resolve its view before the token gate")
	}
}

// TestSavings_MeasuredButUnpricedCellIsNotAZero: a cell that served
// requests none of which could be priced must render money as an em dash
// with a reason — not $0.00 — while its TOKENS stay in every token
// column, which is what makes "N% of tokens priced" mean anything.
func TestSavings_MeasuredButUnpricedCellIsNotAZero(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "mystery-model", prices.BasisChat, counts{
		Req: 12, InFresh: 4_000_000, InCached: 1_000_000, Out: 200_000,
	})
	seedResidency(s, day, "gpu-cell", "", 3600)

	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "gpu-cell")
	if !row.Measured {
		t.Fatal("the cell served measured requests; it is measured")
	}
	if row.Gross != nil || row.Net != nil {
		t.Errorf("unpriced cell reported money: gross=%v net=%v", row.Gross, row.Net)
	}
	if row.Reason == "" || row.NetLabel != "net (nothing priced)" {
		t.Errorf("reason=%q net_label=%q, want both to say nothing was priced", row.Reason, row.NetLabel)
	}
	if row.InFresh != 4_000_000 || row.Req != 12 {
		t.Errorf("tokens left the token column: %+v", row)
	}
	if rep.Totals.Gross != nil {
		t.Errorf("fleet total = %v, want nil — a fleet that priced nothing did not save $0.00", *rep.Totals.Gross)
	}
	if rep.Totals.InFresh != 4_000_000 {
		t.Errorf("fleet in_fresh = %d, want the unpriced tokens counted", rep.Totals.InFresh)
	}
	if rep.Totals.TokensPricedPct != 0 {
		t.Errorf("tokens priced = %.1f%%, want 0", rep.Totals.TokensPricedPct)
	}
	// Power is still known and still declared: what is missing is a price
	// for the work, not a measurement of the box.
	if row.Power == nil {
		t.Error("declared power dropped out because the tokens were unpriced")
	}
}

// ─── second adversarial review pass ─────────────────────────────────────────

// TestSavings_LegacyResidencyFallsBackToTheMaxNotTheSum: ledgers written
// before the cell-level residency row carry per-model rows only. Summing
// them bills a box that held two models resident all day for 48 hours of
// idle watts; the fallback is the MAX across models, which under-counts
// a cell that alternated models — the conservative direction for a term
// subtracted from savings.
func TestSavings_LegacyResidencyFallsBackToTheMaxNotTheSum(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	// No cell-level row: two models, each resident the whole day.
	seedResidency(s, day, "gpu-cell", "model-a", 86_400)
	seedResidency(s, day, "gpu-cell", "model-b", 86_400)

	rep := mustSavings(t, s, "30d")
	// 100W * 86400s = 2.4kWh * $0.15 = $0.36. The sum would be $0.72.
	if got := money2(t, cellRow(t, rep, "gpu-cell").Power); got != "0.36" {
		t.Errorf("legacy-ledger power = $%s, want $0.36 (max across models, never the sum)", got)
	}
}

// TestSavings_CellResidencyRowWinsOverPerModelRows: once the cell-level
// row exists it is authoritative, so a box whose per-model rows happen to
// exceed the day cannot inflate the denominator.
func TestSavings_CellResidencyRowWinsOverPerModelRows(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedResidency(s, day, "gpu-cell", "", 3_600)
	seedResidency(s, day, "gpu-cell", "model-a", 86_400)

	rep := mustSavings(t, s, "30d")
	// 100W * 3600s = 0.1kWh * $0.15 = $0.015 → $0.01 to the cent.
	if got := money2(t, cellRow(t, rep, "gpu-cell").Power); got != "0.01" {
		t.Errorf("power = $%s, want $0.01 (the cell-level row is the denominator)", got)
	}
}

// TestSavings_FrontCapitalIsANoteNotABarItCannotMove: the front is
// structurally excluded from the savings table (C7a folds no token rows
// for it), so a payback bar for it has a numerator defined to be zero and
// would read "0% of $N" forever — a screen claiming to have measured
// hardware it never measured.
func TestSavings_FrontCapitalIsANoteNotABarItCannotMove(t *testing.T) {
	hosts := savingsHosts()
	front := hosts.Cells[fleetcfg.FrontCell]
	front.CapitalCost = 900
	front.CapitalNote = "example: the router box"
	hosts.Cells[fleetcfg.FrontCell] = front

	s := newSavingsServer(t, hosts)
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	rep := mustSavings(t, s, "all")
	for _, p := range rep.Payback {
		if p.Cell == fleetcfg.FrontCell {
			t.Errorf("the front got a payback bar it can never move: %+v", p)
		}
	}
	if len(rep.Payback) != 1 || rep.Payback[0].Cell != "gpu-cell" {
		t.Fatalf("payback = %+v, want only the serving cell", rep.Payback)
	}
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "front") && strings.Contains(n, "capital_cost") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the front's capital_cost accounted for in words", rep.Notes)
	}
}

// TestSavings_PartialPowerCoverageIsStated: subtracting SOME cells'
// electricity from a fleet-wide gross is the one place this screen errs
// toward a larger number. It must not be silent about it.
func TestSavings_PartialPowerCoverageIsStated(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedResidency(s, day, "gpu-cell", "", 3600)
	seedTokens(s, day, "laptop", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})

	rep := mustSavings(t, s, "30d")
	if rep.Totals.Power == nil {
		t.Fatal("the fleet total counted no power at all; this test needs a partial one")
	}
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "only some cells") && strings.Contains(n, "laptop") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the uncounted cell named", rep.Notes)
	}
}

// TestSavings_MissingElectricityPriceBlamesTheRightField: a fleet that
// declared wattage on every cell and no electricity price gets an em dash
// everywhere. Blaming a missing `power:` block sends the reader to fix
// the wrong line.
func TestSavings_MissingElectricityPriceBlamesTheRightField(t *testing.T) {
	hosts := savingsHosts()
	hosts.Pricing.ElectricityPricePerKWh = 0
	s := newSavingsServer(t, hosts)
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedResidency(s, day, "gpu-cell", "", 3600)

	rep := mustSavings(t, s, "30d")
	if rep.Totals.Power != nil {
		t.Fatalf("power = %v with no electricity price", *rep.Totals.Power)
	}
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "electricity_price_per_kwh") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %v, want the missing electricity price named", rep.Notes)
	}
}

// TestSavings_CloudTokensStayOutOfTheTokensPricedPct: cloud rows are a
// bill reconstruction on the FRONT and never enter the savings
// arithmetic — including the denominator of "N% of tokens priced", which
// is a statement about what the CELLS served.
func TestSavings_CloudTokensStayOutOfTheTokensPricedPct(t *testing.T) {
	hosts := savingsHosts()
	hosts.Pricing.Models["cloud-chat"] = fleetcfg.ModelPricing{PricedAs: "cloud-chat"}
	s := newSavingsServer(t, hosts)
	day := today()
	seedTokens(s, day, "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})
	seedTokens(s, day, fleetcfg.FrontCell, "cloud-chat", usageBasisCloud, counts{
		Req: 20, InFresh: 9_000_000, Out: 1_000_000,
	})
	rep := mustSavings(t, s, "30d")
	if rep.Totals.TokensPricedPct != 100 {
		t.Errorf("tokens priced = %.1f%%, want 100 — cloud tokens are not cell tokens", rep.Totals.TokensPricedPct)
	}
	if rep.Totals.InFresh != 1_000_000 {
		t.Errorf("fleet in_fresh = %d, want only the cell's tokens", rep.Totals.InFresh)
	}
}

// TestSavings_CancelledRequestIsNotABadRequest: Savings aborts on
// ctx.Err() rather than returning a short document, so the handler must
// not report the client's own disconnect as the client's bad input.
func TestSavings_CancelledRequestIsNotABadRequest(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	seedTokens(s, today(), "gpu-cell", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/fleet/savings?window=30d", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleSavings(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Error("a cancelled request reported as HTTP 400; a typo'd window and a dropped connection must differ")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("HTTP %d, want 503", rec.Code)
	}

	// And a genuinely malformed window is still the caller's fault.
	rec = httptest.NewRecorder()
	s.handleSavings(rec, httptest.NewRequest(http.MethodGet, "/api/fleet/savings?window=lol", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed window: HTTP %d, want 400", rec.Code)
	}
}

// TestFleetPage_MeasuredCellReasonIsRendered: the payload's reason for a
// measured-but-unpriced cell ("nothing priced in this window") is the
// only thing standing between the reader and an unexplained em dash.
func TestFleetPage_MeasuredCellReasonIsRendered(t *testing.T) {
	page := fleetPageSource(t)
	start := strings.Index(page, "function renderSavings(")
	if start < 0 {
		t.Fatal("renderSavings() not found")
	}
	body := page[start:]
	if end := strings.Index(body, "\nfunction showView("); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "c.reason, c.partial") {
		t.Error("renderSavings() does not render a measured cell's reason; its money column is an unexplained dash")
	}
}

// TestSavings_IdleCellStillPaysForItsElectricity: a cell holding a C4
// warm target resident all day and serving nothing measurable burns its
// idle constant. §8 excludes an unmeasured cell from the SAVINGS total;
// electricity is a cost, not a saving, and dropping it inflates the
// fleet net. The payback strip already charged those watts — the window
// table used to disagree with it.
func TestSavings_IdleCellStillPaysForItsElectricity(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	day := today()
	// gpu-cell: resident all day, not one measured request.
	seedResidency(s, day, "gpu-cell", "", 86_400)
	// laptop: real traffic, so the fleet has a gross to compare against.
	seedTokens(s, day, "laptop", "qwen-coder", prices.BasisChat, counts{Req: 1, InFresh: 1_000_000})

	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "gpu-cell")
	if row.Measured {
		t.Fatal("a cell with no measured requests must not read as measured")
	}
	// 100W * 86400s = 2.4kWh * $0.15 = $0.36
	if got := money2(t, row.Power); got != "0.36" {
		t.Errorf("idle cell power = $%s, want $0.36", got)
	}
	if got := money2(t, row.Net); got != "-0.36" {
		t.Errorf("idle cell net = $%s, want -$0.36 — it cost money and saved nothing", got)
	}
	if rep.Totals.Power == nil || money2(t, rep.Totals.Power) != "0.36" {
		t.Errorf("fleet power = %s, want $0.36 — an idle cell's watts are still the fleet's watts",
			money2(t, rep.Totals.Power))
	}
	if rep.Totals.Net == nil || money2(t, rep.Totals.Net) != "-0.16" {
		t.Errorf("fleet net = %s, want -$0.16 ($0.20 gross − $0.36 power)", money2(t, rep.Totals.Net))
	}
	if rep.Totals.CellsMeasured != 1 {
		t.Errorf("cells measured = %d, want 1 — counting the watts does not make the cell measured", rep.Totals.CellsMeasured)
	}
	// And the page has somewhere to put it.
	page := fleetPageSource(t)
	if !strings.Contains(page, "td.colSpan = 4;") {
		t.Error("the page's unmeasured row does not leave room for the power and net columns")
	}
}

// TestSavings_WarmLoopPokesAreNamed: C7a calls poke_req a VISIBLE
// counter because C4's warm loops issue real metered 1-token completions
// that can outnumber human requests. They are excluded from every token
// sum, so a screen that never mentions them makes the exclusion
// invisible.
func TestSavings_WarmLoopPokesAreNamed(t *testing.T) {
	s := newSavingsServer(t, savingsHosts())
	b := s.usage.bucketLocked(usageKey{Day: today(), Cell: "gpu-cell", Model: "qwen-coder", Basis: prices.BasisChat}, "UTC")
	b.Req = 4
	b.InFresh = 1_000_000
	b.PokeReq = 96

	rep := mustSavings(t, s, "30d")
	row := cellRow(t, rep, "gpu-cell")
	if !strings.Contains(row.Partial, "96 warm-loop pokes") {
		t.Errorf("partial note = %q, want the poke count named", row.Partial)
	}
	if row.Req != 4 {
		t.Errorf("req = %d, want 4 — pokes are not requests", row.Req)
	}
}
