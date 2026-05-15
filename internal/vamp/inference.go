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
)

// ChatCompletion calls an OpenAI-compatible /v1/chat/completions endpoint
// with the given prompt and parameter overrides. baseURL is vibe's proxy URL
// (e.g. http://127.0.0.1:9000/v1).
type ChatCompletion struct {
	HTTPClient *http.Client
}

// StreamFunc receives incremental token deltas as they arrive from an SSE
// chat-completion response. It is called once per non-empty content delta and
// must not block on I/O for long; the caller is expected to write to a fast
// sink (e.g. stdout).
type StreamFunc func(delta string)

// Call invokes the chat completion endpoint. When onToken is nil the server is
// asked for a single non-streaming JSON response and the full content is
// returned. When onToken is non-nil the request switches to OpenAI-compatible
// SSE streaming: each content delta is passed to onToken, and the accumulated
// content is returned at the end so callers can persist it.
func (c *ChatCompletion) Call(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	stream := onToken != nil
	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   stream,
	}
	for k, v := range params {
		body[k] = v
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
	resp, err := c.HTTPClient.Do(req)
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
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return "", err
		}
		if len(r.Choices) == 0 {
			return "", errors.New("chat completion: no choices in response")
		}
		return r.Choices[0].Message.Content, nil
	}

	return parseSSEStream(resp.Body, onToken)
}

// parseSSEStream reads an OpenAI-compatible Server-Sent Events stream from r.
// It accumulates content deltas, invoking onToken for each non-empty one, and
// returns the full concatenated content once it sees the [DONE] sentinel or
// EOF.
func parseSSEStream(r io.Reader, onToken StreamFunc) (string, error) {
	scanner := bufio.NewScanner(r)
	// Reasoning models can emit large single-chunk JSON; bump well above the
	// 64KB default so we don't choke mid-stream.
	const maxLine = 1 << 20 // 1 MiB
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	var acc strings.Builder
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
			return acc.String(), nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return "", fmt.Errorf("decode SSE chunk: %w", err)
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
		acc.WriteString(delta)
		onToken(delta)
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read SSE stream: %w", err)
	}
	// Stream ended without [DONE]; return what we have.
	return acc.String(), nil
}

// ResolveModelID queries baseURL/v1/models and returns the first model id, or
// "vibe" if the server returned no models. baseURL is the inference root
// (e.g. http://127.0.0.1:9000); /v1/models is appended.
func ResolveModelID(ctx context.Context, hc *http.Client, baseURL string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
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
