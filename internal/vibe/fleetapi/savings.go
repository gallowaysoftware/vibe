package fleetapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/prices"
)

// The savings screen's arithmetic (fleet-control C7b). C7a counts
// tokens; this prices them and answers "did the hardware sort of pay for
// itself?".
//
// Two choices dominate every other decision in this file, and both are
// load-bearing in the direction of a SMALLER number:
//
//   - Equivalence is the same open-weight model RENTED, priced at the
//     median across real hosts and rendered as a range. Pricing a 27B
//     local model as a frontier model moves the answer about 72x, and a
//     screen that says "paid for itself in 85 days" when the honest
//     answer is "16.7 years" is worse than no screen. The frontier
//     comparable exists only as a config-declared, rationale-carrying
//     second line.
//   - Prompt tokens are priced in two buckets, fresh and cache-read,
//     ALWAYS, from C7a's measurement. Real local corpora run ~92% cached
//     input; pricing every prompt token at the full input rate overstates
//     by ~5x. The two errors compound to ~350x.
//
// Everything else here follows from one rule: a number this screen does
// not know is an em dash with a reason, never a zero.

// SavingsWindow values accepted by the endpoint and the MCP tool.
const (
	WindowAll = "all"
)

// defaultSavingsWindow is what an unqualified request gets.
const defaultSavingsWindow = "30d"

// projectionMinDays is how many covered days a trailing-rate projection
// needs before it means anything.
const projectionMinDays = 14

// projectionWindowDays is the trailing window the payback projection
// extrapolates from.
const projectionWindowDays = 28

// projectionClampYears is where a projection stops being a date and
// becomes a verdict.
const projectionClampYears = 10

// SavingsReport is the whole /api/fleet/savings document.
type SavingsReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	Window      string    `json:"window"`
	SinceDay    string    `json:"since_day,omitempty"`
	TZ          string    `json:"tz"`

	Prices PriceTableInfo `json:"prices"`

	// Currency is the unit of every money field below, and the reason the
	// two halves of this screen can or cannot be netted against each
	// other. It is a first-class part of the payload rather than a page
	// affordance because the MCP tool serves this same document to
	// agents, and "$12.40 net" quoted out of a report whose gross is USD
	// and whose electricity is CAD is a wrong number no reader can
	// detect.
	Currency CurrencyInfo `json:"currency"`

	// Empty is the fresh-install state: no ledger at all. The page shows
	// the install panel, NOT a $0.00 hero — a zero hero is a claim about
	// the fleet, and there is nothing to claim yet.
	Empty bool `json:"empty"`

	Cells   []CellSavings `json:"cells"`
	Totals  SavingsTotals `json:"totals"`
	Payback []Payback     `json:"payback"`
	// Frontier is the optional "if I'd used <frontier model> instead"
	// line. It renders only with a written rationale.
	Frontier *FrontierLine `json:"frontier,omitempty"`
	// Cloud is actual cloud spend in the same window — a bill
	// reconstruction, not a counterfactual. It is the induced-demand
	// control: if both rose together, the fleet added work rather than
	// replacing it.
	Cloud CloudSpend `json:"cloud"`

	Caveat string   `json:"caveat"`
	Notes  []string `json:"notes,omitempty"`
}

// PriceTableInfo is the provenance strip: what priced this, from when,
// and whether it is real.
type PriceTableInfo struct {
	AsOf      string          `json:"as_of"`
	Oldest    string          `json:"oldest"`
	Example   bool            `json:"example_data"`
	Stale     bool            `json:"stale"`
	Snapshots int             `json:"snapshots"`
	Sources   []prices.Source `json:"sources,omitempty"`
}

// CurrencyInfo says what each half of the screen is denominated in.
//
// Two halves, two sources. TOKENS are priced out of the vendored table,
// which is normalized to prices.Currency and is not the operator's
// business. LOCAL — electricity and capital — is the operator's own
// money, declared in `pricing.currency`. They are the same number's
// worth of money only when the two codes agree.
type CurrencyInfo struct {
	// Tokens is the price table's currency: every gross, frontier and
	// cloud figure in this report.
	Tokens string `json:"tokens"`
	// Local is the currency of every power and capital figure.
	Local string `json:"local"`
	// LocalDeclared is false when hosts.yaml said nothing and Local is
	// therefore an ASSUMPTION this report is making out loud rather than
	// a fact it was told. The page renders a chip for it, next to the
	// EXAMPLE DATA one, for the same reason.
	LocalDeclared bool `json:"local_declared"`
	// Mixed is true when the two differ. Every subtraction and every
	// ratio that would cross the two is then refused: this repo ships no
	// exchange rate, a rate invented at render time is an unsourced
	// number in a screen whose entire premise is that it does not invent
	// numbers, and a rate is dated anyway (the ledger spans months).
	Mixed bool `json:"mixed"`
	// Note is the one-sentence explanation the page and the agents both
	// render. Never empty when Mixed or when Local is undeclared.
	Note string `json:"note,omitempty"`
}

// canNet reports whether a token-priced figure and a locally-priced one
// may be combined into one number. It is the single gate every
// subtraction and every payback ratio in this file goes through.
func (c CurrencyInfo) canNet() bool { return !c.Mixed }

// currencyOf resolves the two currencies from config. Undeclared local
// is assumed to be the table's — that is what the arithmetic did before
// this field existed, so it is the only choice that does not silently
// change every existing fleet's numbers — but the assumption is
// RECORDED, and the page and the agents both render it.
func currencyOf(hosts *fleetcfg.File) CurrencyInfo {
	info := CurrencyInfo{Tokens: prices.Currency, Local: prices.Currency}
	var declared string
	if hosts != nil {
		declared = hosts.Pricing.CurrencyOrEmpty()
	}
	if declared == "" {
		info.Note = "electricity and capital have no declared currency (pricing.currency), so they are assumed to be " +
			prices.Currency + " like the price table; if your hydro bill and your hardware receipts are in another currency, " +
			"the net and the payback bars below are wrong by the exchange rate"
		return info
	}
	info.Local = declared
	info.LocalDeclared = true
	if declared == prices.Currency {
		return info
	}
	info.Mixed = true
	info.Note = "token savings are priced in " + prices.Currency + " (the vendored price table) and electricity and capital are declared in " +
		declared + ". This report will not subtract one from the other or divide one by the other: no exchange rate ships with vibe, " +
		"a rate invented here would be an unsourced number, and the ledger spans months over which any rate moved. " +
		"Both figures are shown, each labelled with its own currency; declare pricing.currency: " + prices.Currency +
		" (converting the values yourself) to get a net and a payback bar back"
	return info
}

