// Package modelprobe is the cell-side throughput probe (fleet-control
// C8): one canned, deterministic request against a model that is ALREADY
// RESIDENT on this box, timed and scored against that model's own recent
// healthy samples. It answers the design's friction pain 2 — llama-server
// can degrade 10-100x while /health stays green.
//
// Cell-side, not fleetd-side, for four reasons (C8 §1). The first is the
// one that would make a fleetd implementation actively harmful: a probe
// issued through the front is a request like any other, and llama-swap's
// contract is JIT-on-request, so probing a stopped model STARTS it —
// exactly the eviction C4's warm policy exists to prevent. Only the box
// holding the model can read /running on localhost immediately before
// issuing and abort instead. The others: only the cell resolves aliases
// to the canonical def name (C7a's finding), a probe through the front
// measures the LAN and the front's queue as much as the model, and C3
// inverted discovery so a cell needs no inbound port.
//
// The cardinal rule, restated because every other rule here serves it:
// A PROBE NEVER LOADS A MODEL. There is no flag that relaxes it.
package modelprobe

import (
	"bytes"
	"context"
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

// Probe kinds. The kind decides which endpoint and which metric; it is
// derived from the def (an embedding-serving def probes as embed) and is
// never guessed from a model name.
const (
	KindChat  = "chat"
	KindEmbed = "embed"
)

// The metrics. They are separate names rather than one "throughput"
// because they are not comparable: decode_tok_s comes from llama.cpp's
// own timings block and excludes queueing, e2e_tok_s is wall time for
// the whole request. Baselines key on the metric, so an engine that
// stops reporting timings starts a fresh baseline instead of reading as
// an instant regression.
const (
	MetricDecode = "decode_tok_s"
	MetricE2E    = "e2e_tok_s"
	MetricEmbed  = "embed_inputs_s"
)

// The canned specs. Deterministic by construction — same prompt, same
// token budget, temperature 0 — because a baseline over varying work
// measures the variation, not the model.
const (
	specChat  = "chat/v1:64out"
	specEmbed = "embed/v1:64in"
)

// probeMaxTokens is the generated-token budget for a chat probe. Big
// enough that decode rate is measured rather than sampled (a 4-token
// probe is mostly first-token latency), small enough that the whole
// budget in C8 §3 stays trivial.
const probeMaxTokens = 64

// embedBatch is the embed probe's input count: the field-proven
// 64-input batch shape from the ops friction log that motivated pain 2.
const embedBatch = 64

// probeTimeout bounds one probe end to end. A model degraded 100x still
// answers inside it (64 tokens at 0.5 tok/s is ~128s... which is why
// this is generous); past it the probe is abandoned and recorded as a
// failed attempt rather than left hanging.
//
// It is applied in Run, not in Start, because Start is not the only
// caller: `vibe model try` (C18) calls Run directly with the command's
// context, and Config.HTTPClient defaults to a client with NO timeout of
// its own — so a wedged llama-server left that path unbounded, holding
// the cell's single probe slot for as long as the socket stayed open.
const probeTimeout = 3 * time.Minute

// runningTimeout bounds the local /running read INSIDE probeTimeout. It
// is short on purpose and the shortness is the point: the residency read
// is the cheapest request in the phase (localhost, no model work), it
// runs while the single-flight lock is held, and it is the one request
// whose failure must NOT be reported as an answer. Letting it inherit
// the whole probe budget would mean a hung llama-swap locks this cell out
// of probing anything for three minutes to learn nothing.
const runningTimeout = 5 * time.Second

// The budget, enforced HERE rather than at the queue: C6 made piggyback
// delivery at-least-once, so a redelivered command must not buy a second
// probe.
const (
	// minGap is the floor between two probes of the same model.
	minGap = 5 * time.Minute
	// dailyCap bounds probes per cell per rolling 24h.
	dailyCap = 96
)

// Scoring thresholds (C8 §4). The band between them is hysteresis: a
// model sitting near the line keeps its previous verdict instead of
// dripping transition events every interval.
const (
	degradedBelow = 0.50
	okAtOrAbove   = 0.65
	// minSamples is the baseline size below which no verdict is possible.
	// A fresh cell must never scream on boot.
	minSamples = 5
	// windowCap bounds the stored samples per key. history.go's shape
	// (small rolling window, whole-file rewrite on record) is right here
	// for its own stated reason: records are minutes-to-hours apart.
	windowCap = 20
)

// Spec is one model's probe configuration, derived from its backend def.
type Spec struct {
	Model string
	Kind  string
	// FlagsSHA256 binds a sample to the serving argv it measured. A def
	// edit starts a fresh baseline rather than reporting a configuration
	// change as a regression.
	FlagsSHA256 string
	// Disabled skips this model entirely (a def that must never be probed:
	// a paid cloud_peer, a model whose warm-up cost dwarfs the signal).
	Disabled bool
}

// Config wires a Prober.
type Config struct {
	// LlamaSwapURL is the LOCAL llama-swap base. Always localhost: this
	// prober measures the cell it runs on, never a peer.
	LlamaSwapURL string
	// StatePath backs the rolling baselines and the budget counters.
	StatePath string
	// ReadOnly loads StatePath and never writes it back. It exists because
	// the file is a whole-file rewrite from in-memory state (history.go's
	// shape), so a SECOND process holding it discards every record the
	// first one made after the second one started. The cell daemon's
	// prober is the owner and the only writer; a short-lived prober beside
	// it (`vibe model try`) reads the incumbent's window to print it and
	// must not spend the daemon's samples or its 96/day budget to do so.
	ReadOnly bool
	// Specs resolves a model id to its probe spec. Nil means every model
	// probes as chat with no fingerprint binding.
	Specs func(model string) Spec
	// HTTPClient is the transport seam; nil gets a client with no timeout
	// of its own. The bound is probeTimeout, applied to the context in Run
	// so that every entry point — Start, and `vibe model try`'s direct Run
	// — gets it whatever client it supplies.
	HTTPClient *http.Client
	// Logger; nil → slog.Default.
	Logger *slog.Logger
	// Now is the clock seam for tests; nil → time.Now.
	Now func() time.Time
}

// sample is one recorded measurement.
type sample struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// baseline is one (model, flags_sha256, metric) key's rolling window
// plus the state the verdict needs across runs.
type baseline struct {
	Model         string     `json:"model"`
	FlagsSHA256   string     `json:"flags_sha256,omitempty"`
	Metric        string     `json:"metric"`
	Samples       []sample   `json:"samples"`
	Verdict       string     `json:"verdict,omitempty"`
	DegradedSince *time.Time `json:"degraded_since,omitempty"`
}

// state is the on-disk file: baselines, last results, and the budget
// ledger, written as one atomic unit so a crash can't leave the counters
// disagreeing with the samples.
type state struct {
	Baselines []baseline                         `json:"baselines"`
	Results   map[string]*fleetapi.AnnounceProbe `json:"results"`
	// Attempts are the timestamps of probes ISSUED (not completed) in the
	// last 24h — the daily cap counts attempts, because an abandoned probe
	// spent the GPU time too.
	Attempts []time.Time `json:"attempts"`
	// LastAttempt is the per-model cooldown clock. It is deliberately NOT
	// the last RESULT's timestamp: a refusal (not resident, cell busy)
	// carries the previous measurement forward, and keying the cooldown on
	// that would let a string of refusals lock a model out of probing for
	// as long as they kept arriving.
	LastAttempt map[string]time.Time `json:"last_attempt,omitempty"`
}

// Prober runs and scores this cell's probes.
type Prober struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger
	now    func() time.Time

	// runMu is the single-flight gate: one probe at a time per cell.
	// TryLock, not Lock — a second command must be REFUSED with a note,
	// never queued behind the first, or a burst of redelivered commands
	// becomes a serial probe train.
	runMu sync.Mutex

	mu          sync.Mutex
	baselines   map[string]*baseline
	results     map[string]*fleetapi.AnnounceProbe
	attempts    []time.Time
	lastAttempt map[string]time.Time
}

