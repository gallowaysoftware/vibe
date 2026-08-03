package prices

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// The vendoring path (fleet-control C7b §2). Run by a human on an
// internet-connected box via `vibe fleet prices vendor`; CI never has
// network and never runs it. Stdlib only.
//
// Two upstreams, both MIT, each for what the other lacks:
//
//   - models.dev is the only source with a first-class open_weights flag
//     AND many hosting providers per identical model, which is exactly
//     what a twin median needs.
//   - LiteLLM carries the two fields models.dev lacks: mode
//     (chat/embedding/rerank) and input_cost_per_query, without which the
//     rerank family prices at its token rate — literally 0.
//
// Not OpenRouter (its ToS bars scraping and redistribution) and not
// Artificial Analysis (key-gated, redistribution by permission).

// DefaultModelsDevURL and DefaultLiteLLMURL are the upstream artifacts.
const (
	DefaultModelsDevURL = "https://models.dev/api.json"
	DefaultLiteLLMURL   = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	// The commit endpoints are the provenance half: a vendored price
	// table with no upstream commit is a number nobody can audit.
	DefaultModelsDevCommitURL = "https://api.github.com/repositories/996360053/commits?per_page=1"
	DefaultLiteLLMCommitURL   = "https://api.github.com/repos/BerriAI/litellm/commits?per_page=1&path=model_prices_and_context_window.json"
)

// excludedProviders never enter the vendored table regardless of which
// upstream carries them. OpenRouter's terms bar redistribution of its
// price list, and a row sourced from models.dev's mirror of it is the
// same content.
var excludedProviders = map[string]bool{"openrouter": true}

// tableNotice rides in the artifact. It exists so anyone who opens the
// file — or greps it out of a bug report — knows what it is and is not.
const tableNotice = "Vendored model price table for vibe's fleet savings screen. " +
	"Prices are USD per million tokens (per query for the rerank family), normalized from the sources listed per snapshot. " +
	"A price of 0 means UNKNOWN, never free. Regenerate with `vibe fleet prices vendor`."

// exampleNotice marks a synthetic table.
const exampleNotice = "EXAMPLE DATA — SYNTHETIC PRICES, NOT REAL RATES. " +
	"This table exists so the pricing arithmetic has something to chew on offline. " +
	"Run `vibe fleet prices vendor` on an internet-connected box to replace it with real vendored prices."

// VendorConfig drives one vendoring run. Every URL is a field so the
// tests can point the whole pipeline at httptest servers — the tool is
// unit-tested with no network, which is the only way it could be tested
// at all.
type VendorConfig struct {
	ModelsDevURL       string
	LiteLLMURL         string
	ModelsDevCommitURL string
	LiteLLMCommitURL   string
	// EffectiveFrom dates the new snapshot (default: today, UTC).
	EffectiveFrom string
	// MaxDisagreements is how many cross-source price conflicts past
	// Ratio the run tolerates before failing. Default 0: any conflict
	// fails the run loudly, with every offender printed, because the
	// failure this check exists for is a units error that trips
	// everything at once. A human who has read the list re-runs with a
	// reviewed count, and the count is recorded in the artifact.
	MaxDisagreements int
	// Ratio is the disagreement bound (default 2: a 2x price difference).
	Ratio  float64
	Client *http.Client
	Now    func() time.Time
}

// VendorReport is the printed summary of one run.
type VendorReport struct {
	EffectiveFrom string
	Base          bool
	Rows          int
	Added         []string
	Removed       []string
	Changed       []string
	Unchanged     int
	Sources       []Source
	Disagreements []Disagreement
	Excluded      map[string]int
	Warnings      []string
}