// SavingsTotals is the fleet line. Every money field is a pointer:
// absent means unmeasured, and unmeasured is excluded from the sum
// rather than counted as zero (the invariant InFlight already encodes).
type SavingsTotals struct {
	Gross     *float64 `json:"gross,omitempty"`
	GrossLow  *float64 `json:"gross_low,omitempty"`
	GrossHigh *float64 `json:"gross_high,omitempty"`
	Power     *float64 `json:"power,omitempty"`
	Net       *float64 `json:"net,omitempty"`
	// NetLabel says what the net actually is: "net" or
	// "net (power not counted)".
	NetLabel string `json:"net_label"`

	InFresh  int64 `json:"in_fresh"`
	InCached int64 `json:"in_cached"`
	Out      int64 `json:"out"`
	Req      int64 `json:"req"`

	// CachedPct is the screen's biggest error term and its most
	// interesting fact at the same time.
	CachedPct       *float64 `json:"cached_pct,omitempty"`
	TokensPricedPct float64  `json:"tokens_priced_pct"`
	CellsMeasured   int      `json:"cells_measured"`
	CellsTotal      int      `json:"cells_total"`
}

// CellSavings is one row of the table.
type CellSavings struct {
	Cell string `json:"cell"`
	// Measured is false when the cell announced no usage in the window.
	// An unmeasured cell renders em dashes plus a reason chip and is
	// EXCLUDED from the totals.
	Measured bool   `json:"measured"`
	Reason   string `json:"reason,omitempty"`

	Models []ModelSavings `json:"models,omitempty"`

	InFresh  int64 `json:"in_fresh"`
	InCached int64 `json:"in_cached"`
	Out      int64 `json:"out"`
	Req      int64 `json:"req"`

	Gross     *float64 `json:"gross,omitempty"`
	GrossLow  *float64 `json:"gross_low,omitempty"`
	GrossHigh *float64 `json:"gross_high,omitempty"`
	Power     *float64 `json:"power,omitempty"`
	// PowerDeclared marks a power figure that came from declared wattage
	// rather than measurement. The page prefixes it with "~" so a
	// declared cell never looks measured.
	PowerDeclared bool     `json:"power_declared,omitempty"`
	Net           *float64 `json:"net,omitempty"`
	NetLabel      string   `json:"net_label,omitempty"`

	TokensPricedPct float64 `json:"tokens_priced_pct"`
	// Partial is the honest footnote on a cell that served more requests
	// than it could measure (mlx streaming, cancelled streams, rows lost
	// to an in-memory ring).
	Partial string `json:"partial,omitempty"`
}

// ModelSavings is one model's contribution inside a cell.
type ModelSavings struct {
	Model string `json:"model"`
	Basis string `json:"basis"`
	Twin  string `json:"twin,omitempty"`

	InFresh  int64 `json:"in_fresh"`
	InCached int64 `json:"in_cached"`
	Out      int64 `json:"out"`
	Req      int64 `json:"req"`

	// Priced is false when no honest rate exists. The tokens stay in the
	// token column and leave the money column; Unpriced says why.
	Priced   bool   `json:"priced"`
	Unpriced string `json:"unpriced,omitempty"`

	Cost     *float64 `json:"cost,omitempty"`
	CostLow  *float64 `json:"cost_low,omitempty"`
	CostHigh *float64 `json:"cost_high,omitempty"`
	// Hosts is how many real hosts stood behind the median.
	Hosts int `json:"hosts,omitempty"`
	// RateLow/RateHigh bound the per-MTok input rate across those hosts.
	RateLow  float64 `json:"rate_low,omitempty"`
	RateHigh float64 `json:"rate_high,omitempty"`
	// NoCacheDiscount marks a twin whose median host publishes no cache
	// read price, so cached tokens were billed at the full input rate.
	NoCacheDiscount bool `json:"no_cache_discount,omitempty"`
	// Counterfactual is the declared tier (interactive | batch | free)
	// and Multiplier what it scaled the cost by.
	Counterfactual string  `json:"counterfactual,omitempty"`
	Multiplier     float64 `json:"multiplier,omitempty"`
	// SharePct is this model's share of the cell's billable tokens.
	SharePct float64 `json:"share_pct"`
}

// FrontierLine is the optional frontier comparable.
type FrontierLine struct {
	Model     string   `json:"model"`
	Rationale string   `json:"rationale"`
	Cost      *float64 `json:"cost,omitempty"`
	Unpriced  string   `json:"unpriced,omitempty"`
	Hosts     int      `json:"hosts,omitempty"`
}

// CloudSpend is actual money that actually left the account, for
// cloud_peer traffic through the front in the same window.
type CloudSpend struct {
	// Measured is false when fleetd has no cloud rows at all (no
	// cloud_peer defs, or the front's activity store is off).
	Measured bool     `json:"measured"`
	Reason   string   `json:"reason,omitempty"`
	Cost     *float64 `json:"cost,omitempty"`
	Req      int64    `json:"req"`
	InFresh  int64    `json:"in_fresh"`
	InCached int64    `json:"in_cached"`
	Out      int64    `json:"out"`
	Unpriced []string `json:"unpriced,omitempty"`
}

