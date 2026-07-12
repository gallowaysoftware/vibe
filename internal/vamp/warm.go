package vamp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// warmAttemptTimeout bounds a single warm probe. Behind an external router
// (llama-swap on :9000) the probe is what triggers the JIT load, and a big
// model can take minutes to come up — so one attempt gets room to ride the
// load out instead of cancelling and re-queueing every 60s. The caller's
// overall deadline stays the hang guard; this cap only bounds a wedged
// backend that accepted the connection and went silent.
const warmAttemptTimeout = 5 * time.Minute

// warmRetryInterval is the pause between warm attempts in WaitForWarm.
const warmRetryInterval = 3 * time.Second

// warmOverallTimeout bounds one capability's ensure in WarmCapabilities
// (activation + model resolve + warm). Sized above the biggest local cold
// start observed so far plus headroom; a stuck backend fails the warm pass
// instead of hanging the run forever.
const warmOverallTimeout = 10 * time.Minute

// WarmModel confirms model genuinely generates by asking baseURL (proxy
// root, no /v1) for a single token with stream: true and reading the SSE to
// completion. Streaming matters behind llama-swap: a cold start is
// observable byte-by-byte (loading-state chunks arrive as
// delta.reasoning_content while the model loads) instead of a silent POST
// hold, and a start failure arrives as an in-stream error payload that maps
// to a typed *RouterError. hc nil uses the default bounded client.
//
// Success = the stream reached [DONE] or produced a real completion chunk
// (content delta or finish_reason). Loading-state chunks alone don't count:
// a stream that dies mid-load without llama-swap's error payload is a start
// failure, not a warm model.
func WarmModel(ctx context.Context, hc *http.Client, baseURL, model string) error {
	if hc == nil {
		hc = defaultInferenceClient()
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ok"}},
		"max_tokens": 1,
		"stream":     true,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return classifyTransportError(model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return classifyHTTPFailure(model, resp.StatusCode, body)
	}
	return readWarmStream(ctx, resp.Body, model)
}

// readWarmStream consumes the probe's response body until completion,
// failure payload, or EOF. Tolerates both SSE ("data: {...}") and a bare
// JSON error body — llama-swap reports post-200 start failures as the final
// SSE data line on streaming paths and as a plain JSON body otherwise.
func readWarmStream(ctx context.Context, r io.Reader, model string) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	completed := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		payload := line
		if strings.HasPrefix(line, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		} else if !strings.HasPrefix(line, "{") {
			// SSE field we don't care about (event:, id:) or a keepalive
			// comment line.
			continue
		}
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return nil
		}
		if msg, ok := routerFailureMessage([]byte(payload)); ok {
			return classifyFailureMessage(model, msg)
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			// Loading-state chunks carry only delta.reasoning_content;
			// neither branch fires for them, so they keep the wait alive
			// without counting as a completion.
			if c.Delta.Content != "" || (c.FinishReason != nil && *c.FinishReason != "") {
				completed = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("warm %s: %w", model, ctx.Err())
		}
		return fmt.Errorf("warm %s: read stream: %w", model, err)
	}
	if completed {
		return nil
	}
	return &RouterError{Code: RouterStartFailed, Model: model, Detail: "stream ended before the model produced a completion"}
}

// WaitForWarm retries WarmModel until it succeeds, the failure is
// non-retryable, or ctx expires. Each attempt is individually bounded by
// attemptTimeout (<=0 uses the 5-minute default) so a wedged backend can't
// absorb the whole budget.
//
// Non-retryable: NOT_FOUND (the catalog won't grow a model by asking again)
// and START_FAILED (the router watched the start fail after committing to a
// 200 — an authoritative verdict; re-asking immediately re-queues another
// multi-minute doomed load). Retryable: CAPACITY, UPSTREAM_DOWN, and
// untyped errors (per-attempt timeouts, mid-stream read hiccups) — those are
// transient by nature, so wait retryInterval (<=0 uses 3s) and try again.
func WaitForWarm(ctx context.Context, hc *http.Client, baseURL, model string, attemptTimeout, retryInterval time.Duration) error {
	if attemptTimeout <= 0 {
		attemptTimeout = warmAttemptTimeout
	}
	if retryInterval <= 0 {
		retryInterval = warmRetryInterval
	}
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := WarmModel(attemptCtx, hc, baseURL, model)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrStartFailed) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("model %q did not warm before timeout: %w", model, lastErr)
		case <-ticker.C:
		}
	}
}

