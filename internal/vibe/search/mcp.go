package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The MCP surface exists for one reason: page fetching has no redirectable
// endpoint in any mainstream harness. oh-my-pi's providers.fetch chooses among
// built-in readers whose hosts are compiled in, so unlike search — where
// speaking SearXNG is enough to route a client here — there is no way to point
// a harness's native fetch at this service. MCP is the only remaining hook.
//
// That makes this a deliberate, narrow exception to the rule that shared
// infrastructure does not belong in MCP (MCP being the harness's local,
// per-profile identity plane). Search is NOT exposed here by default for that
// reason — it has a native path worth keeping — but it is available for
// clients that have no redirectable search either.
//
// Transport is streamable HTTP: a single JSON-RPC request per POST, answered
// with a single JSON response. No SSE stream and no session state, because
// every tool here is a self-contained request/response with no server-initiated
// messages to deliver.

// mcpProtocolVersion is the spec revision this server implements. A client
// asking for a different revision is answered with its own version when we can
// serve it, since the wire shape used here (initialize / tools/list /
// tools/call) has been stable across revisions.
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
	rpcInternalError  = -32603
)

// MCPOptions tunes the MCP surface.
type MCPOptions struct {
	// ExposeSearch adds a web_search tool. Off by default: a harness with a
	// redirectable search endpoint should use its native path, which renders
	// results properly and participates in its own provider chain.
	ExposeSearch bool
}

// mcpHandler serves the MCP endpoint. It is a method set over Server so the
// tools reuse exactly the same tiered fetch the HTTP surface uses — two code
// paths that could disagree about escalation would be a bug factory.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET on the MCP endpoint is how a client opens a server-initiated SSE
		// stream. This server never initiates messages, so declining is
		// correct and clients treat it as "no stream available".
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPCError(w, nil, rpcParseError, "read body: "+err.Error())
		return
	}
	// Batches are legal JSON-RPC. Tools here are independent, so handling each
	// in turn and returning an array is both correct and simple.
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
		// A notification (no id) gets no body — 202 is what the streamable
		// HTTP transport expects here.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// dispatchRPC routes one JSON-RPC message. The bool reports whether a response
// should be sent: notifications are answered with nothing at all.
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
			"serverInfo":      map[string]any{"name": "vibe-search", "version": "1"},
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
			// Tool failures are reported as a RESULT with isError, not as a
			// JSON-RPC error: the model is supposed to see the failure text and
			// decide what to do, and a protocol-level error hides it.
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
	tools := []any{
		map[string]any{
			"name": "fetch_url",
			"description": "Fetch a web page and return its readable text content, with " +
				"navigation and markup stripped. Handles JavaScript-rendered pages: if the " +
				"page returns an unrendered shell, it is automatically re-fetched through a " +
				"full extraction service. Use this whenever you need the contents of a page " +
				"rather than just a search snippet.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "Absolute http(s) URL of the page to fetch.",
					},
				},
				"required": []string{"url"},
			},
		},
	}
	if s.MCP.ExposeSearch {
		tools = append(tools, map[string]any{
			"name": "web_search",
			"description": "Search the web and return ranked results with titles, URLs, and " +
				"snippets. Follow up with fetch_url when a snippet is not enough.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query."},
					"limit": map[string]any{"type": "integer", "description": "Maximum results to return."},
				},
				"required": []string{"query"},
			},
		})
	}
	return tools
}

func (s *Server) callTool(ctx context.Context, name string, rawArgs json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	switch name {
	case "fetch_url":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		if strings.TrimSpace(args.URL) == "" {
			return "", fmt.Errorf("url is required")
		}
		doc, meta, err := s.fetchTiered(ctx, args.URL)
		if err != nil {
			return "", fmt.Errorf("could not fetch %s: %v", args.URL, err)
		}
		s.logger().Info("mcp fetch_url", "url", args.URL, "extractor", meta.Extractor,
			"escalated", meta.Escalated, "chars", len(doc.Text))
		var b strings.Builder
		if doc.Title != "" {
			fmt.Fprintf(&b, "# %s\n\n", doc.Title)
		}
		fmt.Fprintf(&b, "Source: %s\n\n%s", doc.URL, doc.Text)
		// Tell the model when the content is untrustworthy. Without this it
		// sees a short string that reads like the whole page and will happily
		// summarize a navigation stub as if it were the article.
		if meta.EscalationErr != "" {
			fmt.Fprintf(&b, "\n\n[warning: this page could not be fully extracted "+
				"(%s). The text above is likely incomplete — it may be a "+
				"JavaScript shell or a blocked request rather than the real content.]",
				meta.EscalationErr)
		}
		return b.String(), nil

	case "web_search":
		if !s.MCP.ExposeSearch {
			return "", fmt.Errorf("unknown tool %q", name)
		}
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %v", err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("query is required")
		}
		resp, err := s.Provider.Search(ctx, Query{Text: args.Query, Limit: args.Limit})
		if err != nil {
			return "", fmt.Errorf("search failed: %v", err)
		}
		return formatResultsForModel(args.Query, resp), nil

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

// formatResultsForModel renders results as compact markdown. Numbered entries
// with the URL on its own line, because a model asked to "open the second
// result" needs to be able to lift the URL unambiguously.
func formatResultsForModel(query string, resp *Response) string {
	var b strings.Builder
	if resp.Answer != "" {
		fmt.Fprintf(&b, "%s\n\n", resp.Answer)
	}
	if len(resp.Results) == 0 {
		fmt.Fprintf(&b, "No results for %q.", query)
		return b.String()
	}
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Content != "" {
			fmt.Fprintf(&b, "   %s\n", r.Content)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	// HTTP 200 with a JSON-RPC error body: the transport succeeded, the call
	// did not. Clients parse the envelope, not the status line.
	writeJSON(w, http.StatusOK, jsonRPCResponse{
		JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: msg},
	})
}