// Payback is one cell's lifetime scoreboard. Lifetime, not windowed: a
// scoreboard is a running total against a fixed threshold.
type Payback struct {
	Cell        string  `json:"cell"`
	CapitalCost float64 `json:"capital_cost"`
	CapitalNote string  `json:"capital_note"`
	Recovered   float64 `json:"recovered"`
	// RecoveredPct is clamped nowhere: a cell past 100% says so.
	RecoveredPct float64 `json:"recovered_pct"`
	// BreakEvenDay is the exact day the running total crossed the
	// capital number, walked from the daily series. Empty until it is
	// crossed.
	BreakEvenDay string `json:"break_even_day,omitempty"`
	// Projection is the trailing-28-day extrapolation, already rendered
	// as the string the page shows: a month, ">10 years at this rate",
	// "—", or "too early to project".
	Projection string `json:"projection"`
	// DaysCovered is how many days of this cell's ledger the projection
	// had to work with.
	DaysCovered int `json:"days_covered"`
}

// savingsCaveat is C7b §9, rendered under the hero and collapsed by
// default. It is part of the payload rather than the page because the
// MCP tool serves the same document to agents, and an agent quoting the
// number without the caveat is exactly the failure mode.
const savingsCaveat = "This is an estimate of money NOT SPENT, and it is an upper bound. " +
	"It counts only the tokens the fleet actually measured — anything served while a cell was unmeasured, " +
	"restarting, cancelled mid-stream, or bypassing the front is missing, so the token totals are a floor. " +
	"Those tokens are priced against the same open-weight model rented from a real host, at the rates in effect " +
	"on the day the work ran, because pricing a 27B local model as a frontier model moves this number about " +
	"seventy-fold and tells you nothing. Prompt tokens are split into fresh and cache-read and priced separately, " +
	"because most of an agentic prompt is a re-sent prefix that a host bills at a fraction of the rate. " +
	"The figure assumes you would have paid for every one of these tokens, which you would not have — local models " +
	"get used casually precisely because the marginal token is free — which is why actual cloud spend sits beside it " +
	"at the same size. Electricity is subtracted from declared wattage, not measured, and is good to roughly ±30%. " +
	"Payback counts the capital number in hosts.yaml and nothing else — not the hours spent building and running the " +
	"fleet, which very likely exceed everything shown here. " +
	"The token figures are priced in the vendored table's currency and the electricity and capital figures in the one " +
	"declared in hosts.yaml; when those differ this report refuses to net them rather than subtracting across an " +
	"exchange rate it does not have."

// hostedTwinFootnote is the honest defence of comparing a Q5/Q6 local
// build against a hosted twin, rather than pretending quantization does
// not exist.
const hostedTwinFootnote = "Hosted twins are themselves quantized (fp8 is the common serving format), " +
	"which is the honest defence of comparing a Q5/Q6 local build against them."

// ─── aggregation ────────────────────────────────────────────────────────────

// modelKey identifies one priced row inside a cell-day.
type modelKey struct {
	Model string
	Basis string
}

// counts is the mutable accumulator behind a modelKey.
type counts struct {
	Req, InFresh, InCached, Out int64
	PokeReq, ErrReq, Unmeasured int64
	BusyMS                      int64
}

func (c *counts) add(b UsageBucket) {
	c.Req += b.Req
	c.InFresh += b.InFresh
	c.InCached += b.InCached
	c.Out += b.Out
	c.PokeReq += b.PokeReq
	c.ErrReq += b.ErrReq
	c.Unmeasured += b.UnmeasuredReq
	c.BusyMS += b.BusyMS
}

// cellDay is one (day, cell) slice of the ledger.
type cellDay struct {
	models map[modelKey]*counts
	// residentByModel is per-model residency; cellResidentS is the
	// cell-level row (model ""), which is the energy denominator when it
	// exists.
	residentByModel map[string]int64
	cellResidentS   int64
	lostRows        int64
}

func newCellDay() *cellDay {
	return &cellDay{models: map[modelKey]*counts{}, residentByModel: map[string]int64{}}
}

// residentSeconds is the cell's wall time with a model resident.
//
// The cell-level row is authoritative when present. The fallback is the
// MAX across models, never the sum: two models resident at once would
// otherwise bill the box's idle watts twice. Max under-counts a cell
// that alternated models through the day, which is the conservative
// direction for a term that is subtracted from savings.
func (d *cellDay) residentSeconds() int64 {
	if d.cellResidentS > 0 {
		return d.cellResidentS
	}
	var max int64
	for _, s := range d.residentByModel {
		if s > max {
			max = s
		}
	}
	return max
}

func (d *cellDay) busySeconds() int64 {
	var ms int64
	for _, c := range d.models {
		ms += c.BusyMS
	}
	secs := ms / 1000
	// Concurrent requests each report their own wall time, so the sum can
	// exceed the day's residency. Cap it: a cell cannot be busy longer
	// than it was on.
	if res := d.residentSeconds(); res > 0 && secs > res {
		secs = res
	}
	return secs
}

// dayNet is one cell-day's priced result, the payback series' element.
type dayNet struct {
	Gross      float64
	Power      float64
	PowerKnown bool
	Measured   bool
}

func (d dayNet) net() float64 { return d.Gross - d.Power }

// ─── entry point ────────────────────────────────────────────────────────────

