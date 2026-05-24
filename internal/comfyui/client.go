// Package comfyui is a typed HTTP client for ComfyUI's REST API.
//
// ComfyUI (https://github.com/comfyanonymous/ComfyUI) is a node-based image
// generation server. This package wraps the subset of its REST API needed by
// vamp's image-generation stages: submit a workflow, poll history, download
// generated files. The websocket event stream is intentionally not modeled —
// polling /history is sufficient for batch use.
package comfyui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned by GetHistory when ComfyUI has no record for the
// given prompt_id (typically because execution has not begun yet).
var ErrNotFound = errors.New("comfyui: not found in history")

// Client is a typed HTTP client for a single ComfyUI server.
type Client struct {
	baseURL  string
	hc       *http.Client
	clientID string
}

// New returns a Client targeting the given baseURL (e.g.
// "http://localhost:8188"). If hc is nil, http.DefaultClient is used. A stable
// client_id is generated for the life of the Client.
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		hc:       hc,
		clientID: newClientID(),
	}
}

// newClientID returns a 32-hex-char random identifier. ComfyUI accepts any
// string here; the canonical web UI uses a UUID. We use the same width as a
// UUID-without-dashes so logs look familiar.
func newClientID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is exceptional; fall back to a timestamp so we
		// still return a non-empty id.
		return fmt.Sprintf("comfyui-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ClientID returns the client_id used in submissions. Stable for the life of
// the Client.
func (c *Client) ClientID() string { return c.clientID }

// BaseURL returns the trimmed base URL the client targets.
func (c *Client) BaseURL() string { return c.baseURL }

// promptResponse is the JSON shape ComfyUI returns from POST /prompt.
type promptResponse struct {
	PromptID   string                    `json:"prompt_id"`
	Number     int                       `json:"number"`
	NodeErrors map[string]nodeErrorEntry `json:"node_errors"`
	Error      *promptTopLevelError      `json:"error,omitempty"`
}

// nodeErrorEntry is ComfyUI's per-node error record. The server emits this
// when a workflow fails validation: a top-level "node_errors" map keyed by
// node id, each value with a class_type, a dependent_outputs list, and a list
// of per-error structs.
type nodeErrorEntry struct {
	ClassType        string            `json:"class_type"`
	DependentOutputs []string          `json:"dependent_outputs"`
	Errors           []nodeErrorDetail `json:"errors"`
}

type nodeErrorDetail struct {
	Type      string         `json:"type"`
	Message   string         `json:"message"`
	Details   string         `json:"details"`
	ExtraInfo map[string]any `json:"extra_info,omitempty"`
}

// promptTopLevelError is the (rarer) workflow-level validation error
// ComfyUI returns alongside or instead of node_errors.
type promptTopLevelError struct {
	Type      string         `json:"type"`
	Message   string         `json:"message"`
	Details   string         `json:"details"`
	ExtraInfo map[string]any `json:"extra_info,omitempty"`
}

// SubmitError is returned from Submit when ComfyUI accepted the request
// (HTTP 200) but rejected the workflow via the node_errors body. It carries
// enough structured data for callers to surface meaningful diagnostics
// without re-parsing.
type SubmitError struct {
	PromptID   string
	NodeErrors map[string]NodeErrors
	TopLevel   *TopLevelError
}

// NodeErrors is the per-node validation result exposed to callers.
type NodeErrors struct {
	ClassType        string
	DependentOutputs []string
	Errors           []NodeError
}

// NodeError is a single validation error attached to a node.
type NodeError struct {
	Type    string
	Message string
	Details string
}

// TopLevelError is a workflow-level validation error not tied to a specific
// node. ComfyUI emits this for malformed payloads (e.g. missing prompt key).
type TopLevelError struct {
	Type    string
	Message string
	Details string
}

func (e *SubmitError) Error() string {
	var b strings.Builder
	b.WriteString("comfyui: submit rejected")
	if e.TopLevel != nil {
		b.WriteString(": ")
		b.WriteString(e.TopLevel.Message)
		if e.TopLevel.Details != "" && e.TopLevel.Details != e.TopLevel.Message {
			b.WriteString(" (")
			b.WriteString(e.TopLevel.Details)
			b.WriteString(")")
		}
	}
	if len(e.NodeErrors) > 0 {
		// Stable iteration so error strings are deterministic for tests/logs.
		ids := make([]string, 0, len(e.NodeErrors))
		for id := range e.NodeErrors {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			ne := e.NodeErrors[id]
			for _, er := range ne.Errors {
				fmt.Fprintf(&b, "; node %s (%s): %s", id, ne.ClassType, er.Message)
				if er.Details != "" && er.Details != er.Message {
					fmt.Fprintf(&b, " [%s]", er.Details)
				}
			}
		}
	}
	return b.String()
}

// Submit posts a workflow to /prompt and returns the prompt_id assigned by
// ComfyUI. workflow is the raw map of node_id -> node definition.
//
// ComfyUI returns HTTP 200 with a populated node_errors body when it rejects
// a workflow; Submit translates that into a *SubmitError so callers can use
// errors.As to inspect the diagnostics. Transport errors and non-2xx
// responses are wrapped with %w-friendly fmt.Errorf.
func (c *Client) Submit(ctx context.Context, workflow map[string]any) (string, error) {
	body := map[string]any{
		"prompt":    workflow,
		"client_id": c.clientID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("comfyui: encode prompt: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/prompt", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("comfyui: build prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("comfyui: POST /prompt: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("comfyui: read /prompt response: %w", err)
	}

	if resp.StatusCode >= 400 {
		snippet := strings.TrimSpace(string(respBody))
		if len(snippet) > 512 {
			snippet = snippet[:512] + "..."
		}
		return "", fmt.Errorf("comfyui: POST /prompt: %s: %s", resp.Status, snippet)
	}

	var pr promptResponse
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &pr); err != nil {
			return "", fmt.Errorf("comfyui: decode /prompt response: %w", err)
		}
	}

	hasNodeErrors := false
	for _, ne := range pr.NodeErrors {
		if len(ne.Errors) > 0 {
			hasNodeErrors = true
			break
		}
	}
	if hasNodeErrors || pr.Error != nil {
		se := &SubmitError{
			PromptID:   pr.PromptID,
			NodeErrors: make(map[string]NodeErrors, len(pr.NodeErrors)),
		}
		for id, ne := range pr.NodeErrors {
			if len(ne.Errors) == 0 {
				continue
			}
			out := NodeErrors{
				ClassType:        ne.ClassType,
				DependentOutputs: ne.DependentOutputs,
				Errors:           make([]NodeError, 0, len(ne.Errors)),
			}
			for _, er := range ne.Errors {
				out.Errors = append(out.Errors, NodeError{
					Type:    er.Type,
					Message: er.Message,
					Details: er.Details,
				})
			}
			se.NodeErrors[id] = out
		}
		if pr.Error != nil {
			se.TopLevel = &TopLevelError{
				Type:    pr.Error.Type,
				Message: pr.Error.Message,
				Details: pr.Error.Details,
			}
		}
		return "", se
	}

	if pr.PromptID == "" {
		return "", fmt.Errorf("comfyui: /prompt response missing prompt_id (body: %s)", strings.TrimSpace(string(respBody)))
	}
	return pr.PromptID, nil
}

// History is a minimally-modeled version of /history/{prompt_id}'s shape.
type History struct {
	Status  HistoryStatus          `json:"status"`
	Outputs map[string]NodeOutputs `json:"outputs"`
}

// HistoryStatus is the status sub-object inside a /history entry.
type HistoryStatus struct {
	Completed bool    `json:"completed"`
	Messages  [][]any `json:"messages,omitempty"`
	StatusStr string  `json:"status_str,omitempty"`
}

// NodeOutputs is a node's outputs map. ComfyUI buckets outputs by media kind
// — SaveImage emits "images", the modern SaveVideo (and recent
// VHS_VideoCombine builds) emit "videos", and older SaveAnimatedWEBP /
// legacy VHS variants emit "gifs". Add fields here as we encounter new
// kinds (audio, latents, etc.). The executor flattens all populated buckets
// into a single deterministically-ordered file list.
type NodeOutputs struct {
	Images []OutputFile `json:"images,omitempty"`
	Videos []OutputFile `json:"videos,omitempty"`
	Gifs   []OutputFile `json:"gifs,omitempty"`
}

// OutputFile names a single file produced by a workflow node. Filename and
// Subfolder are server-relative; Type discriminates the storage area
// ("output" | "temp" | "input").
type OutputFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// GetHistory queries /history/{prompt_id}. Returns (nil, ErrNotFound) when
// the prompt_id has not been recorded yet — ComfyUI returns an empty object
// in that case.
func (c *Client) GetHistory(ctx context.Context, promptID string) (*History, error) {
	if promptID == "" {
		return nil, errors.New("comfyui: GetHistory: empty prompt_id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/history/"+url.PathEscape(promptID), nil)
	if err != nil {
		return nil, fmt.Errorf("comfyui: build /history request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("comfyui: GET /history/%s: %w", promptID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("comfyui: GET /history/%s: %s: %s", promptID, resp.Status, strings.TrimSpace(string(b)))
	}

	// /history returns either {} (no record) or {<prompt_id>: {...}}.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("comfyui: decode /history response: %w", err)
	}
	entry, ok := raw[promptID]
	if !ok {
		return nil, ErrNotFound
	}
	var h History
	if err := json.Unmarshal(entry, &h); err != nil {
		return nil, fmt.Errorf("comfyui: decode /history entry: %w", err)
	}
	return &h, nil
}

// WaitForCompletion blocks until ComfyUI reports that promptID has finished
// or ctx fires.
//
// Implementation: first attempt a WS subscription to /ws?clientId=<id> and
// block on the event stream until the executing/null completion signal for
// our promptID arrives. If the WS handshake fails (dial error, non-101
// response, bad Sec-WebSocket-Accept), fall back to polling /history every
// `interval`. Some ComfyUI deployments disable the WS endpoint or sit behind
// a proxy that strips Upgrade headers; the fallback keeps us correct on
// those.
//
// A non-positive interval is normalized to one second so a misconfigured
// caller doesn't spin in the polling fallback.
//
// Backward-compatible signature: WS is purely an optimization (no extra
// round-trips, no spin loop) so callers don't need to opt in.
func (c *Client) WaitForCompletion(ctx context.Context, promptID string, interval time.Duration) (*History, error) {
	if promptID == "" {
		return nil, errors.New("comfyui: WaitForCompletion: empty prompt_id")
	}
	if interval <= 0 {
		interval = time.Second
	}

	// Try WS first. wsWaitForCompletion blocks until either ctx fires or a
	// completion event arrives. On dial / handshake errors we fall back to
	// polling — but mid-stream errors (connection reset, malformed frames)
	// surface to the caller because falling back from a known-broken WS
	// session would risk masking server-side problems.
	wsErr := c.wsWaitForCompletion(ctx, promptID)
	if wsErr == nil {
		// Completion was signaled over WS; one final GetHistory call
		// returns the outputs the caller actually needs.
		return c.GetHistory(ctx, promptID)
	}
	// Honor ctx cancellation regardless of WS path.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Fall back to polling. We swallow wsErr here because the polling path is
	// the canonical correct behavior; the operator who cares about WS gaps
	// can find them in debug logs of the server itself.
	return c.pollForCompletion(ctx, promptID, interval)
}

// pollForCompletion is the legacy /history-polling loop, retained as the WS
// fallback path. ErrNotFound is treated as transient — execution may not
// have started yet. Any other error is returned immediately.
func (c *Client) pollForCompletion(ctx context.Context, promptID string, interval time.Duration) (*History, error) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		h, err := c.GetHistory(ctx, promptID)
		if err == nil && h.Status.Completed {
			return h, nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// FetchView downloads /view?filename=...&subfolder=...&type=.... The caller
// is responsible for closing the returned ReadCloser.
func (c *Client) FetchView(ctx context.Context, file OutputFile) (io.ReadCloser, error) {
	if file.Filename == "" {
		return nil, errors.New("comfyui: FetchView: empty filename")
	}
	q := url.Values{}
	q.Set("filename", file.Filename)
	q.Set("subfolder", file.Subfolder)
	if file.Type == "" {
		q.Set("type", "output")
	} else {
		q.Set("type", file.Type)
	}
	u := c.baseURL + "/view?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("comfyui: build /view request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("comfyui: GET /view: %w", err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("comfyui: GET /view %s: %s: %s", file.Filename, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// FreeMemory asks the ComfyUI server to unload all model weights and release
// any cached VRAM, keeping the server process itself alive. Used between
// pipeline runs to free GPU memory for non-ComfyUI profiles (e.g. a
// long_form_exl3 activation that needs ~28 GiB) without paying the ComfyUI
// cold-start cost again on the next image_gen stage.
//
// POST /free with both unload_models + free_memory true. ComfyUI returns
// no body; any non-2xx is reported verbatim. Soft-failure semantics — the
// caller (vamp's ComfyUI executor) treats an error from this call as
// non-fatal and logs it, since the workflow already succeeded.
func (c *Client) FreeMemory(ctx context.Context) error {
	body := strings.NewReader(`{"unload_models":true,"free_memory":true}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/free", body)
	if err != nil {
		return fmt.Errorf("comfyui: build /free request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("comfyui: POST /free: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("comfyui: POST /free: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// SaveOutputToFile fetches the named file and writes it to destPath. The
// write is atomic: bytes go to destPath+".tmp" and are renamed into place on
// success. Parent directories are created as needed. Returns the absolute
// path written.
func (c *Client) SaveOutputToFile(ctx context.Context, file OutputFile, destPath string) (string, error) {
	abs, err := filepath.Abs(destPath)
	if err != nil {
		return "", fmt.Errorf("comfyui: resolve dest path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("comfyui: create dest dir: %w", err)
	}

	body, err := c.FetchView(ctx, file)
	if err != nil {
		return "", err
	}
	defer body.Close()

	tmp := abs + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", fmt.Errorf("comfyui: create temp file: %w", err)
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("comfyui: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("comfyui: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("comfyui: rename %s -> %s: %w", tmp, abs, err)
	}
	return abs, nil
}