// Vendor fetches both upstreams and returns the table with a new dated
// snapshot appended. prev may be nil (first vendoring: the run produces
// a base snapshot).
func (cfg VendorConfig) Vendor(ctx context.Context, prev *Table) (*Table, *VendorReport, error) {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Ratio <= 0 {
		cfg.Ratio = 2
	}
	if cfg.ModelsDevURL == "" {
		cfg.ModelsDevURL = DefaultModelsDevURL
	}
	if cfg.LiteLLMURL == "" {
		cfg.LiteLLMURL = DefaultLiteLLMURL
	}
	day := cfg.EffectiveFrom
	if day == "" {
		day = cfg.Now().UTC().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return nil, nil, fmt.Errorf("effective_from %q is not YYYY-MM-DD", day)
	}

	rep := &VendorReport{EffectiveFrom: day, Excluded: map[string]int{}}

	var mdRaw map[string]modelsDevProvider
	if err := fetchJSON(ctx, cfg.Client, cfg.ModelsDevURL, &mdRaw); err != nil {
		return nil, nil, fmt.Errorf("fetch models.dev: %w", err)
	}
	var llRaw map[string]liteLLMEntry
	if err := fetchJSON(ctx, cfg.Client, cfg.LiteLLMURL, &llRaw); err != nil {
		return nil, nil, fmt.Errorf("fetch litellm: %w", err)
	}

	lite := normalizeLiteLLM(llRaw)
	rows, excluded := normalizeModelsDev(mdRaw, lite)
	rep.Excluded = excluded
	rows = append(rows, liteLLMOnlyRows(lite, rows)...)

	conflicts := crossCheck(rows, lite, cfg.Ratio)
	if len(conflicts) > cfg.MaxDisagreements {
		return nil, nil, &DisagreementError{Ratio: cfg.Ratio, Allowed: cfg.MaxDisagreements, Conflicts: conflicts}
	}
	rows = dropConflicting(rows, conflicts)
	rep.Disagreements = conflicts

	slices.SortFunc(rows, func(a, b Row) int { return strings.Compare(rowID(a), rowID(b)) })
	rep.Rows = len(rows)

	sources := []Source{
		{Name: "models.dev", URL: cfg.ModelsDevURL, Licence: "MIT",
			Commit: cfg.commitSHA(ctx, cfg.ModelsDevCommitURL, DefaultModelsDevCommitURL, rep), Fetched: day},
		{Name: "litellm", URL: cfg.LiteLLMURL, Licence: "MIT",
			Commit: cfg.commitSHA(ctx, cfg.LiteLLMCommitURL, DefaultLiteLLMCommitURL, rep), Fetched: day},
	}
	rep.Sources = sources

	out := &Table{
		Schema:   SchemaVersion,
		Notice:   tableNotice,
		Licences: []string{"models.dev: MIT (https://github.com/sst/models.dev)", "LiteLLM: MIT (https://github.com/BerriAI/litellm)"},
	}
	if prev != nil {
		out.Snapshots = slices.Clone(prev.Snapshots)
		if prev.Notice != "" && prev.Example {
			// Replacing an example table with real prices drops the
			// example marker AND its history: synthetic rows must never
			// silently price a real day.
			out.Snapshots = nil
		}
	}
	last := lastSnapshot(out)
	if last == nil {
		out.Snapshots = []Snapshot{{EffectiveFrom: day, Base: true, Rows: rows, Sources: sources, Disagreements: conflicts}}
		rep.Base = true
		for _, r := range rows {
			rep.Added = append(rep.Added, rowID(r))
		}
		sort.Strings(rep.Added)
		return out, rep, nil
	}
	if day < last.EffectiveFrom {
		return nil, nil, fmt.Errorf("effective_from %s is older than the newest snapshot (%s); snapshots are append-only", day, last.EffectiveFrom)
	}
	lastDay := last.EffectiveFrom

	prevRows := resolveRows(out.Snapshots)
	overlay := Snapshot{EffectiveFrom: day, Sources: sources, Disagreements: conflicts}
	now := map[string]Row{}
	for _, r := range rows {
		now[rowID(r)] = r
	}
	for id, r := range now {
		old, had := prevRows[id]
		switch {
		case !had:
			overlay.Rows = append(overlay.Rows, r)
			rep.Added = append(rep.Added, id)
		case !sameRow(old, r):
			overlay.Rows = append(overlay.Rows, r)
			rep.Changed = append(rep.Changed, fmt.Sprintf("%s: in %g→%g out %g→%g", id, old.In, r.In, old.Out, r.Out))
		default:
			rep.Unchanged++
		}
	}
	for id := range prevRows {
		if _, ok := now[id]; !ok {
			overlay.Removed = append(overlay.Removed, id)
			rep.Removed = append(rep.Removed, id)
		}
	}
	slices.SortFunc(overlay.Rows, func(a, b Row) int { return strings.Compare(rowID(a), rowID(b)) })
	sort.Strings(overlay.Removed)
	sort.Strings(rep.Added)
	sort.Strings(rep.Removed)
	sort.Strings(rep.Changed)

	if len(overlay.Rows) == 0 && len(overlay.Removed) == 0 && day == lastDay {
		// Re-running on the same day with no upstream change must not
		// append an empty snapshot; the artifact stays byte-identical.
		return out, rep, nil
	}
	out.Snapshots = append(out.Snapshots, overlay)
	return out, rep, nil
}