// Savings prices the ledger. Exported so fleetmcp serves the identical
// document to agents — the page, the endpoint and the agents can never
// disagree about the money.
//
// Deliberately NOT hung off Snapshot: every SSE line triggers a debounced
// full state re-GET, StateSnapshot goes verbatim to agents through
// fleet_status, and Snapshot runs under a 3s probe budget a ledger read
// would eat.
func (s *Server) Savings(ctx context.Context, window string) (SavingsReport, error) {
	window = strings.TrimSpace(strings.ToLower(window))
	if window == "" {
		window = defaultSavingsWindow
	}
	days, err := parseWindow(window)
	if err != nil {
		return SavingsReport{}, err
	}
	loc := s.UsageTZ()
	now := time.Now().In(loc)

	table := s.priceTable()
	rep := SavingsReport{
		GeneratedAt: time.Now().UTC(),
		Window:      window,
		TZ:          loc.String(),
		Caveat:      savingsCaveat,
		Currency:    currencyOf(s.hosts),
		Cells:       []CellSavings{},
		Payback:     []Payback{},
		Prices: PriceTableInfo{
			AsOf:      table.AsOf(),
			Oldest:    table.Oldest(),
			Example:   table.Example,
			Stale:     table.Stale(time.Now()),
			Snapshots: len(table.Snapshots),
		},
	}
	if len(table.Snapshots) > 0 {
		rep.Prices.Sources = table.Snapshots[len(table.Snapshots)-1].Sources
	}
	if days > 0 {
		rep.SinceDay = dayKey(now.AddDate(0, 0, -(days-1)), loc)
	}
	if table.Example {
		rep.Notes = append(rep.Notes, "the vendored price table is EXAMPLE DATA — synthetic rates, not real prices")
	}
	rep.Notes = append(rep.Notes, hostedTwinFootnote)

	report := s.UsageReport("")
	if len(report.Buckets) == 0 {
		rep.Empty = true
		rep.Cloud = CloudSpend{Reason: "no usage ledger yet"}
		return rep, nil
	}

	// One pass into (day, cell) slices; pricing is a second pass, because
	// energy needs a whole cell-day (residency and busy time arrive on
	// different buckets than the tokens).
	byDay := map[string]map[string]*cellDay{}
	for _, b := range report.Buckets {
		day := byDay[b.Day]
		if day == nil {
			day = map[string]*cellDay{}
			byDay[b.Day] = day
		}
		cd := day[b.Cell]
		if cd == nil {
			cd = newCellDay()
			day[b.Cell] = cd
		}
		switch b.Basis {
		case usageBasisResident:
			if b.Model == "" {
				cd.cellResidentS += b.ResidentS
			} else {
				cd.residentByModel[b.Model] += b.ResidentS
			}
		case usageBasisCell:
			cd.lostRows += b.LostRows
		default:
			if b.Model == "" {
				continue
			}
			k := modelKey{Model: b.Model, Basis: b.Basis}
			c := cd.models[k]
			if c == nil {
				c = &counts{}
				cd.models[k] = c
			}
			c.add(b)
		}
	}

	if err := s.priceLedger(ctx, &rep, byDay, table, now, loc); err != nil {
		return SavingsReport{}, err
	}
	return rep, nil
}

