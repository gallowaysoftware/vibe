// Package fleetmcp is the fleet control plane's MCP facade (fleet-control
// C1): conversational agents flip fleet state through typed tools instead
// of editing YAML. Wire pattern cloned from internal/vibe/search/mcp.go —
// streamable HTTP, one JSON-RPC request per POST, no SSE and no session
// state, because every tool is a self-contained request/response.
//
// Mounted at /mcp on the daemon's mux only under fleet_registry: an
// ordinary box's daemon has no fleet to describe. Auth is the mux
// wrapper's business (bearer on TCP, none on the unix socket).
package fleetmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// mcpProtocolVersion mirrors the search MCP: the initialize/tools-list/
// tools-call shape has been stable across spec revisions, so a client
// asking for a different revision gets its own version echoed back.
const mcpProtocolVersion = "2025-06-18"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// toolTimeout bounds the fleet_status and unload_model tool calls. The
// state snapshot already probes every cell with its own deadline, so this
// only needs slack beyond it.
const toolTimeout = 10 * time.Second

// Server serves the fleet MCP tools over the fleetapi substrate and the
// hosts.yaml membership record.
type Server struct {
	fleet *fleetapi.Server
	hosts *fleetcfg.File
	http  *http.Client
	// warmHTTP has no client-level Timeout: the warm request exists to
	// trigger a JIT load, and slow cold starts (minutes) are exactly the
	// ones the tool is for. The goroutine's context is the only bound.
	warmHTTP *http.Client
	// frontConfig is the front's rendered llama-swap config path as seen
	// from this daemon (a same-host mount) — render_front diffs against
	// it. Empty means render-only, no diff.
	frontConfig string
	// backendsDir and llamaBinary feed the renderer the same inputs the
	// CLI gives it.
	backendsDir string
	llamaBinary string
}

// Options carries the render_front tool's renderer inputs (the daemon's
// own config values).
type Options struct {
	FrontConfig string
	BackendsDir string
	LlamaBinary string
}

// New builds the facade over the daemon's fleet server and parsed
// hosts.yaml (both fleet_registry-only, so both always present here).
func New(fleet *fleetapi.Server, hosts *fleetcfg.File, opts Options) *Server {
	return &Server{
		fleet:       fleet,
		hosts:       hosts,
		http:        &http.Client{Timeout: toolTimeout},
		warmHTTP:    &http.Client{},
		frontConfig: opts.FrontConfig,
		backendsDir: opts.BackendsDir,
		llamaBinary: opts.LlamaBinary,
	}
}

// Register mounts the MCP endpoint on the daemon mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/mcp", s.handleMCP)
}

// handleMCP serves one JSON-RPC request per POST. GET is how a client
// would open a server-initiated SSE stream; this server never initiates
// messages, so declining is correct.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPCError(w, nil, rpcParseError, "read body: "+err.Error())
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reqs []jsonRPCRequest
		if err := json.Unmarshal(body, &reqs); err != nil {
			writeRPCError(w, nil, rpcParseError, "invalid json batch")
			return
		}
		var out []jsonRPCResponse
		for _, req := range reqs {
			if resp, ok := s.dispatchRPC(r.Context(), req); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, rpcParseError, "invalid json")
		return
	}
	resp, ok := s.dispatchRPC(r.Context(), req)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// dispatchRPC routes one JSON-RPC message. The bool reports whether a
// response should be sent: notifications get none.
func (s *Server) dispatchRPC(ctx context.Context, req jsonRPCRequest) (jsonRPCResponse, bool) {
	isNotification := len(req.ID) == 0
	reply := func(result any) (jsonRPCResponse, bool) {
		if isNotification {
			return jsonRPCResponse{}, false
		}
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	}
	fail := func(code int, msg string) (jsonRPCResponse, bool) {
		if isNotification {
			return jsonRPCResponse{}, false
		}
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: &jsonRPCError{Code: code, Message: msg}}, true
	}

	switch req.Method {
	case "initialize":
		version := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 && json.Unmarshal(req.Params, &p) == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
		return reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vibe-fleet", "version": "1"},
		})

	case "notifications/initialized", "notifications/cancelled":
		return jsonRPCResponse{}, false

	case "ping":
		return reply(map[string]any{})

	case "tools/list":
		return reply(map[string]any{"tools": s.mcpTools()})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(rpcInvalidParams, "invalid params: "+err.Error())
		}
		result, err := s.callTool(ctx, p.Name, p.Arguments)
		if err != nil {
			// Tool failures are a RESULT with isError, not a protocol
			// error: the model is supposed to see the failure text and
			// decide what to do.
			return reply(map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			})
		}
		return reply(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": result}},
			"isError": false,
		})

	default:
		return fail(rpcMethodNotFound, "unknown method "+req.Method)
	}
}

