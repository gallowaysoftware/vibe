package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	internalvamp "github.com/gallowaysoftware/vibe/internal/vamp"
)

// DefaultProxyURL is vibe's OpenAI-compatible proxy address (without the
// trailing /v1). Every active-mode profile is fronted here, so a Go program
// talks to one stable address regardless of which model is loaded.
const DefaultProxyURL = "http://127.0.0.1:9000"

// Endpoint is a ready-to-use LLM endpoint returned by EnsureProfile /
// EnsureCapability: an OpenAI-compatible base URL plus the model id the proxy
// is currently serving. Drive it with any OpenAI client —
// POST {BaseURL}/v1/chat/completions with "model": Model.
type Endpoint struct {
	BaseURL string // proxy root, no /v1 (e.g. http://127.0.0.1:9000)
	Model   string // model id to send as the "model" field
}

// EnsureOptions tunes how EnsureProfile waits for readiness.
type EnsureOptions struct {
	// ProxyURL overrides DefaultProxyURL.
	ProxyURL string
	// Timeout bounds the wait for the model to become ready (load weights +
	// answer a probe). Default 6 minutes. Ignored if the context already has a
	// shorter deadline.
	Timeout time.Duration
	// Probe, when true (the default), confirms readiness with a 1-token chat
	// completion rather than trusting /v1/models — a backend can register its
	// alias with the proxy before its weights finish loading, so the probe is
	// what catches a "registered but not actually serving" model early instead
	// of letting the caller's first real request hang.
	Probe *bool
}

// EnsureProfile brings up the named vibe profile and returns a working LLM
// endpoint to drive from Go. This is the "stand up the environment, then use it
// from Go" primitive: declare the profile you need, call EnsureProfile, then
// speak plain OpenAI HTTP to the returned endpoint.
//
// It starts the profile exactly as `<pipeline> activate` does — `vibe start
// <profile>`, which the daemon routes by the profile's mode: field — and is
// idempotent (a profile already holding the active slot is a no-op). It then
// waits until the proxy actually serves the model (see EnsureOptions.Probe).
//
// Note: vibe has a single active LLM slot, so ensuring a profile swaps out
// whatever active profile was running. Call this as a deliberate step, not in
// the middle of someone else's session.
func EnsureProfile(ctx context.Context, profile string, opts ...EnsureOptions) (*Endpoint, error) {
	if strings.TrimSpace(profile) == "" {
		return nil, fmt.Errorf("EnsureProfile: empty profile name")
	}
	o := EnsureOptions{}
	if len(opts) > 0 {
		o = opts[0]
	}
	base := o.ProxyURL
	if base == "" {
		base = DefaultProxyURL
	}
	base = strings.TrimRight(base, "/")
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Minute
	}
	probe := true
	if o.Probe != nil {
		probe = *o.Probe
	}

	if err := vibeStartProfile(ctx, profile); err != nil {
		return nil, err
	}

	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	model, err := waitForModel(waitCtx, base)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", profile, err)
	}
	if probe {
		if err := waitForGeneration(waitCtx, base, model); err != nil {
			return nil, fmt.Errorf("profile %q started but model %q is not generating (still loading or unhealthy): %w", profile, model, err)
		}
	}
	return &Endpoint{BaseURL: base, Model: model}, nil
}

// EnsureCapability resolves the vibe profile bound to capability in
// ~/.config/vibe/capabilities.yaml (the same mapping `activate` and the run
// executor use) and ensures it. Use EnsureProfile directly when you'd rather
// name the profile than rely on a capabilities mapping.
func EnsureCapability(ctx context.Context, capability string, opts ...EnsureOptions) (*Endpoint, error) {
	caps, err := internalvamp.LoadCapabilities()
	if err != nil {
		return nil, fmt.Errorf("EnsureCapability %q: %w", capability, err)
	}
	profile, err := caps.Profile(capability)
	if err != nil {
		return nil, fmt.Errorf("EnsureCapability %q: %w", capability, err)
	}
	return EnsureProfile(ctx, profile, opts...)
}

// StopProfile stops a vibe profile (idempotent). Callers that ensured a profile
// only for a one-shot job can release the GPU slot with this; most leave the
// profile up (like `activate` does) for the next call.
func StopProfile(ctx context.Context, profile string) error {
	cmd := exec.CommandContext(ctx, "vibe", "stop", profile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		body := strings.TrimSpace(string(out))
		if strings.Contains(body, "not running") || strings.Contains(body, "NotFound") {
			return nil
		}
		return fmt.Errorf("vibe stop %s: %w\n%s", profile, err, body)
	}
	return nil
}

// vibeStartProfile shells out to `vibe start <profile>`, treating an
// already-running profile as success. Mirrors the activate command's vibeStart.
func vibeStartProfile(ctx context.Context, profile string) error {
	cmd := exec.CommandContext(ctx, "vibe", "start", profile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		body := strings.TrimSpace(string(out))
		if strings.Contains(body, "already running") || strings.Contains(body, "AlreadyExists") {
			return nil
		}
		return fmt.Errorf("vibe start %s: %w\n%s", profile, err, body)
	}
	return nil
}

// waitForModel polls {base}/v1/models until the proxy reports a model id, then
// returns it. Accepts both vibe's {"models":[{"model"|"name"|"id"}]} shape and
// OpenAI's {"data":[{"id"}]}.
func waitForModel(ctx context.Context, base string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		id, err := fetchModelID(ctx, client, base)
		if err == nil && id != "" {
			return id, nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("proxy at %s never reported a model: %w", base, lastErr)
			}
			return "", fmt.Errorf("proxy at %s never reported a model before timeout", base)
		case <-ticker.C:
		}
	}
}

func fetchModelID(ctx context.Context, client *http.Client, base string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /v1/models: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Model string `json:"model"`
			Name  string `json:"name"`
			ID    string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	for _, d := range parsed.Data {
		if d.ID != "" {
			return d.ID, nil
		}
	}
	for _, m := range parsed.Models {
		switch {
		case m.Model != "":
			return m.Model, nil
		case m.Name != "":
			return m.Name, nil
		case m.ID != "":
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("no model id in /v1/models response")
}

// waitForGeneration confirms the model genuinely generates by asking for a
// single token, retrying until it succeeds or ctx expires. This is the check
// that distinguishes "proxy knows the alias" from "model is loaded and serving".
func waitForGeneration(ctx context.Context, base, model string) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := probeGeneration(ctx, base, model); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("model did not generate before timeout: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func probeGeneration(ctx context.Context, base, model string) error {
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ok"}},
		"max_tokens": 1,
		"stream":     false,
	}
	b, _ := json.Marshal(payload)
	// Per-attempt budget: model load can make the first request block for a
	// while; cap it so a wedged backend doesn't hold the whole attempt.
	attemptCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(attemptCtx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("chat probe status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