// priceLedger walks the aggregated ledger, prices every cell-day, and
// fills the window table, the totals, the cloud line and the payback
// strip.
func (s *Server) priceLedger(ctx context.Context, rep *SavingsReport, byDay map[string]map[string]*cellDay, table *prices.Table, now time.Time, loc *time.Location) error {
	hosts := s.hosts
	// One resolution per snapshot GENERATION, not per day: payback is
	// lifetime, so even a 30d window walks every day the ledger holds,
	// and resolving the vendored table per day cost ~1.3 s per request at
	// 400 days of history.
	resolveAt := table.Resolver()

	type cellAgg struct {
		models     map[modelKey]*ModelSavings
		gross      float64
		low, high  float64
		frontier   float64
		power      float64
		powerKnown bool
		priced     int64
		total      int64
		anyPriced  bool
		req        int64
		fresh      int64
		cached     int64
		out        int64
		unmeasured int64
		errReq     int64
		lostRows   int64
		pokeReq    int64
		measured   bool
	}
	windowCells := map[string]*cellAgg{}
	cellAggFor := func(cell string) *cellAgg {
		a := windowCells[cell]
		if a == nil {
			a = &cellAgg{models: map[modelKey]*ModelSavings{}}
			windowCells[cell] = a
		}
		return a
	}
	lifetime := map[string]map[string]*dayNet{}
	lifeDay := func(cell, day string) *dayNet {
		byCell := lifetime[cell]
		if byCell == nil {
			byCell = map[string]*dayNet{}
			lifetime[cell] = byCell
		}
		d := byCell[day]
		if d == nil {
			d = &dayNet{}
			byCell[day] = d
		}
		return d
	}
	cloud := CloudSpend{}
	cloudUnpriced := map[string]bool{}
	var frontierUnpriced string
	frontierHosts := 0

	for _, day := range slices.Sorted(maps.Keys(byDay)) {
		// A whole-history walk is cheap but not free, and the caller is an
		// HTTP request. Abandoning it on disconnect returns an ERROR rather
		// than a short report: a savings document missing three days would
		// be indistinguishable from a fleet that served nothing those days.
		if err := ctx.Err(); err != nil {
			return err
		}
		res := resolveAt(day)
		inWindow := rep.SinceDay == "" || day >= rep.SinceDay
		for cell, cd := range byDay[day] {
			ld := lifeDay(cell, day)
			var agg *cellAgg
			if inWindow {
				agg = cellAggFor(cell)
			}
			for _, k := range sortedModelKeys(cd.models) {
				c := cd.models[k]
				if k.Basis == prices.BasisCloud {
					priceCloudRow(res, hosts, k, c, &cloud, cloudUnpriced, inWindow)
					continue
				}
				mp, _ := hosts.ModelPricingFor(k.Model)
				spread, priced, reason := priceTwin(res, mp, k, c)
				mult := mp.Multiplier()
				if priced {
					ld.Gross += spread.Median * mult
					ld.Measured = true
				} else if c.Req > 0 {
					ld.Measured = true
				}
				if agg == nil {
					continue
				}
				agg.measured = agg.measured || c.Req > 0 || c.Unmeasured > 0 || c.ErrReq > 0
				agg.req += c.Req
				agg.fresh += c.InFresh
				agg.cached += c.InCached
				agg.out += c.Out
				agg.unmeasured += c.Unmeasured
				agg.errReq += c.ErrReq
				agg.pokeReq += c.PokeReq
				billable := prices.BillableInput(prices.Counts{Basis: k.Basis, InFresh: c.InFresh, InCached: c.InCached}) + c.Out
				agg.total += billable

				row := agg.models[k]
				if row == nil {
					row = &ModelSavings{Model: k.Model, Basis: k.Basis, Twin: mp.Twin,
						Counterfactual: counterfactualLabel(mp), Multiplier: mult}
					agg.models[k] = row
				}
				row.Req += c.Req
				row.InFresh += c.InFresh
				row.InCached += c.InCached
				row.Out += c.Out
				if !priced {
					if row.Unpriced == "" {
						row.Unpriced = reason
					}
					continue
				}
				row.Priced = true
				row.Unpriced = ""
				agg.anyPriced = true
				row.Hosts = spread.N
				row.RateLow, row.RateHigh = spread.InRateMin, spread.InRateMax
				row.NoCacheDiscount = spread.NoCacheDiscount
				addMoney(&row.Cost, spread.Median*mult)
				addMoney(&row.CostLow, spread.Min*mult)
				addMoney(&row.CostHigh, spread.Max*mult)
				agg.gross += spread.Median * mult
				agg.low += spread.Min * mult
				agg.high += spread.Max * mult
				agg.priced += billable

				// The frontier comparable prices the same token set the
				// twin priced, not every measured token: a ratio between
				// two different token sets would not be the 72x anyone is
				// arguing about.
				if fr := frontierOf(hosts); fr != nil {
					fspread, fpriced, freason := priceFrontier(res, fr, k, c)
					if fpriced {
						agg.frontier += fspread.Median * mult
						frontierHosts = fspread.N
					} else if frontierUnpriced == "" {
						frontierUnpriced = freason
					}
				}
			}

			cost, known := cellPowerCost(hosts, cell, cd)
			if known {
				ld.Power += cost
				ld.PowerKnown = true
				if agg != nil {
					agg.power += cost
					agg.powerKnown = true
				}
			}
			if agg != nil {
				agg.lostRows += cd.lostRows
			}
		}
	}

	// The registry is the row set, not the ledger: a cell that announced
	// nothing must still appear, as an em dash with a reason.
	names := map[string]bool{}
	for _, c := range s.cells {
		if c.Name == fleetcfg.FrontCell {
			continue
		}
		names[c.Name] = true
	}
	for name := range windowCells {
		// The front is not a serving cell here: C7a folds no token rows
		// for it (the whitelist rule), and its only ledger presence is the
		// cloud_peer reconstruction, which has its own line.
		if name == fleetcfg.FrontCell {
			continue
		}
		names[name] = true
	}

	var totals SavingsTotals
	totals.CellsTotal = len(names)
	powerCounted := false
	// Cells that served measured work and produced no electricity figure.
	// The fleet net is exactly that much too generous, and a partial
	// power term is the one place this screen errs LARGE, so it says so.
	var powerMissing []string
	for _, name := range slices.Sorted(maps.Keys(names)) {
		agg := windowCells[name]
		row := CellSavings{Cell: name}
		if agg == nil || !agg.measured {
			row.Reason = "no measured requests in this window"
			// Electricity is a COST, not a saving. Excluding an unmeasured
			// cell from the SAVINGS total is §8's rule; dropping its power
			// with it inflates the fleet net, and the cell this hits is
			// precisely the one §4 was written for — a box holding a C4 warm
			// target resident all day, burning its idle constant, serving
			// nothing a human asked for. The payback strip already charged
			// those watts; the window table used not to, and the two
			// disagreed.
			if agg != nil && agg.powerKnown {
				row.Power = money(agg.power)
				row.PowerDeclared = true
				row.Reason = "no measured requests in this window; electricity still counted"
				// The net column is denominated in the TOKEN currency
				// throughout. A bare negative electricity figure needs no
				// conversion, but printing it in this column under a mixed
				// pair would put one row's CAD beside another row's USD under
				// one heading — which is the same defect one column over.
				if rep.Currency.canNet() {
					row.Net = money(-agg.power)
					row.NetLabel = "net (nothing priced)"
				} else {
					row.NetLabel = netUnavailableLabel(rep.Currency)
				}
				addMoney(&totals.Power, agg.power)
				powerCounted = true
			}
			rep.Cells = append(rep.Cells, row)
			continue
		}
		row.Measured = true
		totals.CellsMeasured++
		row.Req, row.InFresh, row.InCached, row.Out = agg.req, agg.fresh, agg.cached, agg.out
		if agg.powerKnown {
			row.Power = money(agg.power)
			row.PowerDeclared = true
			powerCounted = true
		} else {
			powerMissing = append(powerMissing, name)
		}
		// A cell that served requests none of which could be PRICED is not
		// a cell that saved $0.00. Money stays absent and the reason says
		// so — while the TOKENS stay in every token column, which is the
		// whole point of "N% of tokens priced". $0 renders only for a
		// genuine measured zero.
		switch {
		case !agg.anyPriced:
			row.Reason = "nothing priced in this window (no twin declared, or no published rate)"
			row.NetLabel = "net (nothing priced)"
		default:
			row.Gross, row.GrossLow, row.GrossHigh = money(agg.gross), money(agg.low), money(agg.high)
			row.Net, row.NetLabel = netOf(agg.gross, agg.power, agg.powerKnown, rep.Currency)
		}
		row.TokensPricedPct = pct(agg.priced, agg.total)
		row.Partial = partialNote(agg.req, agg.unmeasured, agg.errReq, agg.pokeReq, agg.lostRows)
		for _, k := range sortedModelKeys(agg.models) {
			m := *agg.models[k]
			m.SharePct = pct(prices.BillableInput(prices.Counts{Basis: k.Basis, InFresh: m.InFresh, InCached: m.InCached})+m.Out, agg.total)
			row.Models = append(row.Models, m)
		}
		rep.Cells = append(rep.Cells, row)

		totals.Req += agg.req
		totals.InFresh += agg.fresh
		totals.InCached += agg.cached
		totals.Out += agg.out
		if agg.anyPriced {
			addMoney(&totals.Gross, agg.gross)
			addMoney(&totals.GrossLow, agg.low)
			addMoney(&totals.GrossHigh, agg.high)
		}
		if agg.powerKnown {
			addMoney(&totals.Power, agg.power)
		}
		if fr := frontierOf(hosts); fr != nil && agg.frontier > 0 {
			if rep.Frontier == nil {
				rep.Frontier = &FrontierLine{Model: fr.Model, Rationale: fr.Rationale, Hosts: frontierHosts}
			}
			addMoney(&rep.Frontier.Cost, agg.frontier)
		}
	}
	if fr := frontierOf(hosts); fr != nil && rep.Frontier == nil {
		rep.Frontier = &FrontierLine{Model: fr.Model, Rationale: fr.Rationale, Unpriced: orDefault(frontierUnpriced, prices.ReasonNoHosts)}
	}

	if totals.Gross != nil {
		power := 0.0
		if totals.Power != nil {
			power = *totals.Power
		}
		totals.Net, totals.NetLabel = netOf(*totals.Gross, power, totals.Power != nil, rep.Currency)
	} else {
		totals.NetLabel = "net (power not counted)"
		if powerCounted {
			totals.NetLabel = "net"
		}
		if !rep.Currency.canNet() {
			totals.NetLabel = netUnavailableLabel(rep.Currency)
		}
	}
	var priced, total int64
	for _, a := range windowCells {
		priced += a.priced
		total += a.total
	}
	totals.TokensPricedPct = pct(priced, total)
	if in := totals.InFresh + totals.InCached; in > 0 {
		p := float64(totals.InCached) / float64(in) * 100
		totals.CachedPct = &p
	}
	rep.Totals = totals

	for id := range cloudUnpriced {
		cloud.Unpriced = append(cloud.Unpriced, id)
	}
	sort.Strings(cloud.Unpriced)
	switch {
	case !cloud.Measured:
		cloud.Reason = "no cloud_peer traffic recorded at the front"
	case cloud.Cost == nil:
		// Traffic WAS measured and none of it could be priced. Saying
		// "not measured" here would be a lie in the flattering direction:
		// the requests happened, the bill exists, and what is missing is a
		// rate. Name the models so the fix is obvious.
		cloud.Reason = "cloud traffic measured but no rate in the price table for " + strings.Join(cloud.Unpriced, ", ") +
			" (map it with pricing.models.<id>.priced_as)"
	}
	rep.Cloud = cloud

	rep.Payback, rep.Notes = s.payback(lifetime, now, loc, rep.Notes, rep.Currency)
	// The inverted rule, on the page (C7b §4): energy lands around 11-16%
	// of net savings against an honest twin-priced headline, so an
	// electricity line that looks like a rounding error is evidence the
	// COMPARABLE is wrong, not evidence that power is free.
	//
	// The ratio is a division across the same seam netOf refuses, so it
	// is gated identically: "electricity is 0.8% of the gross figure" is
	// a claim about an exchange rate when the two are in different
	// currencies.
	if rep.Currency.canNet() && totals.Power != nil && totals.Gross != nil && *totals.Gross > 0 {
		if share := *totals.Power / *totals.Gross * 100; share < 3 {
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"electricity is %.1f%% of the gross figure; against an honest same-model-rented comparable it lands around 11-16%%, so a power line this small usually means the comparable is too expensive",
				share))
		}
	}
	if note := powerGapNote(s.hosts, powerMissing, powerCounted); note != "" {
		rep.Notes = append(rep.Notes, note)
	}
	if rep.Prices.Stale {
		rep.Notes = append(rep.Notes, "the vendored price table is more than "+strconv.Itoa(prices.StaleAfterDays)+" days old; re-run `vibe fleet prices vendor`")
	}
	return nil
}

