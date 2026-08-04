package modelprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// swap is a scriptable stand-in for a cell's local llama-swap.
type swap struct {
	mu sync.Mutex
	// running is the /running answer: model → state.
	running map[string]string
	// tokPerSec is what the chat response's timings block reports.
	tokPerSec float64
	// noTimings drops the timings block (the mlx shape).
	noTimings bool
	// chatCalls counts requests that reached a model endpoint — the
	// number gate 1 asserts is ZERO for a non-resident model.
	chatCalls  atomic.Int64
	embedCalls atomic.Int64
	// block, when non-nil, holds the chat handler until it is closed.
	block chan struct{}
}

func newSwap(t *testing.T, running map[string]string) (*swap, *httptest.Server) {
	t.Helper()
	s := &swap{running: running, tokPerSec: 40}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		type row struct {
			Model string `json:"model"`
			State string `json:"state"`
		}
		var out struct {
			Running []row `json:"running"`
		}
		for m, st := range s.running {
			out.Running = append(out.Running, row{Model: m, State: st})
		}
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.chatCalls.Add(1)
		if s.block != nil {
			<-s.block
		}
		s.mu.Lock()
		tps, noT := s.tokPerSec, s.noTimings
		s.mu.Unlock()
		body := map[string]any{"usage": map[string]any{"completion_tokens": probeMaxTokens}}
		if !noT {
			body["timings"] = map[string]any{
				"predicted_n":          probeMaxTokens,
				"predicted_ms":         float64(probeMaxTokens) / tps * 1000,
				"predicted_per_second": tps,
				"prompt_ms":            120,
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		s.embedCalls.Add(1)
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		data := make([]map[string]any, len(in.Input))
		for i := range data {
			data[i] = map[string]any{"index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *swap) setRunning(model, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[model] = state
}

func (s *swap) setRate(tps float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokPerSec = tps
}

// clock is a manual clock so cooldown/cap tests need no sleeps and the
// e2e fallback has a measurable elapsed time.
type clock struct {
	mu sync.Mutex
	t  time.Time
	// step advances the clock on every read, which is what gives the
	// wall-clock fallback a non-zero duration.
	step time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newProber(t *testing.T, srv *httptest.Server, specs map[string]Spec, clk *clock) *Prober {
	t.Helper()
	p, err := New(Config{
		LlamaSwapURL: srv.URL,
		StatePath:    filepath.Join(t.TempDir(), "model-probe.json"),
		HTTPClient:   srv.Client(),
		Now:          clk.now,
		Specs: func(model string) Spec {
			if s, ok := specs[model]; ok {
				return s
			}
			return Spec{Model: model, Kind: KindChat}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func testClock() *clock {
	return &clock{t: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC), step: time.Second}
}

// TestProbe_RefusesAModelThatIsNotResidentAndIssuesNoRequest is gate 1,
// the cardinal rule: a probe must never be the thing that loads a model.
// The assertion is on the REQUEST COUNT, not on the verdict — a refusal
// that still sent the completion would have already caused the JIT load.
func TestProbe_RefusesAModelThatIsNotResidentAndIssuesNoRequest(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{})
	p := newProber(t, srv, nil, testClock())

	res := p.Run(context.Background(), "qwen", false)
	if n := sw.chatCalls.Load(); n != 0 {
		t.Fatalf("issued %d requests for a non-resident model; a probe must never load one", n)
	}
	if res.Verdict != fleetapi.VerdictUnknown || !strings.Contains(res.Note, "not resident") {
		t.Fatalf("want an unknown verdict noting residency, got %+v", res)
	}
	if !strings.Contains(res.Note, "warm it first") {
		t.Fatalf("the refusal must point at the declared verb that DOES load: %q", res.Note)
	}
}

// TestProbe_RefusesAModelThatIsStartingRatherThanReady: mid-load is not
// resident. Timing a starting model measures the load.
func TestProbe_RefusesAModelThatIsStartingRatherThanReady(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "starting"})
	p := newProber(t, srv, nil, testClock())

	res := p.Run(context.Background(), "qwen", false)
	if n := sw.chatCalls.Load(); n != 0 {
		t.Fatalf("probed a starting model (%d requests): that measures the load, not the model", n)
	}
	if !strings.Contains(res.Note, "not resident") {
		t.Fatalf("want a residency refusal, got %q", res.Note)
	}
}

// TestScore_UnderMinSamplesIsUnknownNeverDegraded: a fresh cell must not
// scream on boot, however slow the first sample looks.
func TestScore_UnderMinSamplesIsUnknownNeverDegraded(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	for i := range minSamples - 1 {
		sw.setRate(40)
		if i > 0 {
			clk.advance(minGap)
		}
		res := p.Run(context.Background(), "qwen", false)
		if res.Verdict != fleetapi.VerdictUnknown {
			t.Fatalf("sample %d: verdict %q with only %d samples behind it; want unknown", i, res.Verdict, i)
		}
	}
	// A catastrophic sample while still baselining is still unknown.
	clk.advance(minGap)
	sw.setRate(1)
	if res := p.Run(context.Background(), "qwen", false); res.Verdict == fleetapi.VerdictDegraded {
		t.Fatalf("degraded without a full baseline: %+v", res)
	}
}

// baselineUp runs n healthy probes so a baseline exists.
func baselineUp(t *testing.T, p *Prober, sw *swap, clk *clock, model string, n int, rate float64) {
	t.Helper()
	for range n {
		sw.setRate(rate)
		clk.advance(minGap)
		p.Run(context.Background(), model, false)
	}
}

// TestScore_ThreeTimesSlowdownAgainstABaselineIsDegraded is the phase's
// whole reason for existing: llama-server degrades 10-100x while /health
// stays green.
func TestScore_ThreeTimesSlowdownAgainstABaselineIsDegraded(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)
	baselineUp(t, p, sw, clk, "qwen", 10, 40)

	sw.setRate(13)
	clk.advance(minGap)
	res := p.Run(context.Background(), "qwen", false)
	if res.Verdict != fleetapi.VerdictDegraded {
		t.Fatalf("13 tok/s against a 40 tok/s baseline is %q, want degraded (%+v)", res.Verdict, res)
	}
	if res.Ratio <= 0 || res.Ratio > 0.5 {
		t.Fatalf("ratio %v does not describe the slowdown", res.Ratio)
	}
	if res.DegradedSince == nil {
		t.Fatal("degraded_since unset: a model flagged for a week must be legible as such")
	}
}

// TestScore_HysteresisBandKeepsThePreviousVerdict: a model sitting near
// the line must not drip transition events every interval.
func TestScore_HysteresisBandKeepsThePreviousVerdict(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)
	baselineUp(t, p, sw, clk, "qwen", 10, 40)

	// 0.55 of baseline: inside the band, previous verdict was ok.
	sw.setRate(22)
	clk.advance(minGap)
	if res := p.Run(context.Background(), "qwen", false); res.Verdict != fleetapi.VerdictOK {
		t.Fatalf("band sample after ok: got %q, want the previous verdict kept", res.Verdict)
	}
	// Drop below the line, then come back into the band: still degraded.
	sw.setRate(10)
	clk.advance(minGap)
	if res := p.Run(context.Background(), "qwen", false); res.Verdict != fleetapi.VerdictDegraded {
		t.Fatalf("want degraded, got %q", res.Verdict)
	}
	sw.setRate(22)
	clk.advance(minGap)
	if res := p.Run(context.Background(), "qwen", false); res.Verdict != fleetapi.VerdictDegraded {
		t.Fatalf("band sample after degraded: got %q, want the previous verdict kept", res.Verdict)
	}
	// Clearly healthy again: recovered.
	sw.setRate(40)
	clk.advance(minGap)
	if res := p.Run(context.Background(), "qwen", false); res.Verdict != fleetapi.VerdictOK {
		t.Fatalf("want ok after a healthy sample, got %q", res.Verdict)
	}
}

// TestBaseline_DegradedSamplesDoNotEnterTheWindow: if they did, a
// persistent regression would wash out of the median and the status
// would go green while the box was still slow.
func TestBaseline_DegradedSamplesDoNotEnterTheWindow(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)
	baselineUp(t, p, sw, clk, "qwen", 10, 40)

	var last *fleetapi.AnnounceProbe
	for range 15 {
		sw.setRate(10)
		clk.advance(minGap)
		last = p.Run(context.Background(), "qwen", false)
	}
	if last.Verdict != fleetapi.VerdictDegraded {
		t.Fatalf("after 15 consecutive slow probes the verdict is %q: the baseline re-based itself", last.Verdict)
	}
	if last.BaselineP50 < 30 {
		t.Fatalf("baseline drifted to %v; degraded samples must not enter the window", last.BaselineP50)
	}
}

// TestBaseline_RebaselineClearsTheWindow is the escape hatch for a
// LEGITIMATE permanent slowdown, and the only reason the rule above is
// affordable.
func TestBaseline_RebaselineClearsTheWindow(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)
	baselineUp(t, p, sw, clk, "qwen", 10, 40)

	sw.setRate(10)
	clk.advance(minGap)
	if res := p.Run(context.Background(), "qwen", true); res.Verdict != fleetapi.VerdictUnknown {
		t.Fatalf("after rebaseline the verdict is %q; want unknown (baselining from scratch)", res.Verdict)
	}
	if res := p.Run(context.Background(), "qwen", false); res.BaselineP50 > 20 {
		t.Fatalf("old samples survived the rebaseline: p50 %v", res.BaselineP50)
	}
}

// TestBaseline_FlagsChangeStartsAFreshBaseline: a def edit produces a
// different server, and scoring the new one against the old one's
// numbers reports a configuration change as a regression.
func TestBaseline_FlagsChangeStartsAFreshBaseline(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	specs := map[string]Spec{"qwen": {Model: "qwen", Kind: KindChat, FlagsSHA256: "aaa"}}
	p := newProber(t, srv, specs, clk)
	baselineUp(t, p, sw, clk, "qwen", 10, 40)

	specs["qwen"] = Spec{Model: "qwen", Kind: KindChat, FlagsSHA256: "bbb"}
	sw.setRate(10)
	clk.advance(minGap)
	res := p.Run(context.Background(), "qwen", false)
	if res.Verdict != fleetapi.VerdictUnknown {
		t.Fatalf("scored across a flags change: verdict %q, baseline %v", res.Verdict, res.BaselineP50)
	}
	if res.FlagsSHA256 != "bbb" {
		t.Fatalf("result not bound to the flags it measured: %q", res.FlagsSHA256)
	}
}

// TestProbe_CooldownRefusesTheSecondProbeAndKeepsTheLastResult: the
// piggyback queue is at-least-once (C6), so a redelivered command must
// cost nothing.
func TestProbe_CooldownRefusesTheSecondProbeAndKeepsTheLastResult(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	first := p.Run(context.Background(), "qwen", false)
	second := p.Run(context.Background(), "qwen", false)
	if n := sw.chatCalls.Load(); n != 1 {
		t.Fatalf("two commands inside the cooldown ran %d probes; want 1", n)
	}
	if !strings.Contains(second.Note, "cooldown") {
		t.Fatalf("want a cooldown note, got %q", second.Note)
	}
	if second.Value != first.Value || second.Metric != first.Metric {
		t.Fatalf("a refusal erased the last measurement: %+v vs %+v", second, first)
	}

	clk.advance(minGap + time.Second)
	p.Run(context.Background(), "qwen", false)
	if n := sw.chatCalls.Load(); n != 2 {
		t.Fatalf("probe past the cooldown did not run (%d calls)", n)
	}
}

// TestProbe_RefusalsDoNotExtendTheCooldown: keying the cooldown on the
// last RESULT would let a stream of refusals lock a model out forever,
// because a refusal carries the previous measurement's timestamp
// forward.
func TestProbe_RefusalsDoNotExtendTheCooldown(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	// Not resident: refused, no attempt spent.
	p.Run(context.Background(), "qwen", false)
	sw.setRunning("qwen", "ready")
	if res := p.Run(context.Background(), "qwen", false); strings.Contains(res.Note, "cooldown") {
		t.Fatalf("a refusal started the cooldown clock: %q", res.Note)
	}
	if n := sw.chatCalls.Load(); n != 1 {
		t.Fatalf("want the probe to run once the model became resident, got %d calls", n)
	}
}

// TestProbe_DailyCapRefusesPastTheBudget bounds the one place the
// control plane deliberately spends GPU time.
func TestProbe_DailyCapRefusesPastTheBudget(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	for range dailyCap {
		clk.advance(minGap + time.Second)
		p.Run(context.Background(), "qwen", false)
	}
	if n := sw.chatCalls.Load(); n != int64(dailyCap) {
		t.Fatalf("ran %d probes before the cap; want %d", n, dailyCap)
	}
	clk.advance(minGap + time.Second)
	res := p.Run(context.Background(), "qwen", false)
	if !strings.Contains(res.Note, "daily cap") {
		t.Fatalf("want a cap refusal, got %q", res.Note)
	}
	if n := sw.chatCalls.Load(); n != int64(dailyCap) {
		t.Fatalf("the cap did not hold: %d probes ran", n)
	}
	// The cap is a ROLLING 24h window, not a calendar day.
	clk.advance(25 * time.Hour)
	p.Run(context.Background(), "qwen", false)
	if n := sw.chatCalls.Load(); n != int64(dailyCap)+1 {
		t.Fatalf("the rolling window never reopened: %d probes ran", n)
	}
}

// TestProbe_SingleFlightRefusesAConcurrentProbe: a burst of redelivered
// commands must not become a serial probe train.
func TestProbe_SingleFlight(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready", "other": "ready"})
	sw.block = make(chan struct{})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		p.Run(context.Background(), "qwen", false)
	}()
	<-started
	// Wait until the first probe is actually inside the handler.
	deadline := time.Now().Add(2 * time.Second)
	for sw.chatCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	res := p.Run(context.Background(), "other", false)
	if !strings.Contains(res.Note, "already running") {
		t.Fatalf("want a single-flight refusal, got %q", res.Note)
	}
	close(sw.block)
	// Join before the temp dir goes: the first probe still has a state
	// write to do, and a write into a deleted dir is a flaky failure.
	<-done
}

