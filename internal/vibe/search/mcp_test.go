package search

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mcpCall(t *testing.T, base, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func mcpTestServer(t *testing.T, expose bool) *httptest.Server {
	t.Helper()
	return newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{
			Answer:  "an answer",
			Results: []Result{{URL: "https://example.com/a", Title: "A", Content: "snippet"}},
		}},
		Fetcher:  &stubFetcher{name: "direct", doc: &Document{Title: "Doc", Text: strings.Repeat("prose ", 200)}},
		Escalate: &stubFetcher{name: "tavily", doc: &Document{Title: "Rendered", Text: strings.Repeat("js ", 300)}},
		MCP:      MCPOptions{ExposeSearch: expose},
	})
}

func TestMCPInitializeEchoesClientProtocolVersion(t *testing.T) {
	ts := mcpTestServer(t, false)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize",
		"params":{"protocolVersion":"2025-03-26","capabilities":{}}}`)

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", out)
	}
	// Answering with our own version when the client asked for one it speaks
	// makes some clients abort the handshake.
	if got := result["protocolVersion"]; got != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want the client's 2025-03-26", got)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities missing tools: %v", result["capabilities"])
	}
}

// A notification has no id and must produce no response body at all.
func TestMCPNotificationGetsNoBody(t *testing.T) {
	ts := mcpTestServer(t, false)
	resp, err := http.Post(ts.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %s, want 202", resp.Status)
	}
}

func TestMCPToolsListHidesSearchUnlessExposed(t *testing.T) {
	ts := mcpTestServer(t, false)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want only fetch_url", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] != "fetch_url" {
		t.Errorf("tool = %v, want fetch_url", first["name"])
	}
	// The schema is what the model reads to call the tool correctly.
	schema := first["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	if _, ok := props["url"]; !ok {
		t.Errorf("inputSchema missing url property: %v", schema)
	}

	exposed := mcpTestServer(t, true)
	out2 := mcpCall(t, exposed.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if got := len(out2["result"].(map[string]any)["tools"].([]any)); got != 2 {
		t.Errorf("tools with ExposeSearch = %d, want 2", got)
	}
}

func TestMCPFetchURLReturnsTextContent(t *testing.T) {
	ts := mcpTestServer(t, false)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":4,"method":"tools/call",
		"params":{"name":"fetch_url","arguments":{"url":"https://example.com/a"}}}`)

	result := out["result"].(map[string]any)
	if result["isError"] == true {
		t.Fatalf("unexpected error result: %v", result)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "# Doc") || !strings.Contains(text, "prose") {
		t.Errorf("text = %q, want title heading and body", truncate(text))
	}
	// The source URL has to survive: a model citing this needs it.
	if !strings.Contains(text, "https://example.com/a") {
		t.Errorf("text missing source url: %q", truncate(text))
	}
}

// Tool failures must come back as a RESULT with isError so the model sees the
// message and can react; a JSON-RPC error would hide it.
func TestMCPToolFailureIsResultNotProtocolError(t *testing.T) {
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  &stubFetcher{name: "direct", err: errTest},
	})
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":5,"method":"tools/call",
		"params":{"name":"fetch_url","arguments":{"url":"https://example.com/x"}}}`)

	if _, isRPCError := out["error"]; isRPCError {
		t.Fatalf("got a JSON-RPC error, want a result with isError: %v", out)
	}
	result := out["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("isError = %v, want true", result["isError"])
	}
}

func TestMCPMissingURLIsReported(t *testing.T) {
	ts := mcpTestServer(t, false)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":6,"method":"tools/call",
		"params":{"name":"fetch_url","arguments":{}}}`)
	if out["result"].(map[string]any)["isError"] != true {
		t.Errorf("want isError for a missing url, got %v", out["result"])
	}
}

func TestMCPUnknownMethodIsRPCError(t *testing.T) {
	ts := mcpTestServer(t, false)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":7,"method":"nope"}`)
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("want a JSON-RPC error, got %v", out)
	}
	if int(e["code"].(float64)) != rpcMethodNotFound {
		t.Errorf("code = %v, want %d", e["code"], rpcMethodNotFound)
	}
}

// web_search must stay unreachable when it isn't advertised, or a client could
// call a tool that tools/list says does not exist.
func TestMCPHiddenSearchToolIsNotCallable(t *testing.T) {
	ts := mcpTestServer(t, false)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":8,"method":"tools/call",
		"params":{"name":"web_search","arguments":{"query":"x"}}}`)
	if out["result"].(map[string]any)["isError"] != true {
		t.Errorf("want isError calling an unadvertised tool, got %v", out["result"])
	}
}

func TestMCPSearchToolFormatsResults(t *testing.T) {
	ts := mcpTestServer(t, true)
	out := mcpCall(t, ts.URL, `{"jsonrpc":"2.0","id":9,"method":"tools/call",
		"params":{"name":"web_search","arguments":{"query":"x","limit":3}}}`)
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"an answer", "1. A", "https://example.com/a", "snippet"} {
		if !strings.Contains(text, want) {
			t.Errorf("formatted results missing %q; got %q", want, truncate(text))
		}
	}
}

func TestMCPBatchRequestsAreAnswered(t *testing.T) {
	ts := mcpTestServer(t, false)
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("batch responses = %d, want 2", len(out))
	}
}

func TestMCPRespectsToken(t *testing.T) {
	srv := &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  &stubFetcher{name: "direct", doc: &Document{Text: "x"}},
		Token:    "s3cret",
	}
	ts := newTestServer(t, srv)
	resp, err := http.Post(ts.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %s, want 401 — the MCP surface spends the same quota", resp.Status)
	}
}

var errTest = &testError{"upstream exploded"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func truncate(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