// priceCloudRow reconstructs actual spend for one cloud_peer model. It
// is priced at the REAL model's real rate — a bill, not a
// counterfactual — and it never enters the savings sum.
func priceCloudRow(res *prices.Resolved, hosts *fleetcfg.File, k modelKey, c *counts, cloud *CloudSpend, unpriced map[string]bool, inWindow bool) {
	if !inWindow {
		return
	}
	cloud.Measured = true
	cloud.Req += c.Req
	cloud.InFresh += c.InFresh
	cloud.InCached += c.InCached
	cloud.Out += c.Out
	id := k.Model
	if mp, ok := hosts.ModelPricingFor(k.Model); ok && mp.PricedAs != "" {
		id = mp.PricedAs
	}
	spread, ok := prices.PriceAcross(res.Hosts(id, false), prices.Counts{
		Basis: k.Basis, Req: c.Req, InFresh: c.InFresh, InCached: c.InCached, Out: c.Out,
	})
	if !ok {
		unpriced[k.Model] = true
		return
	}
	addMoney(&cloud.Cost, spread.Median)
}

// priceTwin prices one local model's counts against its declared twin.
func priceTwin(res *prices.Resolved, mp fleetcfg.ModelPricing, k modelKey, c *counts) (prices.Spread, bool, string) {
	if c.Req == 0 && c.InFresh == 0 && c.Out == 0 {
		return prices.Spread{}, false, "nothing measured"
	}
	if strings.TrimSpace(mp.Twin) == "" {
		return prices.Spread{}, false, prices.ReasonNoTwin
	}
	counts := prices.Counts{Basis: k.Basis, Req: c.Req, InFresh: c.InFresh, InCached: c.InCached, Out: c.Out}
	// openOnly: the twin median's whole premise is the SAME open-weight
	// model rented from a real host.
	spread, ok := prices.PriceAcross(res.Hosts(mp.Twin, true), counts)
	if !ok {
		return spread, false, orDefault(spread.Reason, prices.ReasonNoRate)
	}
	return spread, true, ""
}