// TestProbe_FallsBackToWallClockUnderItsOwnMetric: an engine with no
// timings block (mlx) is still measurable, but its numbers are NOT
// comparable with decode rates, so they carry a different metric name
// and therefore a different baseline key.
func TestProbe_FallsBackToWallClockUnderItsOwnMetric(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	sw.mu.Lock()
	sw.noTimings = true
	sw.mu.Unlock()
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	res := p.Run(context.Background(), "qwen", false)
	if res.Metric != MetricE2E {
		t.Fatalf("metric %q; a wall-clock measurement must not claim to be a decode rate", res.Metric)
	}
	if res.Value <= 0 {
		t.Fatalf("no value measured: %+v", res)
	}
}

// TestProbe_EmbedKindUsesTheEmbeddingsEndpoint: an embedding model
// probed with a chat completion measures a 400.
func TestProbe_EmbedKindUsesTheEmbeddingsEndpoint(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"embed-large": "ready"})
	clk := testClock()
	p := newProber(t, srv, map[string]Spec{"embed-large": {Model: "embed-large", Kind: KindEmbed}}, clk)

	res := p.Run(context.Background(), "embed-large", false)
	if sw.embedCalls.Load() != 1 || sw.chatCalls.Load() != 0 {
		t.Fatalf("embed probe hit chat=%d embed=%d", sw.chatCalls.Load(), sw.embedCalls.Load())
	}
	if res.Metric != MetricEmbed || res.Value <= 0 {
		t.Fatalf("want an embed-inputs rate, got %+v", res)
	}
}