// WarmCapabilities fires Ensure for every capability the pipeline's stages
// declare, in parallel, so cold starts overlap instead of serializing at
// each capability group's first stage. For LLM-bearing capabilities
// (text/compact stages) the ensure includes a streaming warm probe; for
// non-OpenAI backends (comfyui, TTS) it stops at activation. One log line
// per capability reports the resolve and warm durations.
func (e *Executor) WarmCapabilities(ctx context.Context) error {
	type capPlan struct {
		name       string
		needsModel bool
	}
	index := make(map[string]int)
	var plans []capPlan
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		if st.Capability == "" || !stageRequiresVibeProfile(st) {
			continue
		}
		t := stageTypeOrDefault(st)
		needsModel := t == StageTypeText || t == StageTypeCompact
		if idx, ok := index[st.Capability]; ok {
			plans[idx].needsModel = plans[idx].needsModel || needsModel
			continue
		}
		index[st.Capability] = len(plans)
		plans = append(plans, capPlan{name: st.Capability, needsModel: needsModel})
	}
	if len(plans) == 0 {
		e.logf("warm: pipeline declares no capabilities; nothing to do")
		return nil
	}
	e.logf("warm: ensuring %d capability(ies) in parallel", len(plans))
	var wg sync.WaitGroup
	errs := make([]error, len(plans))
	for i, pl := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.warmCapability(ctx, pl.name, pl.needsModel)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// warmCapability ensures a single capability: candidate activation, model-id
// resolution, then (for LLM capabilities) the streaming warm probe.
func (e *Executor) warmCapability(ctx context.Context, capability string, needsModel bool) error {
	warmCtx, cancel := context.WithTimeout(ctx, warmOverallTimeout)
	defer cancel()
	resolveStart := time.Now()
	status, cand, err := e.activateCapability(warmCtx, capability)
	if err != nil {
		e.logf("warm %q: activation failed after %s: %v", capability, time.Since(resolveStart).Round(time.Millisecond), err)
		return fmt.Errorf("warm capability %q: %w", capability, err)
	}
	base := strings.TrimSuffix(status.ProxyAddr, "/v1")
	if !needsModel {
		e.logf("warm %q: backend %q active (resolve %s; non-LLM backend, no generate probe)",
			capability, cand, time.Since(resolveStart).Round(time.Millisecond))
		return nil
	}
	model, err := ResolveModelID(warmCtx, nil, base, status.GetProfile())
	if err != nil {
		e.logf("warm %q: backend %q active but model resolution failed: %v", capability, cand, err)
		return fmt.Errorf("warm capability %q: resolve model id: %w", capability, err)
	}
	resolveDur := time.Since(resolveStart)
	warmStart := time.Now()
	if err := WaitForWarm(warmCtx, nil, base, model, warmAttemptTimeout, warmRetryInterval); err != nil {
		e.logf("warm %q: backend %q model %q failed after %s: %v", capability, cand, model, time.Since(warmStart).Round(time.Millisecond), err)
		return fmt.Errorf("warm capability %q (model %q): %w", capability, model, err)
	}
	e.logf("warm %q: backend %q model %q ready (resolve %s, warm %s)",
		capability, cand, model, resolveDur.Round(time.Millisecond), time.Since(warmStart).Round(time.Millisecond))
	return nil
}
