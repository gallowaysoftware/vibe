// Package prices is the vendored, dated model price table and the
// arithmetic that turns raw token counts into money (fleet-control C7b
// §2/§3).
//
// Three properties are load-bearing and none of them is a style choice:
//
//   - The table is VENDORED and embedded. The savings screen must render
//     on a LAN with no internet, and a config-declared table would make
//     every price an unsourced house value. Vendoring a DATA file is not
//     a dependency (plan ground rule 5); `go mod tidy` still reports no
//     new module.
//   - Prices are DATED. A day bucket is priced at the newest snapshot
//     whose effective_from is on or before that day. Re-pricing history
//     at today's rates would retroactively erase money that genuinely was
//     not spent at the time — the Opus tier once fell 67% in one release.
//   - A price of exactly 0 means UNKNOWN, never free. Dozens of upstream
//     chat, embedding and rerank rows carry 0. A zero that prices as free
//     makes the embed cell report $0.00 forever and nobody notices.
package prices

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed prices.json
var tableFS embed.FS

// TableFile is the vendored artifact's name inside this package — the
// path `vibe fleet prices vendor` rewrites in a checkout.
const TableFile = "prices.json"

// SchemaVersion is the vendored artifact's schema. Bumping it is a
// breaking change to a file this package embeds, so the loader refuses a
// version it does not know rather than mis-reading prices.
const SchemaVersion = 1

// StaleAfterDays is how old the newest snapshot may get before the page
// shows a staleness chip. A stale table must be VISIBLE, not silently
// wrong.
const StaleAfterDays = 90

// Source records one upstream the table was built from, with its licence
// and the commit it was fetched at. Both upstreams are MIT; the notice
// travels with the data.
type Source struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Licence string `json:"licence"`
	Commit  string `json:"commit,omitempty"`
	Fetched string `json:"fetched,omitempty"`
	Rows    int    `json:"rows,omitempty"`
}

// Disagreement is one cross-source price conflict past the ratio bound.
// The row is DROPPED from the table (an unresolvable conflict is an
// unknown price, and unknown renders "unpriced"), and the conflict is
// carried in the artifact so the drop is auditable rather than silent.
type Disagreement struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Field    string  `json:"field"`
	A        float64 `json:"a"`
	B        float64 `json:"b"`
}

// Row is one (provider, model) price point, normalized to USD per
// million tokens. Field names are one letter because the base snapshot
// carries a few thousand of them.
type Row struct {
	Provider string `json:"p"`
	Model    string `json:"m"`
	// Key is the normalized model name — the join key across providers,
	// because every host spells the same open-weight model differently
	// (accounts/fireworks/models/…, Qwen/Qwen3-…, qwen3-…).
	Key string `json:"k"`
	// In/Out/CacheRead are USD per million tokens; PerQuery is USD per
	// request (the rerank family, whose token price is literally 0).
	In        float64 `json:"i,omitempty"`
	Out       float64 `json:"o,omitempty"`
	CacheRead float64 `json:"c,omitempty"`
	PerQuery  float64 `json:"q,omitempty"`
	// Open marks an open-weights model. The twin median exists only over
	// these: the whole point is "what would this exact model have cost
	// rented from a real host".
	Open bool `json:"w,omitempty"`
	// Mode is "chat" (empty), "embedding" or "rerank". Chat is the
	// default because it is most of the table.
	Mode string `json:"d,omitempty"`
	// Deprecated rows never price anything; they stay in the artifact so
	// an older day can still be priced at the rate that was in effect.
	Deprecated bool `json:"x,omitempty"`
}

// Modes.
const (
	ModeChat      = "chat"
	ModeEmbedding = "embedding"
	ModeRerank    = "rerank"
)

// Snapshot is one dated layer. The first snapshot is the base (a full
// table); later ones are overlays carrying only rows whose price changed
// plus rows that disappeared, which is what keeps a multi-year history
// inside one embedded file.
type Snapshot struct {
	EffectiveFrom string         `json:"effective_from"`
	Base          bool           `json:"base,omitempty"`
	Rows          []Row          `json:"rows"`
	Removed       []string       `json:"removed,omitempty"`
	Sources       []Source       `json:"sources,omitempty"`
	Disagreements []Disagreement `json:"disagreements,omitempty"`
}