// priceFrontier prices the same counts against the declared frontier
// model. Frontier rows are not open-weights, so the open-only filter is
// off here — and that difference is the whole 72x.
func priceFrontier(res *prices.Resolved, fr *fleetcfg.Frontier, k modelKey, c *counts) (prices.Spread, bool, string) {
	counts := prices.Counts{Basis: k.Basis, Req: c.Req, InFresh: c.InFresh, InCached: c.InCached, Out: c.Out}
	spread, ok := prices.PriceAcross(res.Hosts(fr.Model, false), counts)
	if !ok {
		return spread, false, orDefault(spread.Reason, prices.ReasonNoHosts)
	}
	return spread, true, ""
}

// cellPowerCost bills idle and busy separately:
//
//	wh = watts_idle × resident_seconds + (watts_busy − watts_idle) × busy_seconds
//
// A cell holding a warm model 20h/day is mostly reporting its idle
// constant, and C4's warm targets deliberately increase exactly that.
func cellPowerCost(hosts *fleetcfg.File, cell string, cd *cellDay) (float64, bool) {
	if hosts == nil || hosts.Pricing == nil || hosts.Pricing.ElectricityPricePerKWh <= 0 {
		return 0, false
	}
	c, ok := hosts.Cells[cell]
	if !ok || c.Power == nil || c.Power.Source != fleetcfg.PowerDeclared {
		return 0, false
	}
	residentS := float64(cd.residentSeconds())
	busyS := float64(cd.busySeconds())
	if residentS == 0 && busyS == 0 {
		return 0, false
	}
	delta := c.Power.WattsBusy - c.Power.WattsIdle
	if delta < 0 {
		delta = 0
	}
	wh := (c.Power.WattsIdle*residentS + delta*busyS) / 3600
	return wh / 1000 * hosts.Pricing.ElectricityPricePerKWh, true
}

// payback walks each cell's daily series into the lifetime scoreboard.
// It returns the notes it was given plus any it had to add.
func (s *Server) payback(lifetime map[string]map[string]*dayNet, now time.Time, loc *time.Location, notes []string, cur CurrencyInfo) ([]Payback, []string) {
	out := []Payback{}
	if s.hosts == nil {
		return out, notes
	}
	// A payback bar is `recovered / capital_cost` — a token-priced
	// numerator over a locally-declared denominator — and its daily
	// series is `gross − power`. Both cross the currency seam, so a mixed
	// pair gets no bar at all, on the same principle as a cell with no
	// capital_cost: not 0%, not an invented denominator, and here not an
	// invented exchange rate either. The reason is a note, because a
	// screen that silently drops the payback strip is the confusing kind
	// of honest.
	if !cur.canNet() {
		var declared []string
		for _, name := range slices.Sorted(maps.Keys(s.hosts.Cells)) {
			if s.hosts.Cells[name].CapitalCost > 0 {
				declared = append(declared, name)
			}
		}
		if len(declared) > 0 {
			notes = append(notes, "no payback bars: "+strings.Join(declared, ", ")+" declare capital_cost in "+cur.Local+
				" and the savings above are priced in "+cur.Tokens+
				", so the percentage would be a claim about an exchange rate this repo does not ship")
		}
		return out, notes
	}
	for _, name := range slices.Sorted(maps.Keys(s.hosts.Cells)) {
		cfg := s.hosts.Cells[name]
		// No capital_cost → no payback bar at all. Not 0%, not infinity,
		// not an invented denominator.
		if cfg.CapitalCost <= 0 {
			continue
		}
		if name == fleetcfg.FrontCell {
			// The front is structurally excluded from the savings table
			// (C7a folds no token rows for it), so its numerator is defined
			// to be zero. A bar that can only ever read "0% of $N" is a
			// screen making a claim about hardware it never measured — the
			// exact failure mode §8 exists to prevent. Say it in words
			// instead of drawing it.
			notes = append(notes, "cell "+name+" declares capital_cost, but the front is not a serving cell in this ledger "+
				"(its only presence is the cloud reconstruction), so it gets no payback bar rather than a permanent 0%")
			continue
		}
		p := Payback{Cell: name, CapitalCost: cfg.CapitalCost, CapitalNote: cfg.CapitalNote}
		series := lifetime[name]
		days := slices.Sorted(maps.Keys(series))
		var cum float64
		for _, d := range days {
			cum += series[d].net()
			if series[d].Measured {
				p.DaysCovered++
			}
			if p.BreakEvenDay == "" && cum >= cfg.CapitalCost {
				p.BreakEvenDay = d
			}
		}
		p.Recovered = cum
		p.RecoveredPct = cum / cfg.CapitalCost * 100
		p.Projection = project(series, days, cum, cfg.CapitalCost, p.DaysCovered, now, loc)
		out = append(out, p)
	}
	return out, notes
}

// project renders the trailing-rate projection as the string the page
// shows. The clamp is deliberate: on twin-priced arithmetic a real cell
// plausibly reads "3% of $3,100 · >10 years at this rate", and a screen
// that can only render triumph will render triumph regardless of the
// truth.
func project(series map[string]*dayNet, days []string, cum, capital float64, covered int, now time.Time, loc *time.Location) string {
	if cum >= capital {
		return "paid for itself"
	}
	if covered < projectionMinDays {
		return "too early to project"
	}
	cutoff := dayKey(now.AddDate(0, 0, -(projectionWindowDays-1)), loc)
	var recent float64
	n := 0
	for _, d := range days {
		if d < cutoff {
			continue
		}
		recent += series[d].net()
		n++
	}
	if n == 0 || recent <= 0 {
		return "—"
	}
	// Divide by the WINDOW, not by the days that had traffic: a day the
	// fleet served nothing earned nothing, and averaging only over busy
	// days would project the fleet's best fortnight forever.
	perDay := recent / float64(projectionWindowDays)
	remaining := capital - cum
	daysLeft := remaining / perDay
	if daysLeft > projectionClampYears*365 {
		return fmt.Sprintf(">%d years at this rate", projectionClampYears)
	}
	return "break-even ~" + now.AddDate(0, 0, int(math.Ceil(daysLeft))).Format("2006-01")
}

