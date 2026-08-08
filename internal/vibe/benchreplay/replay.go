package benchreplay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/modelprobe"
)

// The replay. n real requests through the cell's own llama-swap, scored
// structurally, against a model that is ALREADY RESIDENT.

// Side is one model to replay the sample through.
type Side struct {
	// Model is the def name the cell's llama-swap serves it under.
	Model string
	// Warm is called before the replay. It belongs to the CALLER because
	// warming is a declared act with its own verb, and folding it into a
	// measurement is how a measurement becomes an actuator. This package
	// then re-checks residency itself and refuses rather than loading —
	// C8's cardinal rule, held on this side of the seam too.
	Warm func(context.Context) error
}

// replayResult is one side's per-request outcomes. Unexported: the only
// thing that leaves this package is Report.
type replayResult struct {
	model  string
	shapes []shape
	// tokS is the per-request rate, 0 where none was measured.
	tokS []float64
	// metric is MetricDecode when every measured request reported
	// llama.cpp's own timings block, MetricE2E when every one fell back to
	// wall clock, and metricMixed when they disagreed. A side that is not
	// one metric is not comparable to anything.
	metric string
}

// metricMixed marks a side whose requests did not agree on how they were
// timed. It is not a metric; it is the reason a ratio is refused.
const metricMixed = "mixed"

// Run replays the sample through both sides and returns the scores.
//
// One entry point for both sides on purpose. A paired ratio and a noise
// floor each need both, and an API that could produce a one-sided score is
// an API that invites somebody to save it and trend it — which is the
// obvious next feature and is wrong, because each run's sample is
// different traffic and a trend across runs would compare two workloads
// and call the difference a model change.
//
// The order is incumbent first, then candidate, matching C18's: on a
// single-GPU cell loading one evicts the other, so they cannot be
// interleaved.
func (s *Set) Run(ctx context.Context, cell string, incumbent, candidate Side) (*Report, error) {
	if s == nil || len(s.items) == 0 {
		return nil, ErrNoCaptures
	}
	inc, err := s.replay(ctx, incumbent)
	if err != nil {
		return nil, err
	}
	cand, err := s.replay(ctx, candidate)
	if err != nil {
		return nil, err
	}
	return s.score(cell, inc, cand), nil
}

func (s *Set) replay(ctx context.Context, side Side) (replayResult, error) {
	if side.Model == "" {
		return replayResult{}, fmt.Errorf("benchreplay: a side needs a model")
	}
	if side.Warm != nil {
		if err := side.Warm(ctx); err != nil {
			return replayResult{}, fmt.Errorf("warm %s before replaying: %w", side.Model, err)
		}
	}
	resident, err := s.isResident(ctx, side.Model)
	if err != nil {
		// "Could not check" is not "yes". Reading an unreadable /running
		// as residency is how a measurement becomes the thing that loads a
		// model.
		return replayResult{}, fmt.Errorf("could not read this cell's /running to check whether %s is resident, and a replay must not load a model: %w", side.Model, err)
	}
	if !resident {
		return replayResult{}, fmt.Errorf("%s is %w", side.Model, ErrNotResident)
	}

	res := replayResult{model: side.Model}
	var sawDecode, sawE2E bool
	for _, item := range s.items {
		sh, rate, metric := s.one(ctx, side.Model, item)
		res.shapes = append(res.shapes, sh)
		res.tokS = append(res.tokS, rate)
		switch metric {
		case modelprobe.MetricDecode:
			sawDecode = true
		case modelprobe.MetricE2E:
			sawE2E = true
		}
	}
	switch {
	case sawDecode && sawE2E:
		res.metric = metricMixed
	case sawDecode:
		res.metric = modelprobe.MetricDecode
	case sawE2E:
		res.metric = modelprobe.MetricE2E
	}
	return res, nil
}