// New builds a Prober and loads its persisted baselines. A missing or
// unreadable state file is a fresh box: empty baselines, and the first
// probes report `unknown` until minSamples land.
func New(cfg Config) (*Prober, error) {
	if cfg.LlamaSwapURL == "" {
		return nil, errors.New("modelprobe: llama_swap_url is required")
	}
	p := &Prober{
		cfg:         cfg,
		http:        cfg.HTTPClient,
		logger:      cfg.Logger,
		now:         cfg.Now,
		baselines:   map[string]*baseline{},
		results:     map[string]*fleetapi.AnnounceProbe{},
		lastAttempt: map[string]time.Time{},
	}
	if p.http == nil {
		p.http = &http.Client{}
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.load()
	return p, nil
}

func baselineKey(model, flags, metric string) string {
	return model + "\x00" + flags + "\x00" + metric
}

func (p *Prober) load() {
	if p.cfg.StatePath == "" {
		return
	}
	data, err := os.ReadFile(p.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		p.logger.Warn("probe state unreadable; starting with no baselines", "path", p.cfg.StatePath, "err", err)
		return
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		p.logger.Warn("probe state corrupt; starting with no baselines", "path", p.cfg.StatePath, "err", err)
		return
	}
	for i := range st.Baselines {
		b := st.Baselines[i]
		p.baselines[baselineKey(b.Model, b.FlagsSHA256, b.Metric)] = &b
	}
	for k, v := range st.Results {
		p.results[k] = v
	}
	p.attempts = st.Attempts
	for k, v := range st.LastAttempt {
		p.lastAttempt[k] = v
	}
}

// saveLocked persists via tmp + rename (the repo's discipline). Failures
// are warned, not fatal: the in-memory baseline stays authoritative for
// this process, and losing a baseline costs samples, not correctness.
func (p *Prober) saveLocked() {
	if p.cfg.StatePath == "" || p.cfg.ReadOnly {
		return
	}
	st := state{Results: p.results, Attempts: p.attempts, LastAttempt: p.lastAttempt}
	for _, b := range p.baselines {
		st.Baselines = append(st.Baselines, *b)
	}
	slices.SortFunc(st.Baselines, func(a, b baseline) int {
		return strings.Compare(baselineKey(a.Model, a.FlagsSHA256, a.Metric), baselineKey(b.Model, b.FlagsSHA256, b.Metric))
	})
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		p.logger.Warn("marshal probe state", "err", err)
		return
	}
	dir := filepath.Dir(p.cfg.StatePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.logger.Warn("mkdir probe state dir", "dir", dir, "err", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".model-probe-*")
	if err != nil {
		p.logger.Warn("create probe state tmp", "err", err)
		return
	}
	tmpPath := tmp.Name()
	fail := func(err error, msg string) {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		p.logger.Warn(msg, "err", err)
	}
	if _, err := tmp.Write(data); err != nil {
		fail(err, "write probe state tmp")
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		p.logger.Warn("close probe state tmp", "err", err)
		return
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		p.logger.Warn("chmod probe state tmp", "err", err)
		return
	}
	if err := os.Rename(tmpPath, p.cfg.StatePath); err != nil {
		_ = os.Remove(tmpPath)
		p.logger.Warn("rename probe state", "err", err)
	}
}

// Results returns the latest per-model probe blocks for the announce.
// Copies: the announce loop marshals these on another goroutine.
func (p *Prober) Results() map[string]*fleetapi.AnnounceProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*fleetapi.AnnounceProbe, len(p.results))
	for k, v := range p.results {
		if v == nil {
			continue
		}
		out[k] = fleetapi.CloneProbe(v)
	}
	return out
}