// TestProbe_DisabledSpecNeverIssuesARequest covers rerank defs (whose
// request shape this probe does not speak) and cloud peers (whose
// throughput is someone else's problem and whose probes cost money).
func TestProbe_DisabledSpecNeverIssuesARequest(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"reranker": "ready"})
	p := newProber(t, srv, map[string]Spec{"reranker": {Model: "reranker", Kind: KindEmbed, Disabled: true}}, testClock())

	res := p.Run(context.Background(), "reranker", false)
	if sw.chatCalls.Load() != 0 || sw.embedCalls.Load() != 0 {
		t.Fatal("a disabled model was probed")
	}
	if !strings.Contains(res.Note, "disabled") {
		t.Fatalf("want a disabled note, got %q", res.Note)
	}
}

// TestProber_BaselinesSurviveARestart: the baseline is the only thing
// that makes a verdict possible, so losing it on restart would mean a
// fleet that never reports anything but `unknown`.
func TestProber_BaselinesSurviveARestart(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	statePath := filepath.Join(t.TempDir(), "model-probe.json")
	mk := func() *Prober {
		p, err := New(Config{LlamaSwapURL: srv.URL, StatePath: statePath, HTTPClient: srv.Client(), Now: clk.now})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}
	p := mk()
	baselineUp(t, p, sw, clk, "qwen", 10, 40)

	restarted := mk()
	sw.setRate(10)
	clk.advance(minGap)
	res := restarted.Run(context.Background(), "qwen", false)
	if res.Verdict != fleetapi.VerdictDegraded {
		t.Fatalf("baseline lost across restart: verdict %q, p50 %v", res.Verdict, res.BaselineP50)
	}
}