// Table is the whole vendored artifact.
type Table struct {
	Schema int `json:"schema"`
	// Example marks a synthetic table. It is rendered on the page: a
	// screen priced from made-up rates must say so on its face.
	Example   bool       `json:"example_data,omitempty"`
	Notice    string     `json:"notice"`
	Licences  []string   `json:"licences,omitempty"`
	Snapshots []Snapshot `json:"snapshots"`
}

var (
	embedOnce  sync.Once
	embedTable *Table
	embedErr   error
)

// Embedded returns the vendored table. Parsed once; the result is shared
// and must be treated as read-only.
func Embedded() (*Table, error) {
	embedOnce.Do(func() {
		data, err := tableFS.ReadFile(TableFile)
		if err != nil {
			embedErr = err
			return
		}
		embedTable, embedErr = Parse(data)
	})
	return embedTable, embedErr
}

// Parse decodes and validates a table artifact.
func Parse(data []byte) (*Table, error) {
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse price table: %w", err)
	}
	if t.Schema != SchemaVersion {
		return nil, fmt.Errorf("price table schema %d, this build reads %d", t.Schema, SchemaVersion)
	}
	if len(t.Snapshots) == 0 {
		return nil, fmt.Errorf("price table has no snapshots")
	}
	if !t.Snapshots[0].Base {
		return nil, fmt.Errorf("price table's first snapshot must be the base (a full table)")
	}
	for i, s := range t.Snapshots {
		if _, err := time.Parse("2006-01-02", s.EffectiveFrom); err != nil {
			return nil, fmt.Errorf("snapshot %d: effective_from %q is not YYYY-MM-DD", i, s.EffectiveFrom)
		}
		if i > 0 && s.EffectiveFrom < t.Snapshots[i-1].EffectiveFrom {
			return nil, fmt.Errorf("snapshot %d (%s) is older than its predecessor (%s); snapshots are append-only",
				i, s.EffectiveFrom, t.Snapshots[i-1].EffectiveFrom)
		}
	}
	return &t, nil
}

// AsOf is the newest snapshot date in the table.
func (t *Table) AsOf() string {
	if t == nil || len(t.Snapshots) == 0 {
		return ""
	}
	return t.Snapshots[len(t.Snapshots)-1].EffectiveFrom
}

// Oldest is the base snapshot's date — the horizon before which a day
// cannot be priced at the rates of its own time.
func (t *Table) Oldest() string {
	if t == nil || len(t.Snapshots) == 0 {
		return ""
	}
	return t.Snapshots[0].EffectiveFrom
}

// Stale reports whether the newest snapshot is older than StaleAfterDays
// relative to now.
func (t *Table) Stale(now time.Time) bool {
	asOf, err := time.Parse("2006-01-02", t.AsOf())
	if err != nil {
		return true
	}
	return now.UTC().Sub(asOf) > StaleAfterDays*24*time.Hour
}

// rowID is a row's overlay identity.
func rowID(r Row) string { return r.Provider + "\x00" + r.Model }

// Resolved is the table as of one day: the base with every overlay up to
// that day applied.
type Resolved struct {
	// EffectiveFrom is the snapshot the day resolved to.
	EffectiveFrom string
	// BeforeBase marks a day older than the base snapshot. It is priced
	// at the base anyway (there is nothing older), and the caller says so
	// on the page rather than dropping the day.
	BeforeBase bool

	byKey map[string][]Row
}

// At resolves the table for one YYYY-MM-DD day.
func (t *Table) At(day string) *Resolved {
	res := &Resolved{byKey: map[string][]Row{}}
	if t == nil || len(t.Snapshots) == 0 {
		return res
	}
	rows := map[string]Row{}
	applied := 0
	for _, s := range t.Snapshots {
		if s.EffectiveFrom > day && applied > 0 {
			break
		}
		if s.Base {
			rows = map[string]Row{}
		}
		for _, r := range s.Rows {
			rows[rowID(r)] = r
		}
		for _, id := range s.Removed {
			delete(rows, id)
		}
		res.EffectiveFrom = s.EffectiveFrom
		applied++
		// A day older than the base still prices at the base: pricing it
		// at nothing would report a fleet that served traffic as having
		// saved zero, which is a stronger claim than "we don't know what
		// the rates were that far back".
		if s.EffectiveFrom > day {
			res.BeforeBase = true
			break
		}
	}
	for _, r := range rows {
		res.byKey[r.Key] = append(res.byKey[r.Key], r)
		if alt := Normalize(r.Model); alt != "" && alt != r.Key {
			res.byKey[alt] = append(res.byKey[alt], r)
		}
	}
	for k := range res.byKey {
		slices.SortFunc(res.byKey[k], func(a, b Row) int { return strings.Compare(rowID(a), rowID(b)) })
	}
	return res
}

