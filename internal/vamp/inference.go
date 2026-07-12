package vamp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ChatCompletion calls an OpenAI-compatible /v1/chat/completions endpoint
// with the given prompt and parameter overrides. baseURL is vibe's proxy URL
// (e.g. http://127.0.0.1:9000/v1).
type ChatCompletion struct {
	HTTPClient *http.Client
}

// defaultInferenceClient bounds time-to-first-byte (and dial/TLS) so a stalled
// or dead backend that accepts the connection but never replies fails with a
// net.Error timeout instead of blocking the run forever. ResponseHeaderTimeout
// (not Client.Timeout) is deliberate: it caps only the wait for response
// headers, leaving long streaming generations free to keep the body open for
// minutes. The resulting timeout satisfies isRetryable so retry_on: timeout
// fires.
func defaultInferenceClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{Transport: tr}
}

// StreamFunc receives incremental token deltas as they arrive from an SSE
// chat-completion response. It is called once per non-empty content delta and
// must not block on I/O for long; the caller is expected to write to a fast
// sink (e.g. stdout).
type StreamFunc func(delta string)

// InferenceMetrics captures per-call throughput. It is populated by Call only
// when a collector is present in the context (see WithMetrics); the zero value
// means "not measured" (e.g. a non-text stage that never hits the LLM).
type InferenceMetrics struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TTFTms           int64   `json:"ttft_ms"`
	TotalMs          int64   `json:"total_ms"`
	GenTPS           float64 `json:"gen_tps"`     // completion tokens / decode seconds
	PrefillTPS       float64 `json:"prefill_tps"` // prompt tokens / prefill seconds
	Source           string  `json:"source"`      // "timings" (llama-server exact) | "usage" (derived)
}

type metricsCtxKey struct{}

// WithMetrics returns a context carrying a fresh metrics collector plus the
// collector itself. The scheduler attaches one around each text stage; Call
// fills it in, and the scheduler reads it back after the call returns. Threading
// it through context keeps InferenceFunc's signature (and its many call sites)
// untouched.
func WithMetrics(ctx context.Context) (context.Context, *InferenceMetrics) {
	m := &InferenceMetrics{}
	return context.WithValue(ctx, metricsCtxKey{}, m), m
}

func metricsFrom(ctx context.Context) *InferenceMetrics {
	m, _ := ctx.Value(metricsCtxKey{}).(*InferenceMetrics)
	return m
}

// usageJSON / timingsJSON mirror the OpenAI `usage` block (portable; both
// llama-server and tabbyAPI emit it) and llama-server's `timings` extension
// (exact prefill/decode split; absent on tabbyAPI).
type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type timingsJSON struct {
	PromptN     int     `json:"prompt_n"`
	PromptMS    float64 `json:"prompt_ms"`
	PredictedN  int     `json:"predicted_n"`
	PredictedMS float64 `json:"predicted_ms"`
}

// finalizeMetrics computes throughput into m from whatever the server returned.
// Prefers llama-server's exact `timings`; otherwise derives from `usage` token
// counts plus measured TTFT/total wall-clock.
func finalizeMetrics(m *InferenceMetrics, u *usageJSON, tg *timingsJSON, ttftMS, totalMS int64) {
	if m == nil {
		return
	}
	m.TotalMs = totalMS
	m.TTFTms = ttftMS
	if u != nil {
		m.PromptTokens = u.PromptTokens
		m.CompletionTokens = u.CompletionTokens
	}
	if tg != nil && tg.PredictedMS > 0 {
		m.Source = "timings"
		if tg.PredictedN > 0 {
			m.CompletionTokens = tg.PredictedN
		}
		if tg.PromptN > 0 {
			m.PromptTokens = tg.PromptN
		}
		m.GenTPS = float64(tg.PredictedN) / (tg.PredictedMS / 1000)
		if tg.PromptMS > 0 {
			m.PrefillTPS = float64(tg.PromptN) / (tg.PromptMS / 1000)
		}
		return
	}
	m.Source = "usage"
	// Decode time = total - prefill(≈TTFT). The -1 excludes the first token,
	// whose latency is prefill, not decode.
	decodeMS := totalMS - ttftMS
	if m.CompletionTokens > 1 && decodeMS > 0 {
		m.GenTPS = float64(m.CompletionTokens-1) / (float64(decodeMS) / 1000)
	}
	if m.PromptTokens > 0 && ttftMS > 0 {
		m.PrefillTPS = float64(m.PromptTokens) / (float64(ttftMS) / 1000)
	}
}