// TestProberStart_ReturnsWhileTheProbeIsStillRunning is the production
// half of gate 6. Start is what the announce loop's command handler
// calls, and the heartbeat is the cell's only evidence of life: if Start
// waited for the probe, a degraded model — the slowest probe there is,
// and the exact case this phase exists for — would mark its own cell
// stale for being slow to prove it was slow.
func TestProberStart_ReturnsWhileTheProbeIsStillRunning(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	sw.block = make(chan struct{})
	p := newProber(t, srv, nil, testClock())

	returned := make(chan struct{})
	go func() {
		p.Start("qwen", false)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Start blocked on the probe")
	}
	// And the probe really is in flight — otherwise this would pass
	// against a Start that never probes at all.
	deadline := time.Now().Add(5 * time.Second)
	for sw.chatCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if sw.chatCalls.Load() == 0 {
		t.Fatal("Start returned without ever issuing the probe")
	}
	close(sw.block)
	// Let the background probe finish its state write before the temp
	// dir goes.
	for i := 0; i < 500 && p.Results()["qwen"] == nil; i++ {
		time.Sleep(5 * time.Millisecond)
	}
}

// TestResults_DoesNotAliasProberMemory: the announce loop marshals these
// on another goroutine while probes keep landing.
func TestResults_DoesNotAliasProberMemory(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)
	baselineUp(t, p, sw, clk, "qwen", 6, 40)
	sw.setRate(5)
	clk.advance(minGap)
	p.Run(context.Background(), "qwen", false)

	got := p.Results()["qwen"]
	if got == nil {
		t.Fatal("no result for a probed model")
	}
	got.Verdict = "tampered"
	got.DegradedSince = nil
	if again := p.Results()["qwen"]; again.Verdict == "tampered" || again.DegradedSince == nil {
		t.Fatal("Results() handed out a pointer into prober memory")
	}
}

