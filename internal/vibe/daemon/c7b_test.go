package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// C7b §6: fleetd tails the FRONT's activity log for cloud_peer model ids
// only. Every other row on that log is a request some cell also counted,
// and folding those would double the whole fleet.

func writeDef(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCloudPeerModelIDs_OnlyCloudPeerDefs(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "cloud.yaml", `
name: cloud
backend:
  cloud_peer:
    base_url: "https://api.example.invalid"
    models: ["vendor-chat-1", "vendor-chat-2"]
`)
	writeDef(t, dir, "qwen-coder.yaml", `
name: qwen-coder
estimated_vram_gb: 20
backend:
  llama_server:
    path: `+filepath.Join(dir, "model.gguf")+`
    port: 8080
    alias: qwen-coder
    context: 4096
`)
	if err := os.WriteFile(filepath.Join(dir, "model.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ids, err := cloudPeerModelIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || !ids["vendor-chat-1"] || !ids["vendor-chat-2"] {
		t.Fatalf("cloud ids = %v, want the two cloud_peer models only", ids)
	}
	if ids["qwen-coder"] {
		t.Error("a local model id entered the cloud filter; its tokens would be counted twice")
	}
}

// TestCloudSpendPoller_SkipsWhenDefsCannotBeRead: a defs read that failed
// cannot tell cloud models from local ones. Folding unfiltered would
// count every cell's traffic a second time under the front, so the whole
// poll is skipped — the counters are cumulative, so the next successful
// poll carries the arrears.
func TestCloudSpendPoller_SkipsWhenDefsCannotBeRead(t *testing.T) {
	polled := 0
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polled++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "total_pages": 1})
	}))
	defer front.Close()

	dir := t.TempDir()
	writeDef(t, dir, "broken.yaml", "this: is: not: yaml:\n")
	poll := cloudSpendPoller(front.URL, filepath.Join(t.TempDir(), "state.json"), dir)
	if poll == nil {
		t.Fatal("poller not built")
	}
	if u := poll(context.Background()); u != nil {
		t.Errorf("folded %+v despite unreadable defs", u)
	}
	if polled != 0 {
		t.Errorf("polled the front %d times with no usable filter; it must skip entirely", polled)
	}
}

func TestCloudSpendPoller_CountsOnlyCloudRows(t *testing.T) {
	rows := []map[string]any{
		{"id": 3, "model": "vendor-chat-1", "req_path": "/v1/chat/completions", "resp_status_code": 200,
			"tokens": map[string]any{"input_tokens": 1000, "output_tokens": 200, "cache_tokens": -1}, "duration_ms": 900},
		{"id": 2, "model": "qwen-coder", "req_path": "/v1/chat/completions", "resp_status_code": 200,
			"tokens": map[string]any{"input_tokens": 9_000_000, "output_tokens": 500_000, "cache_tokens": -1}, "duration_ms": 900},
		{"id": 1, "model": "vendor-chat-1", "req_path": "/v1/chat/completions", "resp_status_code": 200,
			"tokens": map[string]any{"input_tokens": 500, "output_tokens": 100, "cache_tokens": -1}, "duration_ms": 300},
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "page": 1, "limit": 999, "total": len(rows), "total_pages": 1})
	}))
	defer front.Close()

	dir := t.TempDir()
	writeDef(t, dir, "cloud.yaml", `
name: cloud
backend:
  cloud_peer:
    base_url: "https://api.example.invalid"
    models: ["vendor-chat-1"]
`)
	poll := cloudSpendPoller(front.URL, filepath.Join(t.TempDir(), "state.json"), dir)
	u := poll(context.Background())
	if u == nil {
		t.Fatal("no usage returned")
	}
	var req, in int64
	for _, m := range u.Models {
		if m.Model != "vendor-chat-1" {
			t.Fatalf("a non-cloud model entered the cloud counters: %s", m.Model)
		}
		req += m.Req
		in += m.InFresh
	}
	if req != 2 || in != 1500 {
		t.Errorf("cloud counters = %d req / %d in, want 2 / 1500", req, in)
	}
	if u.Epoch == "" {
		// The epoch is opaque, but it must exist: fleetd keys the cloud
		// cursor on it, and an empty one merges two counter generations.
		t.Error("cloud usage carries no epoch")
	}
}