// Normalize reduces a model id or name to its join key: the last path
// segment, lowercased, with every non-alphanumeric character dropped. It
// is what makes "Qwen/Qwen3-Coder-30B-A3B-Instruct",
// "accounts/fireworks/models/qwen3-coder-30b-a3b-instruct" and
// "Qwen3 Coder 30B A3B Instruct" the same model.
func Normalize(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Hosts returns every row matching the model id/name. openOnly restricts
// to open-weights rows — the twin median's whole premise.
func (r *Resolved) Hosts(model string, openOnly bool) []Row {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Row
	for _, row := range r.byKey[Normalize(model)] {
		if openOnly && !row.Open {
			continue
		}
		if row.Deprecated {
			continue
		}
		id := rowID(row)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, row)
	}
	return out
}

// Counts is one priceable workload: the raw ledger counters plus the
// basis that produced them.
type Counts struct {
	// Basis is C7a's token-semantics branch ("chat", "embed", "other",
	// "cloud"). It decides what "billable input" means, and getting it
	// backwards is 1.8x low or 5x high.
	Basis    string
	Req      int64
	InFresh  int64
	InCached int64
	Out      int64
}

// Bases as they appear in the ledger. Duplicated here rather than
// imported: this package must not depend on the fleet packages (the
// vendoring tool links it into the CLI), and these four strings are wire
// format, not internal API.
const (
	BasisChat  = "chat"
	BasisEmbed = "embed"
	BasisOther = "other"
	BasisCloud = "cloud"
)

// CachedIsBillable reports whether cached prompt tokens are billable
// input for this basis.
//
// The direction of the trap INVERTS by endpoint, which is exactly why
// C7a records the basis per row. On chat rows llama.cpp's prompt_n is
// cache-MISS only, so billable input is fresh + cached. On embed/rerank
// rows the OpenAI usage figure already includes the cache, so C7a stored
// the whole prompt in InFresh and adding InCached would count it twice.
func CachedIsBillable(basis string) bool {
	switch basis {
	case BasisEmbed, BasisOther:
		return false
	default:
		// chat, cloud and anything a newer cell invents: fresh + cached.
		// An unknown basis from a newer cell is welcome on the wire (C7a),
		// and the chat arithmetic degenerates correctly when the cell
		// reports no cache at all.
		return true
	}
}

// Quote is one row's answer for one workload.
type Quote struct {
	Row  Row
	Cost float64
	// NoCacheDiscount marks a row that charges cache reads at the full
	// input rate because the host publishes no cache price. Only a
	// minority of open-weight hosts publish one, so this is the common
	// case and the page says so rather than implying a discount.
	NoCacheDiscount bool
}

// Unpriceable reasons, rendered verbatim on the page.
const (
	ReasonNoRate       = "no published rate"
	ReasonZeroRate     = "published rate is 0 (unknown, not free)"
	ReasonModeMismatch = "priced model is not the same kind of endpoint"
	ReasonNoHosts      = "no host in the price table serves this model"
	ReasonNoTwin       = "no twin declared in hosts.yaml"
)