// one replays a single captured request.
//
// Exactly two fields of the client's own body are rewritten, and both are
// named here because §5's argument against injecting a `seed` applies to
// every other field: a sample you had to falsify is no longer YOUR
// workload, which was the whole point.
//
//   - `model`, which must name the side being measured. Unavoidable.
//   - `stream`, forced off, with `stream_options` dropped with it. Both
//     sides get the identical treatment so the paired ratio is unaffected,
//     and it is what makes llama.cpp's `timings` block arrive as one
//     object rather than as a frame nobody may reassemble. It is a
//     caveat, and the report prints it.
//
// temperature, top_p, max_tokens, tools and the messages are the client's
// own and are left exactly as recorded.
func (s *Set) one(ctx context.Context, model string, item sample) (shape, float64, string) {
	body, ok := rewriteForReplay(item.reqBody, model)
	if !ok {
		return shape{toolOutcome: ToolNone, finish: FinishNone}, 0, ""
	}
	ctx, cancel := context.WithTimeout(ctx, s.opt.ReplayTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.opt.LlamaSwapURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return shape{toolOutcome: ToolNone, finish: FinishNone}, 0, ""
	}
	req.Header.Set("Content-Type", "application/json")
	start := s.opt.Now()
	resp, err := s.opt.HTTP.Do(req)
	if err != nil {
		// A transport failure, a timeout: no shape, no rate. An abandoned
		// request is a FAILURE, never a slow number nobody measured.
		return shape{toolOutcome: ToolNone, finish: FinishNone}, 0, ""
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCaptureBytes))
	elapsed := s.opt.Now().Sub(start)
	if err != nil {
		return shape{status: resp.StatusCode, toolOutcome: ToolNone, finish: FinishNone}, 0, ""
	}
	if resp.StatusCode != http.StatusOK {
		// The body is DISCARDED rather than reported. llama-swap's error
		// strings are built out of the upstream's response, and the
		// upstream's response to a captured prompt is about that prompt.
		return shape{status: resp.StatusCode, toolOutcome: ToolNone, finish: FinishNone}, 0, ""
	}
	sh := shapeOf(resp.StatusCode, raw, item.facts)
	rate, metric := rateOf(raw, sh, elapsed)
	return sh, rate, metric
}

// rateOf reads the per-request throughput, preferring llama.cpp's own
// timings block for exactly C8's reason: decode_tok_s excludes queueing
// and e2e_tok_s does not, so the two must never share a column.
func rateOf(raw []byte, sh shape, elapsed time.Duration) (float64, string) {
	var r chatResponse
	if json.Unmarshal(raw, &r) == nil {
		if r.Timings.PredictedPerSecond > 0 {
			return r.Timings.PredictedPerSecond, modelprobe.MetricDecode
		}
		if r.Timings.PredictedN > 0 && r.Timings.PredictedMS > 0 {
			return r.Timings.PredictedN / (r.Timings.PredictedMS / 1000), modelprobe.MetricDecode
		}
	}
	if sh.outTokens > 0 && elapsed > 0 {
		return float64(sh.outTokens) / elapsed.Seconds(), modelprobe.MetricE2E
	}
	return 0, ""
}

// rewriteForReplay produces the body to send. It decodes into raw messages
// so every value the client sent survives byte-for-byte; only the two keys
// named on `one` are touched.
func rewriteForReplay(body []byte, model string) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	if _, ok := obj["messages"]; !ok {
		return nil, false
	}
	m, err := json.Marshal(model)
	if err != nil {
		return nil, false
	}
	obj["model"] = m
	obj["stream"] = json.RawMessage("false")
	delete(obj, "stream_options")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// isResident reads the LOCAL llama-swap's /running. Anything other than a
// clear "yes, ready" is a refusal — including an error, which is why the
// bool and the error are BOTH returned and the caller may not drop either.
func (s *Set) isResident(ctx context.Context, model string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, runningTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(s.opt.LlamaSwapURL, "/")+"/running", nil)
	if err != nil {
		return false, err
	}
	resp, err := s.opt.HTTP.Do(req)
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
	for _, r := range wrap.Running {
		if r.Model == model && r.State == "ready" {
			return true, nil
		}
	}
	return false, nil
}
