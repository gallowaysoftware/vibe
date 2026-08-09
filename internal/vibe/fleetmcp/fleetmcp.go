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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	frontExtras string
	// backendsDir and llamaBinary feed the renderer the same inputs the
	// CLI gives it.
	backendsDir string
	llamaBinary string
	// tokenWarnOnce keeps the $VIBE_TOKEN-vs-token_file precedence note to
	// one line per process instead of one per actuation.
	tokenWarnOnce sync.Once
}

// Options carries the render_front tool's renderer inputs (the daemon's
// own config values).
type Options struct {
	FrontConfig string
	// FrontExtras is fleet.front_extras — the operator-owned YAML the
	// render LOOP merges into the front's config (C15). render_front
	// answers "what would the render write", so it must merge the same
	// file: with the CLI's default instead, the dry run reports the
	// front's `apiKeys:` as drift to be deleted on a fleet whose render
	// loop preserves them.
	FrontExtras string
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
		frontExtras: opts.FrontExtras,
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

// toolEffect is what a tool DOES to the fleet. It is declared BESIDE the
// tool, in the same table as its name, description and schema, for the
// reason fleetapi's route table declares Access beside the mount: two
// lists that must agree are two lists that will not. The protocol's
// `annotations` are derived from this one field — nothing anywhere
// re-states which tools are read-only.
//
// There is deliberately no safe zero value, exactly as with
// fleetapi.Access. A tool added without a decision is effectUndeclared,
// and TestTools_EveryToolDeclaresAnEffect fails on it: "the next agent
// forgot" must not be spelled the same way as "the next agent decided
// this changes nothing".
type toolEffect int

const (
	// effectUndeclared is the zero value and is never a valid declaration.
	effectUndeclared toolEffect = iota
	// effectRead changes nothing on any cell, in any store, or on disk.
	// This is the claim fleet_doctor makes in prose ("This tool CHANGES
	// NOTHING"); declaring it here is what makes it machine-readable.
	effectRead
	// effectMutate changes fleet state additively or reversibly: it
	// records something, declares something, or asks for a model to be
	// loaded. Re-running it cannot lose work that existed before it ran.
	effectMutate
	// effectDestructive can destroy something that re-running the tool
	// cannot recreate: an in-flight generation, a box's availability, a
	// stored measurement baseline.
	effectDestructive
)

// annotations renders the tool-annotation object of MCP
// mcpProtocolVersion. Only the two hints this facade can honestly derive
// are emitted, plus openWorldHint, which is a property of the fleet
// rather than of any one tool. idempotentHint is deliberately ABSENT:
// nothing in this table records whether re-running a verb is a no-op,
// and a hint guessed from the effect class would be exactly the
// confident-answer-from-absent-evidence this plan keeps refusing.
func (e toolEffect) annotations() map[string]any {
	return map[string]any{
		"readOnlyHint":    e == effectRead,
		"destructiveHint": e == effectDestructive,
		// The fleet is an open world by construction: cells come and go,
		// llama-swap owns residency, and a human at the box outranks every
		// verb here. No tool in this facade operates on a closed sandbox.
		"openWorldHint": true,
	}
}

// toolDef is one advertised tool. It is the single authority: tools/list
// renders from it, the effect annotation derives from it, and
// TestTools_AdvertisedAndDispatchedAgree checks callTool's switch against
// it.
type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	Effect      toolEffect
}

// wire renders one tool the way tools/list must return it.
func (d toolDef) wire() any {
	return map[string]any{
		"name":        d.Name,
		"description": d.Description,
		"inputSchema": d.InputSchema,
		"annotations": d.Effect.annotations(),
	}
}

// toolDefs is every tool this facade advertises, from the three files
// that own them.
func (s *Server) toolDefs() []toolDef {
	defs := s.coreTools()
	defs = append(defs, holdTools()...)
	defs = append(defs, sleepTools()...)
	return defs
}

func (s *Server) mcpTools() []any {
	defs := s.toolDefs()
	out := make([]any, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.wire())
	}
	return out
}