// DisagreementError is the loud failure: the run refuses to vendor a
// table whose two sources disagree past the ratio bound.
type DisagreementError struct {
	Ratio     float64
	Allowed   int
	Conflicts []Disagreement
}

func (e *DisagreementError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d cross-source price disagreements past %gx (allowed %d):\n",
		len(e.Conflicts), e.Ratio, e.Allowed)
	for i, c := range e.Conflicts {
		if i == 25 {
			fmt.Fprintf(&b, "  … and %d more\n", len(e.Conflicts)-i)
			break
		}
		fmt.Fprintf(&b, "  %s %s %s: models.dev %g vs litellm %g\n", c.Provider, c.Model, c.Field, c.A, c.B)
	}
	b.WriteString("review the list; conflicting rows are DROPPED from the table, and " +
		"--max-disagreements <n> records the reviewed count in the artifact")
	return b.String()
}

func lastSnapshot(t *Table) *Snapshot {
	if len(t.Snapshots) == 0 {
		return nil
	}
	return &t.Snapshots[len(t.Snapshots)-1]
}

func resolveRows(snaps []Snapshot) map[string]Row {
	rows := map[string]Row{}
	for _, s := range snaps {
		if s.Base {
			rows = map[string]Row{}
		}
		for _, r := range s.Rows {
			rows[rowID(r)] = r
		}
		for _, id := range s.Removed {
			delete(rows, id)
		}
	}
	return rows
}

func sameRow(a, b Row) bool {
	return a.In == b.In && a.Out == b.Out && a.CacheRead == b.CacheRead && a.PerQuery == b.PerQuery &&
		a.Open == b.Open && a.Mode == b.Mode && a.Deprecated == b.Deprecated && a.Key == b.Key
}

// ─── upstream shapes ────────────────────────────────────────────────────────

type modelsDevProvider struct {
	ID     string                  `json:"id"`
	Models map[string]modelsDevRow `json:"models"`
}

type modelsDevRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OpenWeights bool   `json:"open_weights"`
	Cost        struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		CacheRead float64 `json:"cache_read"`
	} `json:"cost"`
}

type liteLLMEntry struct {
	Provider          string  `json:"litellm_provider"`
	Mode              string  `json:"mode"`
	InputPerToken     float64 `json:"input_cost_per_token"`
	OutputPerToken    float64 `json:"output_cost_per_token"`
	CacheReadPerToken float64 `json:"cache_read_input_token_cost"`
	InputPerQuery     float64 `json:"input_cost_per_query"`
	DeprecationDate   string  `json:"deprecation_date"`
}

// liteRow is one normalized LiteLLM entry.
type liteRow struct {
	Provider   string
	Model      string
	Key        string
	Mode       string
	In         float64
	Out        float64
	CacheRead  float64
	PerQuery   float64
	Deprecated bool
}