// Call invokes the chat completion endpoint. When onToken is nil the server is
// asked for a single non-streaming JSON response and the full content is
// returned. When onToken is non-nil the request switches to OpenAI-compatible
// SSE streaming: each content delta is passed to onToken, and the accumulated
// content is returned at the end so callers can persist it.
func (c *ChatCompletion) Call(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
	return c.CallMultimodal(ctx, baseURL, model, prompt, nil, params, onToken)
}

// CallMultimodal is like Call but accepts image files that are base64-encoded
// and attached as multimodal content alongside the text prompt. The messages
// body uses the OpenAI-compatible content-as-array format with image_url
// entries. imagePaths is a list of absolute paths to image files (.png, .jpg,
// etc.) to include.
func (c *ChatCompletion) CallMultimodal(ctx context.Context, baseURL, model, prompt string, imagePaths []string, params map[string]any, onToken StreamFunc) (string, error) {
	// Read HTTPClient into a local instead of lazily initializing the shared
	// field: concurrent foreach items call this on one ChatCompletion, and an
	// unsynchronized read-check-write on the field is a data race.
	client := c.HTTPClient
	if client == nil {
		client = defaultInferenceClient()
	}
	stream := onToken != nil

	// Build content: plain string for text-only, array for multimodal.
	var content any
	if len(imagePaths) == 0 {
		content = prompt
	} else {
		content = buildMultimodalContent(prompt, imagePaths)
	}

	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
		"stream": stream,
	}
	for k, v := range params {
		body[k] = v
	}
	if stream {
		// Ask for a usage block in the final SSE chunk so we can record
		// per-stage throughput. Standard OpenAI stream option; honoured by
		// both llama-server and tabbyAPI. Don't clobber a caller override.
		if _, ok := body["stream_options"]; !ok {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("chat completion %s: %s", resp.Status, string(b))
	}

	if !stream {
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage   *usageJSON   `json:"usage"`
			Timings *timingsJSON `json:"timings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return "", err
		}
		if len(r.Choices) == 0 {
			return "", errors.New("chat completion: no choices in response")
		}
		// No TTFT for a non-streamed call; pass 0 so prefill/decode fall back
		// to total wall-clock unless exact server timings are present.
		finalizeMetrics(metricsFrom(ctx), r.Usage, r.Timings, 0, time.Since(start).Milliseconds())
		return r.Choices[0].Message.Content, nil
	}

	return parseSSEStream(ctx, resp.Body, onToken, start, metricsFrom(ctx))
}

// parseSSEStream reads an OpenAI-compatible Server-Sent Events stream from r.
// It accumulates content deltas, invoking onToken for each non-empty one, and
// returns the full concatenated content once it sees the [DONE] sentinel or
// EOF.
func parseSSEStream(ctx context.Context, r io.Reader, onToken StreamFunc, start time.Time, m *InferenceMetrics) (string, error) {
	scanner := bufio.NewScanner(r)
	// Reasoning models can emit large single-chunk JSON; bump well above the
	// 64KB default so we don't choke mid-stream.
	const maxLine = 1 << 20 // 1 MiB
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	var acc strings.Builder
	var firstAt time.Time
	var lastUsage *usageJSON
	var lastTimings *timingsJSON
	// finalize folds whatever the stream reported into the metrics collector.
	// Called on every return path so a mid-stream error still records partials.
	finalize := func() {
		if m == nil {
			return
		}
		var ttftMS int64
		if !firstAt.IsZero() {
			ttftMS = firstAt.Sub(start).Milliseconds()
		}
		finalizeMetrics(m, lastUsage, lastTimings, ttftMS, time.Since(start).Milliseconds())
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// SSE event separator; nothing to do.
			continue
		}
		// We only care about data: lines. Other fields (event:, id:, retry:)
		// and any comment lines starting with ':' are ignored.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		// Strip a single leading space if present ("data: foo"); a missing
		// space ("data:foo") is also valid per the spec.
		payload = strings.TrimPrefix(payload, " ")
		if payload == "" {
			// Heartbeat / keepalive.
			continue
		}
		if payload == "[DONE]" {
			finalize()
			return acc.String(), nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage   *usageJSON   `json:"usage"`
			Timings *timingsJSON `json:"timings"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return "", fmt.Errorf("decode SSE chunk: %w", err)
		}
		// The include_usage final chunk carries usage (and, on llama-server,
		// timings) with an empty choices array — capture before the skip below.
		if chunk.Usage != nil {
			lastUsage = chunk.Usage
		}
		if chunk.Timings != nil {
			lastTimings = chunk.Timings
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			// First event (role-only) and final stop event both arrive with
			// empty content; skip them silently.
			continue
		}
		if firstAt.IsZero() {
			firstAt = time.Now()
		}
		acc.WriteString(delta)
		onToken(delta)
	}
	if err := scanner.Err(); err != nil {
		// A canceled parent context aborts the HTTP transport mid-read; the
		// resulting body-read error may or may not unwrap to context.Canceled
		// depending on Go version and transport (HTTP/1.1 vs HTTP/2). Prefer
		// ctx.Err() so writePipelineJSON can tell operator-cancel apart from
		// a real upstream failure via errors.Is(err, context.Canceled).
		if ctx.Err() != nil {
			return "", fmt.Errorf("read SSE stream: %w", ctx.Err())
		}
		return "", fmt.Errorf("read SSE stream: %w", err)
	}
	// Stream ended without [DONE]; return what we have.
	finalize()
	return acc.String(), nil
}