// partialNote states measurement loss in the page's own words. It is
// never folded into an estimate: unmeasured rows are 200s that reported
// no tokens (mlx streaming, cancelled streams), and estimating them from
// duration would borrow another row's rate.
func partialNote(req, unmeasured, errReq, pokeReq, lost int64) string {
	var bits []string
	if unmeasured > 0 {
		bits = append(bits, fmt.Sprintf("partial — %d of %d requests reported tokens", req, req+unmeasured))
	}
	if errReq > 0 {
		bits = append(bits, fmt.Sprintf("%d errored requests (unbilled)", errReq))
	}
	if pokeReq > 0 {
		// C7a calls poke_req a VISIBLE counter for a reason: C4's warm
		// loops issue real metered 1-token completions and can outnumber
		// human requests. They are excluded from every token sum, so a
		// screen that never mentions them makes the exclusion invisible.
		bits = append(bits, fmt.Sprintf("%d warm-loop pokes (excluded from every token sum)", pokeReq))
	}
	if lost > 0 {
		bits = append(bits, fmt.Sprintf("%d activity rows aged out before they could be read", lost))
	}
	return strings.Join(bits, "; ")
}

func counterfactualLabel(mp fleetcfg.ModelPricing) string {
	if mp.Counterfactual == "" {
		return fleetcfg.CounterfactualInteractive
	}
	return mp.Counterfactual
}

func frontierOf(hosts *fleetcfg.File) *fleetcfg.Frontier {
	if hosts == nil || hosts.Pricing == nil {
		return nil
	}
	return hosts.Pricing.Frontier
}

// powerGapNote names the cells whose electricity went uncounted, and
// WHY. The reason matters: the likeliest cause is a fleet that declared
// wattage on every cell and no electricity price, which a note blaming a
// missing `power:` block sends the reader to fix in the wrong file. A
// PARTIAL gap gets a note too — subtracting some cells' power from a
// fleet-wide gross is the one place this screen errs toward a larger
// number, so it must not be silent.
func powerGapNote(hosts *fleetcfg.File, missing []string, anyCounted bool) string {
	if len(missing) == 0 {
		return ""
	}
	if hosts == nil || hosts.Pricing == nil || hosts.Pricing.ElectricityPricePerKWh <= 0 {
		return "electricity is not counted anywhere: pricing.electricity_price_per_kwh is unset, " +
			"so every cell's power renders an em dash and every net is a gross"
	}
	who := strings.Join(missing, ", ")
	if !anyCounted {
		return "electricity is not counted for " + who +
			" (no power: block, or no residency and no busy time sampled yet)"
	}
	return "electricity is counted for only some cells — " + who +
		" contributed none, so the fleet net is that much too generous"
}

// netOf combines a token-priced gross with a locally-priced electricity
// term, and is the ONE place in this file where the two sides of the
// screen meet. It refuses the subtraction when they are denominated
// differently: `gross − power` across two currencies is not an
// approximation, it is a different quantity, and it renders as a
// perfectly plausible dollar figure with no visible symptom. An absent
// net with a label that says why is this screen's existing answer for
// every other thing it does not know.
func netOf(gross, power float64, powerKnown bool, cur CurrencyInfo) (*float64, string) {
	if !powerKnown {
		return money(gross), "net (power not counted)"
	}
	if !cur.canNet() {
		return nil, netUnavailableLabel(cur)
	}
	return money(gross - power), "net"
}

// netUnavailableLabel is the sentence that stands where a net would be.
func netUnavailableLabel(cur CurrencyInfo) string {
	return "net unavailable — savings are priced in " + cur.Tokens + " and electricity in " + cur.Local +
		", and no exchange rate is declared"
}

// money boxes a float. Every money field on this report is a pointer so
// "we don't know" is a different value from "zero".
func money(v float64) *float64 {
	out := v
	return &out
}

func addMoney(dst **float64, v float64) {
	if *dst == nil {
		*dst = money(v)
		return
	}
	**dst += v
}

func pct(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func sortedModelKeys[V any](m map[modelKey]V) []modelKey {
	out := make([]modelKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b modelKey) int {
		if c := strings.Compare(a.Model, b.Model); c != 0 {
			return c
		}
		return strings.Compare(a.Basis, b.Basis)
	})
	return out
}

// parseWindow accepts "all" or "<n>d". Days, not hours: the ledger's
// buckets ARE days, and an hours window would silently round to one.
func parseWindow(w string) (int, error) {
	if w == WindowAll {
		return 0, nil
	}
	if !strings.HasSuffix(w, "d") {
		return 0, fmt.Errorf("window must be %q or <n>d (got %q)", WindowAll, w)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(w, "d"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("window must be %q or a positive <n>d (got %q)", WindowAll, w)
	}
	if n > 36600 {
		return 0, fmt.Errorf("window %q is longer than any ledger will ever hold; use %q", w, WindowAll)
	}
	return n, nil
}

// priceTable resolves the table this server prices with: the injected
// one (tests) or the vendored embedded artifact. A table that fails to
// parse degrades to an empty one — every model then renders "unpriced",
// which is the truth, rather than costing the whole page.
func (s *Server) priceTable() *prices.Table {
	if s.prices != nil {
		return s.prices
	}
	t, err := prices.Embedded()
	if err != nil || t == nil {
		return &prices.Table{Schema: prices.SchemaVersion, Notice: "price table unreadable: " + errText(err)}
	}
	return t
}

func errText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// handleSavings serves GET /api/fleet/savings?window=7d|30d|all. Read
// only: the savings view has no action buttons, which is what keeps
// C4's "the page adds no mutation surface" claim literally true.
func (s *Server) handleSavings(w http.ResponseWriter, r *http.Request) {
	rep, err := s.Savings(r.Context(), r.URL.Query().Get("window"))
	if err != nil {
		// A cancelled request is not a malformed one. Savings aborts on
		// ctx.Err() rather than returning a short document, and reporting
		// that abort as a 400 would make a client disconnect and a typo'd
		// window indistinguishable in the logs of the one endpoint whose
		// whole job is not lying about what it knows.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}
