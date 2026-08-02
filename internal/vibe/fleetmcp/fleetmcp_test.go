package fleetmcp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// fakeLlamaSwap is the fleetmcp-facing slice of a llama-swap cell:
// /v1/models catalog and the unload API.
type fakeLlamaSwap struct {
	models   []string
	unloaded atomic.Value // last unloaded model id
	srv      *httptest.Server
}

func newFakeLlamaSwap(t *testing.T, models ...string) *fakeLlamaSwap {
	t.Helper()
	f := &fakeLlamaSwap{models: models}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]string
		for _, id := range f.models {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("POST /api/models/unload/", func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(f.srv.Close)
	return f
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
		fleetapi.Options{IntentPath: dir + "/intent.json", LastSeenPath: dir + "/last-seen.json"})
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
	if !ok || len(tools) != 7 {
		t.Fatalf("tools = %v", list.Result)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"fleet_status", "warm_model", "unload_model", "drain_cell", "resume_cell", "wake_cell", "render_front"} {
		if !names[want] {
			t.Errorf("missing tool %s in %v", want, names)
		}
	}
}

func TestMCPFleetStatus(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
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
	frontSrv := newFakeLlamaSwap(t, "qwen3.6-27b", "bge-embed")
	var warmHits atomic.Int32
	mux := http.NewServeMux()
	frontSrv.srv.Config.Handler = mux
	// Re-register the catalog handler plus the warm endpoint on the same mux.
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]string{
			{"id": "qwen3.6-27b", "object": "model"}, {"id": "bge-embed", "object": "model"},
		}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		warmHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "warm", "object": "chat.completion"})
	})
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front": {URL: frontSrv.srv.URL, Class: fleetcfg.ClassAlwaysOn},
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
	for warmHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if warmHits.Load() == 0 {
		t.Error("warm request never reached the front")
	}

	// Embed-class: typed rejection naming the class, no request fired.
	resp = rpc(t, ts, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"warm_model","arguments":{"model":"bge-embed"}}}`)
	text, isErr = toolText(t, resp)
	if !isErr || !strings.Contains(text, "embed-class") {
		t.Errorf("embed rejection: isErr=%v text=%q", isErr, text)
	}
	if got := warmHits.Load(); got != 1 {
		t.Errorf("embed warm fired a request (hits=%d)", got)
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