func normalizeLiteLLM(raw map[string]liteLLMEntry) map[string][]liteRow {
	out := map[string][]liteRow{}
	for id, e := range raw {
		if id == "sample_spec" {
			continue
		}
		key := Normalize(id)
		if key == "" {
			continue
		}
		r := liteRow{
			Provider:   Normalize(e.Provider),
			Model:      id,
			Key:        key,
			Mode:       canonicalMode(e.Mode),
			In:         perMTok(e.InputPerToken),
			Out:        perMTok(e.OutputPerToken),
			CacheRead:  perMTok(e.CacheReadPerToken),
			PerQuery:   round6(e.InputPerQuery),
			Deprecated: e.DeprecationDate != "",
		}
		out[key] = append(out[key], r)
	}
	// Determinism is a property of the ARTIFACT, not a nicety: two hosts
	// can carry the same normalized key (bedrock's per-region embedding
	// ids), and map order would otherwise decide which one lands in the
	// table — a diff that changes every run means nobody reads diffs.
	for k := range out {
		slices.SortFunc(out[k], func(a, b liteRow) int {
			if c := strings.Compare(a.Provider, b.Provider); c != 0 {
				return c
			}
			return strings.Compare(a.Model, b.Model)
		})
	}
	return out
}

// canonicalMode maps LiteLLM's mode vocabulary onto the three this table
// prices. Anything else (image, audio, moderation) is left alone and
// simply never matches a fleet workload.
func canonicalMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", "chat", "completion", "responses":
		return ModeChat
	case "embedding":
		return ModeEmbedding
	case "rerank":
		return ModeRerank
	default:
		return strings.ToLower(strings.TrimSpace(m))
	}
}

func normalizeModelsDev(raw map[string]modelsDevProvider, lite map[string][]liteRow) ([]Row, map[string]int) {
	var out []Row
	excluded := map[string]int{}
	for pid, p := range raw {
		if excludedProviders[pid] || excludedProviders[Normalize(pid)] {
			excluded[pid] += len(p.Models)
			continue
		}
		for mid, m := range p.Models {
			key := Normalize(m.Name)
			if key == "" {
				key = Normalize(mid)
			}
			row := Row{
				Provider:  pid,
				Model:     mid,
				Key:       key,
				In:        round6(m.Cost.Input),
				Out:       round6(m.Cost.Output),
				CacheRead: round6(m.Cost.CacheRead),
				Open:      m.OpenWeights,
			}
			enrich(&row, lite)
			if !priceable(row) {
				continue
			}
			out = append(out, row)
		}
	}
	return out, excluded
}

// enrich fills the two fields models.dev does not carry, from any
// LiteLLM entry for the same model key. Mode and per-query are
// properties of the ENDPOINT, not of one host's price sheet, so a match
// on any provider is informative; the price itself is never taken from
// LiteLLM here (that is what the cross-check compares against).
func enrich(row *Row, lite map[string][]liteRow) {
	for _, cand := range liteCandidates(row, lite) {
		if cand.Mode != "" && cand.Mode != ModeChat {
			row.Mode = cand.Mode
		}
		if row.PerQuery == 0 && cand.PerQuery > 0 {
			row.PerQuery = cand.PerQuery
		}
		if cand.Deprecated && sameProvider(row.Provider, cand.Provider) {
			row.Deprecated = true
		}
	}
}

func sameProvider(a, b string) bool { return Normalize(a) == Normalize(b) }

// rowKeys are the join keys a row answers to: the normalized NAME and
// the normalized model id. Both are needed and neither is enough alone —
// models.dev keys a row on its display name ("Acme Chat 30B") while
// LiteLLM keys the same model on its api id ("acme/chat-30b"), and
// matching on one of them silently skips half the enrichment AND half
// the cross-check.
func rowKeys(r Row) []string {
	keys := []string{r.Key}
	if alt := Normalize(r.Model); alt != "" && alt != r.Key {
		keys = append(keys, alt)
	}
	return keys
}