// ─── adversarial-review regressions (ground rule 9, second pass) ──────

// TestRecord_SamplesCountsWhatBackedTheVerdictNotTheWindowAfterIt: the
// announced `samples` is documented as "how many backed it", and the
// verdict threshold is minSamples. Counting the just-recorded sample
// published "samples: 5, verdict: unknown" on the fifth probe against a
// documented five-sample threshold — an operator reading that concludes
// the scorer is broken, when it is honestly saying "four so far".
func TestRecord_SamplesCountsWhatBackedTheVerdictNotTheWindowAfterIt(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	var scoredAt int
	for i := 1; i <= minSamples+2; i++ {
		sw.setRate(40)
		clk.advance(minGap)
		res := p.Run(context.Background(), "qwen", false)
		if res.Samples >= minSamples && res.Verdict == fleetapi.VerdictUnknown {
			t.Fatalf("probe %d announced samples=%d with verdict unknown: the count does not describe the baseline behind it (%+v)", i, res.Samples, res)
		}
		if res.Verdict != fleetapi.VerdictUnknown && scoredAt == 0 {
			scoredAt = i
			if res.Samples != minSamples {
				t.Fatalf("first scored probe reports samples=%d, want the %d that backed it", res.Samples, minSamples)
			}
		}
	}
	if scoredAt != minSamples+1 {
		t.Fatalf("the first scored probe was #%d; want #%d (minSamples behind it)", scoredAt, minSamples+1)
	}
}

// TestRecord_BaselineAtIsNilUntilThereIsABaseline: BaselineAt is what
// makes "flagged for six days against a July baseline" legible, so it
// must describe the same window BaselineP50 does. Taken after the append
// it dated a baseline of zero on a fresh cell's very first probe.
func TestRecord_BaselineAtIsNilUntilThereIsABaseline(t *testing.T) {
	sw, srv := newSwap(t, map[string]string{"qwen": "ready"})
	clk := testClock()
	p := newProber(t, srv, nil, clk)

	first := p.Run(context.Background(), "qwen", false)
	if first.BaselineP50 != 0 {
		t.Fatalf("first probe already has a baseline: %+v", first)
	}
	if first.BaselineAt != nil {
		t.Fatalf("a baseline of zero was dated %s", first.BaselineAt)
	}
	sw.setRate(40)
	clk.advance(minGap)
	if second := p.Run(context.Background(), "qwen", false); second.BaselineAt == nil {
		t.Fatal("a real baseline carries no age")
	}
}