// Start launches one probe in the background and returns immediately.
//
// Background is load-bearing, not laziness: the caller is the announce
// loop's command handler, and the heartbeat is the cell's ONLY evidence
// of life (fleetd's staleness bound is 3 intervals). A probe that runs
// inline holds the next heartbeat for its whole duration — and a
// degraded model, the exact case this exists for, is the slowest probe
// there is. It would mark the cell stale for being slow to prove it was
// slow.
func (p *Prober) Start(model string, rebaseline bool) {
	go func() {
		// No deadline here: probeTimeout lives in Run, so the announce
		// loop's path and `vibe model try`'s path are bounded by the same
		// line rather than by two that can drift apart.
		p.Run(context.Background(), model, rebaseline)
	}()
}

// Run executes one probe synchronously and returns the block it
// recorded. Every refusal path records a block too — a probe that did
// not happen must be visible with its reason, not silently absent.
func (p *Prober) Run(ctx context.Context, model string, rebaseline bool) *fleetapi.AnnounceProbe {
	// The end-to-end bound, taken as the MINIMUM with whatever the caller
	// brought: a caller with a shorter deadline keeps it, a caller with
	// none (or with a client that has no timeout) still gets this one.
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	if !p.runMu.TryLock() {
		// Refused, not queued: a serial probe train is a budget leak.
		return p.note(model, "", "a probe is already running on this cell")
	}
	defer p.runMu.Unlock()

	spec := Spec{Model: model, Kind: KindChat}
	if p.cfg.Specs != nil {
		spec = p.cfg.Specs(model)
		if spec.Model == "" {
			spec.Model = model
		}
		if spec.Kind == "" {
			spec.Kind = KindChat
		}
	}
	if spec.Disabled {
		return p.note(model, spec.Kind, "probing is disabled for this model")
	}

	now := p.now()
	if why, ok := p.budgetRefusal(model, now); !ok {
		return p.note(model, spec.Kind, why)
	}

	// The load rule, enforced at the last possible moment before the
	// request: llama-swap JIT-starts anything it is asked for, so this
	// check is the only thing between a measurement and an eviction.
	resident, err := p.isResident(ctx, model)
	if err != nil {
		return p.note(model, spec.Kind, "cannot read local llama-swap /running: "+err.Error())
	}
	if !resident {
		return p.note(model, spec.Kind,
			"not resident — a probe must not load a model; warm it first, then probe")
	}

	p.markAttempt(model, now)
	res, err := p.measure(ctx, spec)
	if err != nil {
		return p.note(model, spec.Kind, "probe request failed: "+err.Error())
	}
	return p.record(spec, res, rebaseline)
}