func liteCandidates(r *Row, lite map[string][]liteRow) []liteRow {
	var out []liteRow
	seen := map[string]bool{}
	for _, k := range rowKeys(*r) {
		for _, c := range lite[k] {
			id := c.Provider + "\x00" + c.Model
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, c)
		}
	}
	return out
}

// liteLLMOnlyRows adds the embedding and rerank rows models.dev does not
// carry at all — its corpus is chat-shaped, and a fleet that runs an
// embedder would otherwise have no rate to price it against under any
// configuration. Chat rows are NOT taken from LiteLLM: it has one price
// per model, which cannot stand in for a median across hosts.
func liteLLMOnlyRows(lite map[string][]liteRow, have []Row) []Row {
	seen := map[string]bool{}
	for _, r := range have {
		for _, k := range rowKeys(r) {
			seen[Normalize(r.Provider)+"\x00"+k] = true
		}
	}
	var out []Row
	for _, key := range slices.Sorted(maps.Keys(lite)) {
		for _, c := range lite[key] {
			if c.Mode != ModeEmbedding && c.Mode != ModeRerank {
				continue
			}
			if excludedProviders[c.Provider] {
				continue
			}
			if seen[c.Provider+"\x00"+c.Key] {
				continue
			}
			row := Row{
				Provider:   c.Provider,
				Model:      c.Model,
				Key:        c.Key,
				In:         c.In,
				Out:        c.Out,
				CacheRead:  c.CacheRead,
				PerQuery:   c.PerQuery,
				Mode:       c.Mode,
				Deprecated: c.Deprecated,
			}
			if !priceable(row) {
				continue
			}
			seen[c.Provider+"\x00"+c.Key] = true
			out = append(out, row)
		}
	}
	return out
}

// priceable drops rows that carry no usable rate at all. A row of zeros
// is not a free model, it is a model whose price nobody recorded, and
// keeping it would give the median a phantom $0 host.
func priceable(r Row) bool {
	switch r.Mode {
	case ModeRerank:
		return r.PerQuery > 0
	case ModeEmbedding:
		return r.In > 0
	default:
		return r.In > 0 && r.Out > 0
	}
}

// crossCheck compares each models.dev row against a LiteLLM entry for
// the SAME provider and model. Comparing across providers would flag
// every open-weight model in the table, because a 5x spread between
// hosts is the normal state of the market and is exactly what the twin
// median is for.
func crossCheck(rows []Row, lite map[string][]liteRow, ratio float64) []Disagreement {
	var out []Disagreement
	// One row can face several LiteLLM entries that normalize to the same
	// key (bedrock's per-region ids). Reporting the same conflict once per
	// candidate would inflate the count a human has to review and make
	// --max-disagreements depend on upstream's regional spelling.
	seen := map[string]bool{}
	for _, r := range rows {
		for _, c := range liteCandidates(&r, lite) {
			if !sameProvider(r.Provider, c.Provider) {
				continue
			}
			for _, cmp := range []struct {
				field string
				a, b  float64
			}{{"input", r.In, c.In}, {"output", r.Out, c.Out}} {
				if cmp.a <= 0 || cmp.b <= 0 {
					continue
				}
				hi, lo := math.Max(cmp.a, cmp.b), math.Min(cmp.a, cmp.b)
				if hi/lo <= ratio {
					continue
				}
				id := rowID(r) + "\x00" + cmp.field
				if seen[id] {
					continue
				}
				seen[id] = true
				out = append(out, Disagreement{Provider: r.Provider, Model: r.Model, Field: cmp.field, A: cmp.a, B: cmp.b})
			}
		}
	}
	slices.SortFunc(out, func(a, b Disagreement) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		if c := strings.Compare(a.Model, b.Model); c != 0 {
			return c
		}
		return strings.Compare(a.Field, b.Field)
	})
	return out
}