func (s *Server) coreTools() []toolDef {
	return []toolDef{
		{
			Effect: effectRead,
			Name:   "fleet_status",
			Description: "Fleet-wide state: one derived row per cell (SERVING / DRAINED / " +
				"STOPPED / DRAINED? / OFF / OFF/AWAY / OFF/AWAY? / INCONSISTENT) with resident " +
				"models, declared intent (why a cell is drained), last-seen for absent cells, and " +
				"per-model cold-start ETA history. DRAINED means somebody chose it and said why; " +
				"STOPPED means the cell's own unit recorded that it stopped and nothing recorded " +
				"why (a clean stop and a crash look identical); DRAINED? means nothing is " +
				"recorded at all. None of the three is a reason to act — they are for humans. " +
				"Answer 'what is every cell doing, and why' with this.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			// Additive: it asks the front to JIT-load a model. Nothing that
			// existed before the call is lost by it.
			Effect: effectMutate,
			Name:   "warm_model",
			Description: "Warm (JIT-load) a chat model by sending a 1-token request through " +
				"the fleet front — JIT IS the start verb. Returns immediately with the " +
				"cold-start ETA from history; the model loads in the background. " +
				"Embed/rerank-class models are rejected: warming those means their pinned " +
				"cell is misconfigured.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"model": map[string]any{"type": "string", "description": "Model id as listed in the front catalog."},
				},
				"required": []string{"model"},
			},
		},
		{
			// Destructive on purpose, and NOT because the model is hard to
			// get back (the next request JIT-loads it). llama-swap's unload
			// stops the upstream llama-server process: anything generating
			// on that model when the call lands dies with it, exactly as
			// drain_cell's SIGTERM does. The tool's own description says
			// "without stopping anything", which is about the CELL, and an
			// agent must not read it as a promise about a stream.
			Effect: effectDestructive,
			Name:   "unload_model",
			Description: "Evict a resident model from a cell (llama-swap unload API). The " +
				"next request for it JIT-loads again. Use to force a VRAM reshuffle without " +
				"stopping anything.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":  map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
					"model": map[string]any{"type": "string", "description": "Model id to evict."},
				},
				"required": []string{"cell", "model"},
			},
		},
		{
			// A measurement — but `rebaseline: true` DELETES that model's
			// stored baseline, and no amount of re-running the tool brings
			// the old one back. The annotation describes what the tool may
			// do, not what the common call does.
			Effect: effectDestructive,
			Name:   "probe_model",
			Description: "Measure one model's throughput on its cell and score it against that " +
				"model's own rolling baseline — the answer to \"is this thing slow right now?\", " +
				"which fleet_status cannot give you (llama-server degrades 10-100x while /health " +
				"stays green). Probes ONLY a model the cell already holds resident: a probe never " +
				"loads a model, so a cold one is refused with a note to warm_model first. It is " +
				"also refused while the cell is busy, leased or drained — a probe spends real GPU " +
				"time. The measurement runs on the cell, so the result lands about two heartbeats " +
				"later; read it back with fleet_status. A degraded verdict changes nothing on its " +
				"own (the catalog, the render and the intent are untouched) — the remediation verb " +
				"is unload_model, then probe again.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":       map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
					"model":      map[string]any{"type": "string", "description": "Model id as the CELL announces it (the canonical def name)."},
					"rebaseline": map[string]any{"type": "boolean", "description": "Clear this model's stored baseline first. Use ONLY when the model is legitimately slower now (new build, deliberate flag change) and the old baseline is what is wrong."},
				},
				"required": []string{"cell", "model"},
			},
		},
		{
			// The description already says it: llama-swap's SIGTERM cancels
			// in-flight streams. This is that sentence, as a flag.
			Effect: effectDestructive,
			Name:   "drain_cell",
			Description: "Reclaim a cell (stop its serving stack). llama-swap's SIGTERM " +
				"CANCELS in-flight streams immediately — a drain without wait_seconds " +
				"truncates whatever is generating, so never tell an operator their stream " +
				"is safe. Returns the pre-drain report — resident models, in-flight count, " +
				"active leases, and whether a requested wait actually happened — so you can " +
				"relay \"heads up: X holds a lease\" before confirming. Records intent " +
				"(reason/eta) at fleetd after the drain succeeds.",
			InputSchema: map[string]any{
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
		{
			Effect: effectMutate,
			Name:   "resume_cell",
			Description: "Resume a drained cell. Models return by JIT on next request; " +
				"clears the cell's recorded intent.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell": map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
				},
				"required": []string{"cell"},
			},
		},
		{
			Effect: effectMutate,
			Name:   "wake_cell",
			Description: "Send a Wake-on-LAN packet to a cell. Always explicit — never " +
				"triggered by a request. Only works for cells with wake: config in hosts.yaml.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell": map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
				},
				"required": []string{"cell"},
			},
		},
		{
			Effect: effectRead,
			Name:   "fleet_usage",
			Description: "Token usage per cell, per model, per day, from the fleet's own " +
				"llama-swap activity logs. RAW COUNTS ONLY — this tool knows no prices and " +
				"returns no dollars. Prompt tokens are split into in_fresh (actually processed) " +
				"and in_cached (served from KV cache) because they are not worth the same. " +
				"Fleet self-traffic (warm pokes) is counted separately in poke_req and excluded " +
				"from every token sum; 200s that reported no tokens are counted in " +
				"unmeasured_req and are never summed as zero.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"days": map[string]any{"type": "integer", "description": "How many days back to include (default 30, 0 = everything)."},
				},
			},
		},
		{
			Effect: effectRead,
			Name:   "fleet_savings",
			Description: "What the fleet did NOT spend, priced from the usage ledger: tokens per cell " +
				"priced against the SAME open-weight model rented from a real host (median across hosts, " +
				"rendered as a range), minus declared-wattage electricity, against each cell's declared " +
				"capital cost. Includes actual cloud spend in the same window beside it. Every figure is " +
				"an upper bound and the payload carries the caveat that says why — quote the caveat with " +
				"the number, never the number alone. Unpriced models keep their tokens and leave the " +
				"money column.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"window": map[string]any{"type": "string", "description": "Window: \"7d\", \"30d\" (default) or \"all\"."},
				},
			},
		},
		{
			// "This tool CHANGES NOTHING" is in the description below. This
			// is the same claim in the field a client can act on without
			// reading English.
			Effect: effectRead,
			Name:   "fleet_doctor",
			Description: "Is the fleet still put together correctly? A READ-ONLY audit that runs " +
				"every 'is it still wired up' check at once: both-direction token auth per cell, " +
				"def-SHA parity, the llama-swap version matrix, TLS expiry, disk headroom, wake " +
				"configuration, whether each roaming cell's announcer is actually running, plus " +
				"intent, lease, fingerprint, probe, warm, ledger and notification hygiene. Each " +
				"check reports ok / warn / fail / UNKNOWN, and unknown means the check could not " +
				"be evaluated — it never means fine. This tool CHANGES NOTHING: it drains, warms, " +
				"unloads, probes and renders nothing, and is safe to call mid-incident. Fixes are " +
				"the operator's, through the other tools.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			// It gates delivery and nothing else — it drains nothing and
			// changes no cell's state — but it is a declaration this daemon
			// persists, so it is not a read.
			Effect: effectMutate,
			Name:   "fleet_notify_scope",
			Description: "Declare whether fleet ALARM notifications should be delivered right " +
				"now: \"away\" (vacation — alarms still fire and stay visible in fleet_status, " +
				"but delivery is withheld and coming home sends one digest naming what was " +
				"missed) or \"home\". This gates notifications only: it drains nothing, changes " +
				"no cell's state, and is never consulted for routing. Prefer setting `until` so " +
				"the fleet un-mutes itself.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"scope":  map[string]any{"type": "string", "description": "\"away\" or \"home\"."},
					"reason": map[string]any{"type": "string", "description": "Why (e.g. \"vacation\")."},
					"until":  map[string]any{"type": "string", "description": "When away ends: an RFC3339 instant or a Go duration from now (e.g. \"336h\"). Away only."},
				},
				"required": []string{"scope"},
			},
		},
		{
			// It sends a message to a human's pager. That is a side effect
			// on the world, so it is not read-only however little it touches
			// the fleet.
			Effect: effectMutate,
			Name:   "fleet_notify_test",
			Description: "Send a test notification through the configured webhook NOW. It is " +
				"not an alarm: it skips the dwell, the dedup and the away gate, so it is also " +
				"how you check the pager still works while away. An alerting path nobody has " +
				"ever sent a message through is not an alerting path.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{"type": "string", "description": "Optional message body."},
				},
			},
		},
		{
			// DRY-RUN ONLY, enforced in toolRenderFront: dry_run: false is
			// refused rather than honoured. It writes nothing.
			Effect: effectRead,
			Name:   "render_front",
			Description: "Dry-run the front config render (vibe router render --cell front): " +
				"renders the peers-only config from backend defs + hosts.yaml and returns the " +
				"unified diff against the live front config (or the full render when no live " +
				"path is mounted). DRY-RUN ONLY — fleetd's presence-driven render loop owns the " +
				"write path; a second writer to the same -watch-config file breaks its " +
				"atomic-write contract.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dry_run": map[string]any{"type": "boolean", "description": "Must be true (dry-run is the only mode)."},
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
	case "suspend_cell":
		// A unit stop plus a logind call, same shape as resume_cell — and
		// the same reason the budget is not the 10s probe one.
		timeout = 90 * time.Second
	case "fleet_doctor":
		// A read, but a wide one: the state snapshot's per-cell probes,
		// one credential call per cell and one TLS dial per https
		// endpoint, each individually bounded. 10s is the budget for ONE
		// probe round.
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch name {
	case "fleet_status":
		return s.toolFleetStatus(ctx)
	case "fleet_doctor":
		return s.toolFleetDoctor(ctx)
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
	case "probe_model":
		var args struct {
			Cell       string `json:"cell"`
			Model      string `json:"model"`
			Rebaseline bool   `json:"rebaseline"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolProbeModel(args.Cell, args.Model, args.Rebaseline)
	case "hold_model":
		var args struct {
			Cell  string `json:"cell"`
			Model string `json:"model"`
			For   string `json:"for"`
			Note  string `json:"note"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolHoldModel(args.Cell, args.Model, args.For, args.Note)
	case "release_hold":
		var args struct {
			Cell  string `json:"cell"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolReleaseHold(args.Cell, args.Model)
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
	case "suspend_cell":
		var args struct {
			Cell   string `json:"cell"`
			Reason string `json:"reason"`
			Force  bool   `json:"force"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolSuspendCell(ctx, args.Cell, args.Reason, args.Force)
	case "fleet_usage":
		var args struct {
			Days *int `json:"days"`
		}
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %v", err)
			}
		}
		return s.toolFleetUsage(args.Days)
	case "fleet_savings":
		var args struct {
			Window string `json:"window"`
		}
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %v", err)
			}
		}
		return s.toolFleetSavings(ctx, args.Window)
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
	case "fleet_notify_scope":
		var args struct {
			Scope  string `json:"scope"`
			Reason string `json:"reason"`
			Until  string `json:"until"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		return s.toolNotifyScope(args.Scope, args.Reason, args.Until)
	case "fleet_notify_test":
		var args struct {
			Message string `json:"message"`
		}
		if len(rawArgs) > 0 {
			if err := json.Unmarshal(rawArgs, &args); err != nil {
				return "", fmt.Errorf("invalid arguments: %v", err)
			}
		}
		return s.toolNotifyTest(args.Message)
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

// defaultUsageDays bounds the default window so an agent's first call
// doesn't paste two years of buckets into its context.
const defaultUsageDays = 30

// maxUsageDays is longer than any ledger this will ever hold, and past it
// "how many days" stops meaning anything. It is a guard, not a policy:
// AddDate on an absurd count OVERFLOWS time.Time and wraps into the
// FUTURE (days=1<<62 lands on tomorrow), which filters every bucket out
// and reads as "the fleet used nothing" instead of "everything".
const maxUsageDays = 36600

// toolFleetUsage returns the raw ledger. It deliberately computes
// nothing: no totals, no rates, no money. C7b prices these counts, and
// keeping the pricing out of C7a is what lets the whole history be
// re-priced when the price table changes.
func (s *Server) toolFleetUsage(days *int) (string, error) {
	n := defaultUsageDays
	if days != nil {
		n = *days
	}
	if n < 0 {
		return "", fmt.Errorf("days must be >= 0 (0 = everything)")
	}
	if n > maxUsageDays {
		n = 0
	}
	since := ""
	if n > 0 {
		// The window is computed in the LEDGER's zone, not the process
		// zone: asking for "the last 7 days" in UTC against Toronto
		// buckets silently clips or pads the edge day.
		y, m, d := time.Now().In(s.fleet.UsageTZ()).AddDate(0, 0, -(n - 1)).Date()
		since = fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
	}
	data, err := json.Marshal(s.fleet.UsageReport(since))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// toolFleetSavings serves the identical document GET
// /api/fleet/savings serves, so an agent and the page can never disagree
// about the money. The caveat travels IN the payload for the same
// reason: an agent quoting the headline without it is the failure mode
// this whole phase exists to avoid.
func (s *Server) toolFleetSavings(ctx context.Context, window string) (string, error) {
	rep, err := s.fleet.Savings(ctx, window)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(rep)
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
	// The guard exists to refuse firing a CHAT completion at an
	// embed/rerank id. A chat-class entry documents ownership and must
	// not be caught by it — the old blanket refusal told the caller a
	// chat model "is not chat". The sentence lives in fleetcfg because
	// the automated warm paths need the identical one.
	if why := s.hosts.WarmClassRefusal(model); why != "" {
		return "", errors.New(why)
	}
	front, ok := s.hosts.Cells[fleetcfg.FrontCell]
	if !ok {
		return "", fmt.Errorf("no %q cell in hosts.yaml", fleetcfg.FrontCell)
	}
	// The catalog read is the operator-facing surface for a broken front
	// credential (C15): it is the one part of this verb that runs
	// synchronously, so a 401 here is the answer the agent gets back
	// instead of a warm that vanishes into a goroutine. Deliberately NOT
	// gated by SwapAuthRefusal — an operator asking is not fleetd
	// guessing, and the operator is the one person who can read the
	// diagnosis and go fix the file.
	known, err := s.modelInCatalog(ctx, front.URL, model)
	if err != nil {
		if why, blocked := s.fleet.SwapAuthRefusal(fleetcfg.FrontCell); blocked {
			return "", fmt.Errorf("check front catalog: %v — %s", err, why)
		}
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
		if err := s.fleet.AuthorizeSwap(req, fleetcfg.FrontCell); err != nil {
			slog.Error("warm_model not sent: the front's llama-swap credential will not resolve", "model", model, "err", credentialDetail(err))
			return
		}
		resp, err := s.warmHTTP.Do(req)
		if err != nil {
			slog.Warn("warm_model request failed", "model", model, "err", err)
			return
		}
		s.fleet.NoteSwapStatus(fleetcfg.FrontCell, resp.StatusCode)
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
	if err := s.getJSON(ctx, fleetcfg.FrontCell, strings.TrimRight(frontURL, "/")+"/v1/models", &wrap); err != nil {
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
	// The admin port is gated by the cell's own apiKeys, not the front's
	// (C15): /api/models/unload/* answers 401 without a key on v239.
	// A credential that will not resolve is a local failure and is NOT
	// queued — the cell would execute the verb happily, so hiding a broken
	// key file behind "queued for the next announce" would let the
	// misconfiguration survive indefinitely.
	if err := s.fleet.AuthorizeSwap(req, cell); err != nil {
		return "", fmt.Errorf("unload %s on %s: %v", model, cell, credentialDetail(err))
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return s.queueUnload(cell, model, fmt.Sprintf("POST %s: %v", url, err))
	}
	defer resp.Body.Close()
	s.fleet.NoteSwapStatus(cell, resp.StatusCode)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 500 {
		return s.queueUnload(cell, model, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	if resp.StatusCode != http.StatusOK {
		// A 4xx is the admin API ANSWERING (no such model, bad request).
		// Queueing it would tell the agent "done on its next heartbeat"
		// about a verb the cell will refuse identically — the piggyback
		// fallback is for delivery failures, not for definitive answers.
		msg := fmt.Sprintf("unload %s on %s: HTTP %d: %s", model, cell, resp.StatusCode, strings.TrimSpace(string(body)))
		if why, blocked := s.fleet.SwapAuthRefusal(cell); blocked {
			// llama-swap's 401 body says "invalid or missing API key" and
			// names no config; this names the one to fix.
			msg += " — " + why
		}
		return "", errors.New(msg)
	}
	return fmt.Sprintf("Unloaded %s on %s. The next request naming it JIT-loads again.", model, cell), nil
}

// queueUnload is the piggyback fallback: after C3 a cell fleetd cannot
// reach directly still collects verbs on its next heartbeat, so an
// unreachable llama-swap admin port is a DELAY, not a failure. It stays
// an error when the cell doesn't announce — nothing would ever collect
// the command, and pretending otherwise is worse than failing.
func (s *Server) queueUnload(cell, model, why string) (string, error) {
	if qerr := s.fleet.QueueCommand(cell, fleetapi.AnnounceCommand{Verb: "unload", Model: model}); qerr != nil {
		return "", fmt.Errorf("unload %s on %s: %s (piggyback also unavailable: %v)", model, cell, why, qerr)
	}
	return fmt.Sprintf("%s's admin port did not answer (%s), so the unload of %s is queued for its next announce (≤ one heartbeat).", cell, why, model), nil
}

// getJSON reads one llama-swap JSON endpoint on a named cell, carrying
// that cell's llama-swap credential (C15). The cell name is a parameter
// for the same reason fleetapi's is: it selects the key, and every
// fleetd→llama-swap call in this repo goes through one authorizer.
// credentialDetail is the token-only rendering of a swap-credential
// failure. fleetapi's error deliberately names only the hosts.yaml key,
// because it propagates into `/api/fleet/state`, which C12 grants to the
// guest bearer; /mcp and this daemon's log are token-only, so they get
// the wrapped cause and with it the PATH an operator has to go fix.
func credentialDetail(err error) error {
	var ske *fleetcfg.SwapKeyError
	if errors.As(err, &ske) {
		return ske
	}
	return err
}

func (s *Server) getJSON(ctx context.Context, cell, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if err := s.fleet.AuthorizeSwap(req, cell); err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	s.fleet.NoteSwapStatus(cell, resp.StatusCode)
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