// measurement is one completed probe's raw numbers.
type measurement struct {
	Metric string
	Value  float64
	TTFTMS int64
}

// budgetRefusal applies the cooldown and the daily cap. Both live on the
// cell because the piggyback queue is at-least-once: fleetd asking twice
// must cost one probe.
func (p *Prober) budgetRefusal(model string, now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.lastAttempt[model]; ok && !last.IsZero() {
		if gap := now.Sub(last); gap < minGap {
			return fmt.Sprintf("cooldown: last probe %s ago (minimum gap %s)", gap.Round(time.Second), minGap), false
		}
	}
	if len(p.attemptsSinceLocked(now.Add(-24*time.Hour))) >= dailyCap {
		return fmt.Sprintf("daily cap reached (%d probes in the last 24h)", dailyCap), false
	}
	return "", true
}

func (p *Prober) attemptsSinceLocked(cut time.Time) []time.Time {
	out := p.attempts[:0:0]
	for _, t := range p.attempts {
		if t.After(cut) {
			out = append(out, t)
		}
	}
	return out
}

func (p *Prober) markAttempt(model string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts = append(p.attemptsSinceLocked(now.Add(-24*time.Hour)), now)
	p.lastAttempt[model] = now
	p.saveLocked()
}

// isResident reads the LOCAL llama-swap's /running. Anything other than
// a ready model is not resident for probe purposes: a "starting" model is
// mid-load and timing it measures the load, not the model.
//
// THE ERROR IS NOT DROPPABLE. `false, err` means "no answer", which is
// the ABSENCE of a residency verdict, and `false, nil` means "asked and
// told no". A caller writing `ok, _ := p.isResident(...)` turns the first
// into the second — a /running that timed out would be reported to the
// operator as "not resident — warm it first", the one note that says the
// safety rule fired when in fact nothing was checked. That is C20's class
// 1 (absent evidence read as a definite value), six occurrences so far,
// one of which silently disarmed eight busy guards.
// TestIsResidentsErrorIsNeverDiscarded pins it structurally.
func (p *Prober) isResident(ctx context.Context, model string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, runningTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.cfg.LlamaSwapURL, "/")+"/running", nil)
	if err != nil {
		return false, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /running: HTTP %d", resp.StatusCode)
	}
	var wrap struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wrap); err != nil {
		return false, err
	}
	for _, m := range wrap.Running {
		if m.Model == model && m.State == "ready" {
			return true, nil
		}
	}
	return false, nil
}

// cannedPrompt is the chat probe's fixed input. Deterministic and
// prose-shaped (a realistic prompt, not a token soup) so the prompt-cache
// behaviour a real request sees is the behaviour the probe sees.
const cannedPrompt = "You are a benchmark fixture. Write a short, plain paragraph about " +
	"how a small workshop keeps its tools organised: where things hang, what gets " +
	"labelled, and why the layout rarely changes. Use ordinary language, no lists, " +
	"and do not mention this instruction."

