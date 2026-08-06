package fleetmcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// fakeLlamaSwap is the llama-swap cell every test in this package points a
// fleetcfg.Cell at. It is internal/swaptest's wire-versioned double for
// every endpoint the double models, plus the ONE route it does not —
// POST /api/models/unload/{model}, the admin endpoint unload_model calls.
//
// What the hand-rolled predecessor got wrong, and what each one could hide:
//
//   - It served /v1/models and 404'd /running. fleetapi's snapshotCell
//     reads a failed /running as "this is a llama.cpp-family cell, not a
//     llama-swap" and switches to the ollama-passthrough branch — so every
//     fleet_status assertion in this package was describing the FALLBACK
//     path, and a regression in the /running ⋈ /v1/models merge could not
//     fail any test here.
//   - Its catalog rows were {"id","object"} only. Real rows carry created
//     and owned_by; a decoder that grew a dependency on either would pass.
//   - It served no /v1/chat/completions, so warm_model's fire-and-forget
//     completion landed on a 404 the test could not see. It now lands on a
//     cell that routes it and logs an activity row.
//
// The unload route is stitched in front of the double with a reverse proxy
// rather than kept as a second fake, so there is exactly one base URL and
// exactly one catalog.
type fakeLlamaSwap struct {
	cell     *swaptest.Cell
	models   []string
	unloaded atomic.Value // last unloaded model id
	srv      *httptest.Server
}

func newFakeLlamaSwap(t *testing.T, models ...string) *fakeLlamaSwap {
	t.Helper()
	catalog := make([]swaptest.Model, 0, len(models))
	for _, id := range models {
		catalog = append(catalog, swaptest.Model{ID: id, State: "ready", TTL: 1800})
	}
	f := &fakeLlamaSwap{models: models, cell: swaptest.NewCell(t, swaptest.WithModels(catalog...))}

	upstream, err := url.Parse(f.cell.URL())
	if err != nil {
		t.Fatalf("swaptest cell URL %q: %v", f.cell.URL(), err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// The default logs to stderr and answers a bare 502; a test that
		// reaches this should be told which half broke.
		http.Error(w, "fakeLlamaSwap: reaching the swaptest cell: "+err.Error(), http.StatusBadGateway)
	}

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	mux.HandleFunc("POST /api/models/unload/", func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Path is already decoded, so this is the id production sent
		// through url.PathEscape.
		id := strings.TrimPrefix(r.URL.Path, "/api/models/unload/")
		found := false
		for _, m := range f.models {
			if m == id {
				found = true
			}
		}
		if !found {
			http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
			return
		}
		f.unloaded.Store(id)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		// Order matters. An open /api/events stream is an OUTSTANDING
		// request on this server and httptest.Server.Close blocks until it
		// ends; the cell's own cleanup — the thing that would end it — is
		// registered EARLIER and so runs LATER. Drop the streams first or a
		// watching test deadlocks in teardown.
		f.cell.DropStreams()
		f.srv.Close()
	})
	return f
}

// completions returns the activity rows this cell logged for chat
// completions — what llama-swap itself recorded, rather than a hand-rolled
// hit counter that cannot tell a routed request from any POST at all.
func (f *fakeLlamaSwap) completions() []swaptest.Row {
	var out []swaptest.Row
	for _, r := range f.cell.Rows() {
		if r.ReqPath == "/v1/chat/completions" {
			out = append(out, r)
		}
	}
	return out
}

func newTestFacade(t *testing.T, cells map[string]fleetcfg.Cell, classes map[string]string) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	var fleetCells []fleetapi.Cell
	for name, c := range cells {
		fleetCells = append(fleetCells, fleetapi.Cell{Name: name, URL: c.URL, Class: string(c.Class)})
	}
	fleet := fleetapi.New(fleetCells, dir+"/history.json",
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: dir + "/intent.json", LastSeenPath: dir + "/last-seen.json",
			LeasePath: dir + "/leases.json"})
	t.Cleanup(fleet.Close)
	hosts := &fleetcfg.File{Cells: cells, ModelClasses: classes}
	s := New(fleet, hosts, Options{})
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts
}

// rpc posts one JSON-RPC message and decodes the response.
func rpc(t *testing.T, ts *httptest.Server, body string) jsonRPCResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /mcp: HTTP %d: %s", resp.StatusCode, b)
	}
	var out jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode rpc: %v", err)
	}
	return out
}

func toolText(t *testing.T, resp jsonRPCResponse) (string, bool) {
	t.Helper()
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("no result in %+v", resp)
	}
	isErr, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content in %+v", result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text, isErr
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{}, nil)

	init := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	result, ok := init.Result.(map[string]any)
	if !ok {
		t.Fatalf("initialize result: %+v", init)
	}
	if result["serverInfo"].(map[string]any)["name"] != "vibe-fleet" {
		t.Errorf("serverInfo = %v", result["serverInfo"])
	}

	list := rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools, ok := list.Result.(map[string]any)["tools"].([]any)
	if !ok || len(tools) != 16 {
		t.Fatalf("tools = %v", list.Result)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"fleet_status", "warm_model", "unload_model", "drain_cell", "resume_cell", "wake_cell", "render_front", "fleet_usage", "fleet_savings", "probe_model", "hold_model", "release_hold", "fleet_notify_scope", "fleet_notify_test", "fleet_doctor", "suspend_cell"} {
		if !names[want] {
			t.Errorf("missing tool %s in %v", want, names)
		}
	}
}

