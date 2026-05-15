package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ChatCompletion calls an OpenAI-compatible /v1/chat/completions endpoint
// with the given prompt and parameter overrides. baseURL is vibe's proxy URL
// (e.g. http://127.0.0.1:9000/v1).
type ChatCompletion struct {
	HTTPClient *http.Client
}

func (c *ChatCompletion) Call(ctx context.Context, baseURL, model, prompt string, params map[string]any) (string, error) {
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
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
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("chat completion %s: %s", resp.Status, string(b))
	}
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