func (s *Server) mcpTools() []any {
	return []any{
		map[string]any{
			"name": "fleet_status",
			"description": "Fleet-wide state: one derived row per cell (SERVING / DRAINED / " +
				"DRAINED? / OFF / OFF/AWAY / INCONSISTENT) with resident models, declared " +
				"intent (why a cell is drained), last-seen for absent cells, and per-model " +
				"cold-start ETA history. Answer 'what is every cell doing, and why' with this.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		map[string]any{
			"name": "warm_model",
			"description": "Warm (JIT-load) a chat model by sending a 1-token request through " +
				"the fleet front — JIT IS the start verb. Returns immediately with the " +
				"cold-start ETA from history; the model loads in the background. " +
				"Embed/rerank-class models are rejected: warming those means their pinned " +
				"cell is misconfigured.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"model": map[string]any{"type": "string", "description": "Model id as listed in the front catalog."},
				},
				"required": []string{"model"},
			},
		},
		map[string]any{
			"name": "unload_model",
			"description": "Evict a resident model from a cell (llama-swap unload API). The " +
				"next request for it JIT-loads again. Use to force a VRAM reshuffle without " +
				"stopping anything.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":  map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
					"model": map[string]any{"type": "string", "description": "Model id to evict."},
				},
				"required": []string{"cell", "model"},
			},
		},
		map[string]any{
			"name": "drain_cell",
			"description": "Reclaim a cell (stop its serving stack; llama-swap drains in-flight " +
				"requests first). Returns the pre-drain report — resident models, in-flight " +
				"count, active leases — so you can relay \"heads up: X holds a lease\" before " +
				"confirming. Records intent (reason/eta) at fleetd after the drain succeeds.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":         map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
					"reason":       map[string]any{"type": "string", "description": "Why the cell is being reclaimed (e.g. \"gaming\")."},
					"eta":          map[string]any{"type": "string", "description": "When it's expected back (e.g. \"23:00\")."},
					"wait_seconds": map[string]any{"type": "integer", "description": "Wait up to this many seconds for in-flight requests to finish before draining (0 = immediate; in-flight work then dies truncated)."},
				},
				"required": []string{"cell"},
			},
		},
		map[string]any{
			"name": "resume_cell",
			"description": "Resume a drained cell. Models return by JIT on next request; " +
				"clears the cell's recorded intent.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell": map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
				},
				"required": []string{"cell"},
			},
		},
		map[string]any{
			"name": "wake_cell",
			"description": "Send a Wake-on-LAN packet to a cell. Always explicit — never " +
				"triggered by a request. Only works for cells with wake: config in hosts.yaml.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell": map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
				},
				"required": []string{"cell"},
			},
		},
		map[string]any{
			"name": "render_front",
			"description": "Dry-run the front config render (vibe router render --cell front): " +
				"renders the peers-only config from backend defs + hosts.yaml and returns the " +
				"unified diff against the live front config (or the full render when no live " +
				"path is mounted). DRY-RUN ONLY — the apply path arrives with presence-driven " +
				"rendering (C3).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dry_run": map[string]any{"type": "boolean", "description": "Must be true (the only mode C2 has)."},
				},
			},
		},
	}
}