// ResolveModelID queries baseURL/v1/models and returns the first model id, or
// "vibe" if the server returned no models. baseURL is the inference root
// (e.g. http://127.0.0.1:9000); /v1/models is appended.
func ResolveModelID(ctx context.Context, hc *http.Client, baseURL string) (string, error) {
	if hc == nil {
		// Same bounded client as the chat path: a dead backend that accepts
		// the connection but never replies must not hang model resolution.
		hc = defaultInferenceClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("/v1/models %s", resp.Status)
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if len(r.Data) == 0 {
		return "vibe", nil
	}
	return r.Data[0].ID, nil
}

// buildMultimodalContent builds the OpenAI-compatible content array for a
// multimodal request. Returns [{type: "text", text: ...}, {type: "image_url",
// image_url: {url: "data:image/...;base64,..."}}] for each image. Only common
// image extensions are accepted; non-image files are silently skipped.
func buildMultimodalContent(text string, imagePaths []string) []map[string]any {
	var content []map[string]any
	content = append(content, map[string]any{"type": "text", "text": text})

	allowed := map[string]bool{
		".png":  true,
		".jpg":  true,
		".jpeg": true,
		".gif":  true,
		".webp": true,
		".bmp":  true,
	}
	for _, p := range imagePaths {
		ext := strings.ToLower(filepath.Ext(p))
		if !allowed[ext] {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		mime := mimeTypeForExt(ext)
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))},
		})
	}
	return content
}

// mimeTypeForExt returns a MIME type for common image extensions. Defaults to
// "application/octet-stream" for unknown extensions (shouldn't happen given
// the allowed set above, but defensive).
func mimeTypeForExt(ext string) string {
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "application/octet-stream"
	}
}
