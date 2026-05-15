// Package vibeclient is the typed Go SDK for vibe's HTTP control plane. It is
// scoped to profile lifecycle (start/stop/status); inference is OpenAI-
// compatible HTTP at Status.ProxyAddr, which callers hit directly with the
// HTTP client of their choice.
package vibeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const DefaultBaseURL = "http://127.0.0.1:9001"

type Client struct {
	baseURL string
	hc      *http.Client
}

// New returns a client targeting vibe's HTTP control plane. baseURL is the
// first non-empty of: the argument, $VIBE_API, DefaultBaseURL.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("VIBE_API")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		hc:      &http.Client{}, // no timeout; Start can take minutes for cold model loads
	}
}

// BaseURL returns the configured control-plane address.
func (c *Client) BaseURL() string { return c.baseURL }

// Status mirrors vibe's status payload. The fields are duplicated here (rather
// than re-exporting internal/vibe/ipc) so external callers can depend on this
// package alone.
type Status struct {
	Running     bool      `json:"running"`
	Ready       bool      `json:"ready"`
	Profile     string    `json:"profile,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	BackendAddr string    `json:"backend_addr,omitempty"`
	ProxyAddr   string    `json:"proxy_addr,omitempty"`
	PID         int       `json:"pid,omitempty"`
}

type Profile struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path"`
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	if err := c.do(ctx, http.MethodGet, "/status", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) ListProfiles(ctx context.Context) ([]Profile, error) {
	var resp struct {
		Profiles []Profile `json:"profiles"`
	}
	if err := c.do(ctx, http.MethodGet, "/list", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Profiles, nil
}

func (c *Client) Start(ctx context.Context, profile string) (*Status, error) {
	var resp struct {
		Status Status `json:"status"`
	}
	body := map[string]string{"profile": profile}
	if err := c.do(ctx, http.MethodPost, "/start", body, &resp); err != nil {
		return nil, err
	}
	return &resp.Status, nil
}

func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/stop", nil, nil)
}

func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/shutdown", nil, nil)
}

// EnsureActive returns immediately if `profile` is already the active, ready
// profile. Otherwise it stops any active profile and starts `profile`. The
// returned Status is the post-condition.
func (c *Client) EnsureActive(ctx context.Context, profile string) (*Status, error) {
	s, err := c.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if s.Running && s.Ready && s.Profile == profile {
		return s, nil
	}
	if s.Running {
		if err := c.Stop(ctx); err != nil {
			return nil, fmt.Errorf("stop %q before switching to %q: %w", s.Profile, profile, err)
		}
	}
	return c.Start(ctx, profile)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