func (s *Server) callTool(ctx context.Context, name string, rawArgs json.RawMessage) (string, error) {
	// Actuation tools get deadlines sized to the work, not the 10s probe
	// budget: a quiescence wait outlives toolTimeout by design, and a
	// unit stop routinely runs 30-60s. (The daemon-side verb is also
	// detach-safe now, but a spurious client-side failure still desyncs
	// intent reporting — the cell drains while the agent hears
	// "failed".)
	timeout := toolTimeout
	switch name {
	case "drain_cell":
		var probe struct {
			WaitSeconds int32 `json:"wait_seconds"`
		}
		_ = json.Unmarshal(rawArgs, &probe)
		timeout = time.Duration(probe.WaitSeconds)*time.Second + 75*time.Second
	case "resume_cell", "wake_cell", "render_front":
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch name {
	case "fleet_status":
		return s.toolFleetStatus(ctx)
	case "warm_model":
		var args struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolWarmModel(ctx, args.Model)
	case "unload_model":
		var args struct {
			Cell  string `json:"cell"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolUnloadModel(ctx, args.Cell, args.Model)
	case "drain_cell":
		var args struct {
			Cell        string `json:"cell"`
			Reason      string `json:"reason"`
			ETA         string `json:"eta"`
			WaitSeconds int32  `json:"wait_seconds"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolDrainCell(ctx, args.Cell, args.Reason, args.ETA, args.WaitSeconds)
	case "resume_cell":
		var args struct {
			Cell string `json:"cell"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolResumeCell(ctx, args.Cell)
	case "wake_cell":
		var args struct {
			Cell string `json:"cell"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolWakeCell(ctx, args.Cell)
	case "render_front":
		var args struct {
			DryRun *bool `json:"dry_run"`
		}
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %v", err)
			}
		}
		return s.toolRenderFront(ctx, args.DryRun)
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

// toolFleetStatus returns the same state document /api/fleet/state serves,
// as compact JSON — one struct shared with the HTTP surface, so agents and
// humans can never disagree about the fleet.
func (s *Server) toolFleetStatus(ctx context.Context) (string, error) {
	snap := s.fleet.Snapshot(ctx)
	data, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// toolWarmModel fires a 1-token chat completion at the front for the named
// model and returns immediately — llama-swap's JIT does the load while the
// agent moves on. The ETA comes from the persisted start-duration history.
func (s *Server) toolWarmModel(ctx context.Context, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("model is required")
	}
	if class, pinned := s.hosts.ModelClasses[model]; pinned {
		return "", fmt.Errorf("%s is %s-class (per hosts.yaml model_classes), not chat: "+
			"warming it with a chat completion would load it for nothing — its pinned cell's "+
			"config is what needs fixing", model, class)
	}
	front, ok := s.hosts.Cells[fleetcfg.FrontCell]
	if !ok {
		return "", fmt.Errorf("no %q cell in hosts.yaml", fleetcfg.FrontCell)
	}
	known, err := s.modelInCatalog(ctx, front.URL, model)
	if err != nil {
		return "", fmt.Errorf("check front catalog: %v", err)
	}
	if !known {
		return "", fmt.Errorf("unknown model %q (not in the front catalog)", model)
	}

	// Fire-and-forget with its own deadline: the request exists to trigger
	// the JIT load, not to be answered. A detached context because the MCP
	// call returns (and cancels ctx) immediately; a dedicated client with
	// no Timeout so a multi-minute cold start isn't aborted mid-load.
	go func() {
		warmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "warm"}},
		})
		if err != nil {
			slog.Warn("warm_model body build failed", "model", model, "err", err)
			return
		}
		req, err := http.NewRequestWithContext(warmCtx, http.MethodPost,
			strings.TrimRight(front.URL, "/")+"/v1/chat/completions",
			bytes.NewReader(body))
		if err != nil {
			slog.Warn("warm_model request build failed", "model", model, "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.warmHTTP.Do(req)
		if err != nil {
			slog.Warn("warm_model request failed", "model", model, "err", err)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if stats, ok := s.fleet.StartStats(model); ok && stats.Count > 0 {
		return fmt.Sprintf("Warming %s through the front (JIT load started in the background)."+
			" ETA from history: ~%.0fs (p50 over %d starts, last %.0fs).",
			model, stats.P50S, stats.Count, stats.LastS), nil
	}
	return fmt.Sprintf("Warming %s through the front (JIT load started in the background)."+
		" No start history for this model yet — first recorded cold start sets the ETA.", model), nil
}

// modelInCatalog checks the model id against the front's /v1/models.
func (s *Server) modelInCatalog(ctx context.Context, frontURL, model string) (bool, error) {
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, strings.TrimRight(frontURL, "/")+"/v1/models", &wrap); err != nil {
		return false, err
	}
	for _, m := range wrap.Data {
		if m.ID == model {
			return true, nil
		}
	}
	return false, nil
}

// toolUnloadModel evicts a resident model via the cell's llama-swap unload
// API. Unknown cells fail loudly — a typo'd cell must not hit nothing.
func (s *Server) toolUnloadModel(ctx context.Context, cell, model string) (string, error) {
	c, ok := s.hosts.Cells[cell]
	if !ok {
		return "", fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("model is required")
	}
	url := fmt.Sprintf("%s/api/models/unload/%s", strings.TrimRight(c.URL, "/"), url.PathEscape(model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unload %s on %s: HTTP %d: %s", model, cell, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Sprintf("Unloaded %s on %s. The next request naming it JIT-loads again.", model, cell), nil
}

func (s *Server) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, http.StatusOK, jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