// Price applies one row to one workload. The bool is false when the row
// cannot honestly price it, and the string says why in the page's own
// vocabulary.
//
// A rate of exactly 0 never prices anything. That is the single most
// important line in this file: upstream tables are full of zeros that
// mean "we didn't record this", and a zero that prices as free reports
// savings of $0.00 forever with no error anywhere.
func (row Row) Price(c Counts) (Quote, bool, string) {
	mode := row.Mode
	if mode == "" {
		mode = ModeChat
	}
	switch mode {
	case ModeRerank:
		// The rerank family is billed per search; its token price is
		// literally 0 upstream, so pricing its tokens would price it free.
		if row.PerQuery <= 0 {
			return Quote{}, false, ReasonZeroRate
		}
		if c.Basis == BasisChat {
			return Quote{}, false, ReasonModeMismatch
		}
		return Quote{Row: row, Cost: float64(c.Req) * row.PerQuery}, true, ""
	case ModeEmbedding:
		if c.Basis == BasisChat {
			return Quote{}, false, ReasonModeMismatch
		}
		if row.In <= 0 {
			return Quote{}, false, ReasonZeroRate
		}
		in := billableInput(c)
		cost := float64(in) * row.In / 1e6
		if row.Out > 0 {
			cost += float64(c.Out) * row.Out / 1e6
		}
		return Quote{Row: row, Cost: cost}, true, ""
	default:
		if c.Basis == BasisEmbed {
			return Quote{}, false, ReasonModeMismatch
		}
		if row.In <= 0 || row.Out <= 0 {
			return Quote{}, false, ReasonZeroRate
		}
		q := Quote{Row: row}
		cached := int64(0)
		if CachedIsBillable(c.Basis) {
			cached = c.InCached
		}
		cacheRate := row.CacheRead
		if cacheRate <= 0 {
			cacheRate = row.In
			if cached > 0 {
				q.NoCacheDiscount = true
			}
		}
		q.Cost = (float64(c.InFresh)*row.In +
			float64(cached)*cacheRate +
			float64(c.Out)*row.Out) / 1e6
		return q, true, ""
	}
}

// billableInput is the prompt figure this basis actually bills for.
func billableInput(c Counts) int64 {
	if CachedIsBillable(c.Basis) {
		return c.InFresh + c.InCached
	}
	return c.InFresh
}

// BillableInput is the exported form, used by the report to show what it
// priced rather than what it counted.
func BillableInput(c Counts) int64 { return billableInput(c) }

// Spread is a workload priced across every qualifying host: the median
// is the headline, min/max are the range the page renders instead of
// fake precision, and N is how many hosts stood behind it.
type Spread struct {
	Median float64
	Min    float64
	Max    float64
	N      int
	// InRateMin/InRateMax bound the per-MTok input rate across the same
	// hosts — the "$0.07–$0.30 /MTok" chip.
	InRateMin float64
	InRateMax float64
	// NoCacheDiscount is true when the MEDIAN host publishes no cache
	// read price, so its cached tokens were billed at the full input
	// rate.
	NoCacheDiscount bool
	// Reason is set when the spread could not be computed.
	Reason string
}

// PriceAcross prices one workload at every row and returns the spread.
// Rows that cannot price the workload are excluded, and a spread with no
// qualifying row carries the reason rather than a zero.
func PriceAcross(rows []Row, c Counts) (Spread, bool) {
	if len(rows) == 0 {
		return Spread{Reason: ReasonNoHosts}, false
	}
	var quotes []Quote
	reason := ""
	for _, r := range rows {
		q, ok, why := r.Price(c)
		if !ok {
			if reason == "" {
				reason = why
			}
			continue
		}
		quotes = append(quotes, q)
	}
	if len(quotes) == 0 {
		if reason == "" {
			reason = ReasonNoRate
		}
		return Spread{Reason: reason}, false
	}
	slices.SortFunc(quotes, func(a, b Quote) int {
		if a.Cost < b.Cost {
			return -1
		}
		if a.Cost > b.Cost {
			return 1
		}
		return strings.Compare(rowID(a.Row), rowID(b.Row))
	})
	sp := Spread{
		N:               len(quotes),
		Min:             quotes[0].Cost,
		Max:             quotes[len(quotes)-1].Cost,
		Median:          median(quotes),
		NoCacheDiscount: medianQuote(quotes).NoCacheDiscount,
	}
	for i, q := range quotes {
		rate := q.Row.In
		if q.Row.Mode == ModeRerank {
			rate = q.Row.PerQuery
		}
		if i == 0 || rate < sp.InRateMin {
			sp.InRateMin = rate
		}
		if i == 0 || rate > sp.InRateMax {
			sp.InRateMax = rate
		}
	}
	return sp, true
}

// median of an even-sized set is the mean of the two middle costs — the
// conventional definition, and it keeps two-host twins from silently
// resolving to whichever host sorted first.
func median(qs []Quote) float64 {
	n := len(qs)
	if n%2 == 1 {
		return qs[n/2].Cost
	}
	return (qs[n/2-1].Cost + qs[n/2].Cost) / 2
}

// medianQuote is the lower-middle quote — the row whose cache policy the
// spread reports.
func medianQuote(qs []Quote) Quote {
	n := len(qs)
	if n%2 == 1 {
		return qs[n/2]
	}
	return qs[n/2-1]
}