// dropConflicting removes every row the cross-check flagged. A conflict
// means the two sources do not agree on what this host charges, and an
// unknown price must render "unpriced" — picking a side would be the
// table inventing an answer.
func dropConflicting(rows []Row, conflicts []Disagreement) []Row {
	if len(conflicts) == 0 {
		return rows
	}
	bad := map[string]bool{}
	for _, c := range conflicts {
		bad[c.Provider+"\x00"+c.Model] = true
	}
	out := rows[:0]
	for _, r := range rows {
		if bad[rowID(r)] {
			continue
		}
		out = append(out, r)
	}
	return out
}

func perMTok(perToken float64) float64 { return round6(perToken * 1e6) }

// round6 kills the float noise ×1e6 introduces (0.30000000000000004) so
// the artifact is stable across runs and its diffs mean something.
func round6(v float64) float64 {
	if v == 0 {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}

func fetchJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// commitSHA resolves an upstream's HEAD commit. A failure is a WARNING,
// not a failed run: losing provenance on one source is worse than
// nothing, but refusing to vendor at all because GitHub rate-limited an
// unauthenticated request would be worse still. The report says which
// source lost its SHA.
func (cfg VendorConfig) commitSHA(ctx context.Context, url, fallback string, rep *VendorReport) string {
	if url == "" {
		url = fallback
	}
	if url == "-" {
		return ""
	}
	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := fetchJSON(ctx, cfg.Client, url, &commits); err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("commit sha unavailable for %s: %v", url, err))
		return ""
	}
	if len(commits) == 0 {
		rep.Warnings = append(rep.Warnings, "commit sha lookup returned no commits for "+url)
		return ""
	}
	return commits[0].SHA
}

// Encode writes the artifact with one ROW per line. A 5,000-row table
// marshalled as one line makes every future diff unreadable, and
// MarshalIndent explodes it to 60,000. This is the only reason the
// encoder is hand-written.
func Encode(t *Table) ([]byte, error) {
	var b bytes.Buffer
	head := struct {
		Schema   int      `json:"schema"`
		Example  bool     `json:"example_data,omitempty"`
		Notice   string   `json:"notice"`
		Licences []string `json:"licences,omitempty"`
	}{t.Schema, t.Example, t.Notice, t.Licences}
	data, err := json.MarshalIndent(head, "", "  ")
	if err != nil {
		return nil, err
	}
	// Splice the snapshots into the head object by hand so the rows can
	// be line-per-row.
	b.Write(bytes.TrimSpace(bytes.TrimSuffix(bytes.TrimSpace(data), []byte("}"))))
	b.WriteString(",\n  \"snapshots\": [\n")
	for i, s := range t.Snapshots {
		if i > 0 {
			b.WriteString(",\n")
		}
		b.WriteString("    {\n")
		fmt.Fprintf(&b, "      %q: %q", "effective_from", s.EffectiveFrom)
		if s.Base {
			b.WriteString(",\n      \"base\": true")
		}
		if err := writeJSONField(&b, "sources", s.Sources); err != nil {
			return nil, err
		}
		if err := writeJSONField(&b, "disagreements", s.Disagreements); err != nil {
			return nil, err
		}
		if len(s.Removed) > 0 {
			if err := writeJSONField(&b, "removed", s.Removed); err != nil {
				return nil, err
			}
		}
		b.WriteString(",\n      \"rows\": [\n")
		for j, r := range s.Rows {
			line, err := json.Marshal(r)
			if err != nil {
				return nil, err
			}
			b.WriteString("        ")
			b.Write(line)
			if j < len(s.Rows)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString("      ]\n    }")
	}
	b.WriteString("\n  ]\n}\n")
	return b.Bytes(), nil
}

func writeJSONField(b *bytes.Buffer, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if string(data) == "null" || string(data) == "[]" {
		return nil
	}
	fmt.Fprintf(b, ",\n      %q: %s", name, data)
	return nil
}