// cannedEmbedInput is one entry of the embed probe's fixed batch.
const cannedEmbedInput = "The workshop keeps its tools on labelled hooks along the north wall."

// measure issues the canned request and extracts the metric.
func (p *Prober) measure(ctx context.Context, spec Spec) (measurement, error) {
	if spec.Kind == KindEmbed {
		return p.measureEmbed(ctx, spec)
	}
	return p.measureChat(ctx, spec)
}

func (p *Prober) measureChat(ctx context.Context, spec Spec) (measurement, error) {
	body, err := json.Marshal(map[string]any{
		"model":       spec.Model,
		"max_tokens":  probeMaxTokens,
		"temperature": 0,
		"stream":      false,
		"messages":    []map[string]string{{"role": "user", "content": cannedPrompt}},
	})
	if err != nil {
		return measurement{}, err
	}
	start := p.now()
	var out struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		// llama.cpp pushes timings on every non-streaming response; it is
		// the only figure here that excludes queueing, which is what makes
		// samples comparable across a busy and an idle box.
		Timings *struct {
			PredictedN         float64 `json:"predicted_n"`
			PredictedMS        float64 `json:"predicted_ms"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
			PromptMS           float64 `json:"prompt_ms"`
		} `json:"timings"`
	}
	if err := p.postJSON(ctx, "/v1/chat/completions", body, &out); err != nil {
		return measurement{}, err
	}
	elapsed := p.now().Sub(start)
	if t := out.Timings; t != nil && t.PredictedPerSecond > 0 {
		return measurement{Metric: MetricDecode, Value: t.PredictedPerSecond, TTFTMS: int64(t.PromptMS)}, nil
	}
	if t := out.Timings; t != nil && t.PredictedMS > 0 && t.PredictedN > 0 {
		return measurement{Metric: MetricDecode, Value: t.PredictedN / (t.PredictedMS / 1000), TTFTMS: int64(t.PromptMS)}, nil
	}
	// The fallback (mlx, or any engine with no timings block) measures the
	// whole request, queueing included — recorded under its OWN metric name
	// so it can never be compared against a decode-rate baseline.
	if out.Usage.CompletionTokens <= 0 || elapsed <= 0 {
		return measurement{}, errors.New("response carried neither timings nor usage tokens")
	}
	return measurement{Metric: MetricE2E, Value: float64(out.Usage.CompletionTokens) / elapsed.Seconds()}, nil
}

func (p *Prober) measureEmbed(ctx context.Context, spec Spec) (measurement, error) {
	inputs := make([]string, embedBatch)
	for i := range inputs {
		// Distinct inputs: an identical batch is a cache-hit measurement,
		// not a throughput one.
		inputs[i] = fmt.Sprintf("%s (%d)", cannedEmbedInput, i)
	}
	body, err := json.Marshal(map[string]any{"model": spec.Model, "input": inputs})
	if err != nil {
		return measurement{}, err
	}
	start := p.now()
	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := p.postJSON(ctx, "/v1/embeddings", body, &out); err != nil {
		return measurement{}, err
	}
	elapsed := p.now().Sub(start)
	if len(out.Data) == 0 || elapsed <= 0 {
		return measurement{}, errors.New("embedding response carried no vectors")
	}
	return measurement{Metric: MetricEmbed, Value: float64(len(out.Data)) / elapsed.Seconds()}, nil
}

func (p *Prober) postJSON(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.LlamaSwapURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// note records a probe that did not produce a number, with the reason.
// It deliberately keeps the previous verdict and baseline visible: a
// refusal is not evidence about the model's health, so it must not erase
// what was known.
func (p *Prober) note(model, kind, why string) *fleetapi.AnnounceProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := &fleetapi.AnnounceProbe{Kind: kind, At: p.now().UTC(), Verdict: fleetapi.VerdictUnknown, Note: why}
	if prev := p.results[model]; prev != nil {
		// Carry the last real measurement forward so the status reads
		// "last measured 41.7 tok/s; this attempt skipped (cooldown)"
		// rather than erasing what was known.
		carried := *prev
		carried.Note = why
		if carried.Kind == "" {
			carried.Kind = kind
		}
		res = &carried
	}
	p.results[model] = res
	p.saveLocked()
	return fleetapi.CloneProbe(res)
}

// record scores a completed measurement against the baseline and stores
// both. The scoring rules are C8 §4:
//
//   - under minSamples the verdict is `unknown` — never `degraded`
//     without a baseline, or a fresh cell screams on boot;
//   - the ratio is against the median of the stored samples, which
//     EXCLUDE the current one;
//   - the band between degradedBelow and okAtOrAbove keeps the previous
//     verdict (hysteresis);
//   - a degraded sample does NOT enter the window. Letting it in means a
//     genuine 2x regression washes out of a 20-sample median in about
//     eleven probes and the status goes quietly green while the box is
//     still slow. The cost — a legitimate permanent slowdown stays
//     flagged — is paid by the explicit rebaseline argument.
func (p *Prober) record(spec Spec, m measurement, rebaseline bool) *fleetapi.AnnounceProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now().UTC()
	key := baselineKey(spec.Model, spec.FlagsSHA256, m.Metric)
	b := p.baselines[key]
	if b == nil {
		b = &baseline{Model: spec.Model, FlagsSHA256: spec.FlagsSHA256, Metric: m.Metric}
		p.baselines[key] = b
	}
	if rebaseline {
		b.Samples = nil
		b.Verdict = ""
		b.DegradedSince = nil
	}

	// The count that BACKED this verdict, captured before the new sample
	// lands. Reporting the post-append length announced "samples: 5" beside
	// "verdict: unknown" on the fifth probe — the threshold is minSamples=5,
	// so that reads as a broken scorer rather than as "four samples so far".
	backing := len(b.Samples)
	var backingAt *time.Time
	if backing > 0 {
		t := b.Samples[backing-1].At
		backingAt = &t
	}
	p50, ok := median(b.Samples)
	verdict := fleetapi.VerdictUnknown
	ratio := 0.0
	switch {
	case !ok || len(b.Samples) < minSamples || p50 <= 0:
		// Baselining: no verdict is possible yet, and saying so is the
		// point.
	default:
		ratio = m.Value / p50
		switch {
		case ratio < degradedBelow:
			verdict = fleetapi.VerdictDegraded
		case ratio >= okAtOrAbove:
			verdict = fleetapi.VerdictOK
		default:
			verdict = b.Verdict
			if verdict == "" {
				verdict = fleetapi.VerdictOK
			}
		}
	}

	if verdict != fleetapi.VerdictDegraded {
		b.Samples = append(b.Samples, sample{At: now, Value: m.Value})
		if len(b.Samples) > windowCap {
			b.Samples = b.Samples[len(b.Samples)-windowCap:]
		}
	}
	if verdict == fleetapi.VerdictDegraded && b.Verdict != fleetapi.VerdictDegraded {
		t := now
		b.DegradedSince = &t
	}
	if verdict != fleetapi.VerdictDegraded {
		b.DegradedSince = nil
	}
	b.Verdict = verdict

	res := &fleetapi.AnnounceProbe{
		Kind:        spec.Kind,
		Spec:        specID(spec.Kind),
		At:          now,
		Metric:      m.Metric,
		Value:       m.Value,
		BaselineP50: p50,
		Samples:     backing,
		Ratio:       ratio,
		Verdict:     verdict,
		TTFTMS:      m.TTFTMS,
		FlagsSHA256: spec.FlagsSHA256,
	}
	if b.DegradedSince != nil {
		t := *b.DegradedSince
		res.DegradedSince = &t
	}
	// The newest sample BEHIND BaselineP50, same window as Samples: taking
	// it after the append made a fresh cell's first probe report a baseline
	// age beside a baseline of zero.
	res.BaselineAt = backingAt
	p.results[spec.Model] = res
	p.saveLocked()
	return fleetapi.CloneProbe(res)
}

func specID(kind string) string {
	if kind == KindEmbed {
		return specEmbed
	}
	return specChat
}

// median of the stored sample values (the baseline). Sorting a copy:
// the stored order is chronological and the window trim depends on it.
func median(ss []sample) (float64, bool) {
	if len(ss) == 0 {
		return 0, false
	}
	vals := make([]float64, len(ss))
	for i, s := range ss {
		vals[i] = s.Value
	}
	slices.Sort(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2], true
	}
	return (vals[n/2-1] + vals[n/2]) / 2, true
}