func TestMCPFleetStatus(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
	// bge-embed is in the catalog but NOT running, which is the state that
	// makes the merge observable at all.
	cellSrv.cell.SetModelState("bge-embed", "stopped")
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: cellSrv.srv.URL, Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {URL: "http://" + deadAddr(t), Class: fleetcfg.ClassOpportunistic},
	}, nil)

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("fleet_status error: %s", text)
	}
	var snap fleetapi.StateSnapshot
	if err := json.Unmarshal([]byte(text), &snap); err != nil {
		t.Fatalf("fleet_status returned non-snapshot: %v\n%s", err, text)
	}
	if len(snap.Cells) != 2 {
		t.Fatalf("got %d cells, want 2", len(snap.Cells))
	}
	byName := map[string]fleetapi.CellSnapshot{}
	for _, c := range snap.Cells {
		byName[c.Name] = c
	}
	if byName["front"].Display != fleetapi.DisplayServing {
		t.Errorf("front display = %s", byName["front"].Display)
	}
	if byName["gpu-cell"].Display != fleetapi.DisplayOffAwayQ {
		t.Errorf("gpu-cell display = %s, want OFF/AWAY?", byName["gpu-cell"].Display)
	}
	// The /running ⋈ /v1/models merge, which the fake this file used to
	// stand up could not exercise: residency comes from /running, the full
	// catalog from /v1/models, and a model in the catalog but not running
	// is "stopped".
	state := map[string]string{}
	for _, m := range byName["front"].Models {
		state[m.ID] = m.State
	}
	if state["qwen3.6-27b"] != "ready" || state["bge-embed"] != "stopped" {
		t.Errorf("front models = %v, want qwen3.6-27b ready (from /running) and bge-embed stopped (catalog only)", state)
	}
}

func TestMCPUnloadModel(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"gpu-cell": {URL: cellSrv.srv.URL, Class: fleetcfg.ClassOpportunistic},
	}, nil)

	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"unload_model","arguments":{"cell":"gpu-cell","model":"qwen3.6-27b"}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("unload error: %s", text)
	}
	if cellSrv.unloaded.Load() != "qwen3.6-27b" {
		t.Errorf("cell saw unload for %v", cellSrv.unloaded.Load())
	}
	if !strings.Contains(text, "Unloaded qwen3.6-27b") {
		t.Errorf("text = %q", text)
	}

	// Unknown cell fails loudly.
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"unload_model","arguments":{"cell":"typo","model":"x"}}}`)
	text, isErr = toolText(t, resp)
	if !isErr || !strings.Contains(text, "unknown cell") {
		t.Errorf("unknown cell: isErr=%v text=%q", isErr, text)
	}
}

func TestMCPWarmModel(t *testing.T) {
	// One fake serves the catalog AND the completion, so the warm is
	// counted the way llama-swap counts it: as an activity ROW naming the
	// model and the path. The shape this replaced stood up a fake and then
	// reassigned httptest.Server.Config.Handler on the already-serving
	// server — a write racing the accept loop's reads — to bolt on a
	// completions handler whose hit counter could not tell a routed
	// completion for the right model from any POST at all.
	front := newFakeLlamaSwap(t, "qwen3.6-27b", "bge-embed")
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front": {URL: front.srv.URL, Class: fleetcfg.ClassAlwaysOn},
	}, map[string]string{"bge-embed": "embed"})

	// Chat model: warm fires through the front.
	resp := rpc(t, ts, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"warm_model","arguments":{"model":"qwen3.6-27b"}}}`)
	text, isErr := toolText(t, resp)
	if isErr {
		t.Fatalf("warm error: %s", text)
	}
	if !strings.Contains(text, "Warming qwen3.6-27b") {
		t.Errorf("text = %q", text)
	}
	// Fire-and-forget: the request lands shortly after the reply.
	deadline := time.Now().Add(3 * time.Second)
	for len(front.completions()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	rows := front.completions()
	if len(rows) == 0 {
		t.Fatal("warm request never reached the front")
	}
	// The row is what llama-swap logged, so it answers the question a hit
	// counter cannot: was this a completion the cell could ROUTE?
	if rows[0].Model != "qwen3.6-27b" || rows[0].RespStatusCode != http.StatusOK {
		t.Errorf("warm row = model %q status %d error %q, want a routed 200 for qwen3.6-27b",
			rows[0].Model, rows[0].RespStatusCode, rows[0].ErrorMsg)
	}

	// Embed-class: typed rejection naming the class, no request fired.
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"warm_model","arguments":{"model":"bge-embed"}}}`)
	text, isErr = toolText(t, resp)
	if !isErr || !strings.Contains(text, "embed-class") {
		t.Errorf("embed rejection: isErr=%v text=%q", isErr, text)
	}
	if got := len(front.completions()); got != 1 {
		t.Errorf("embed warm fired a request (completion rows=%d)", got)
	}

	// Unknown model: catalog check rejects.
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"warm_model","arguments":{"model":"no-such"}}}`)
	text, isErr = toolText(t, resp)
	if !isErr || !strings.Contains(text, "unknown model") {
		t.Errorf("unknown model: isErr=%v text=%q", isErr, text)
	}
}

// deadAddr returns an address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	// A RESERVED address, not a recycled ephemeral one. The old shape
	// (listen on :0, read the port, close) hands the port straight back
	// to the kernel, and a test binary that stands up dozens of
	// httptest servers can be given that exact port a few microseconds
	// later — at which point the "dead" cell answers 404 and the test
	// fails for a reason that has nothing to do with what it asserts.
	// Seen once in ~25 package runs of `go test -race ./...`, which is
	// precisely the failure rate that makes a green CI meaningless.
	// Port 1 is privileged: nothing in this binary can bind it, and a
	// loopback connect gets an immediate ECONNREFUSED. It is already the
	// idiom these tests use for an unreachable front.
	_ = t
	return "127.0.0.1:1"
}
