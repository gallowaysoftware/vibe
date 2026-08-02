package daemon_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
)

// announceSink is a fake fleetd registry capturing announces.
type announceSink struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (s *announceSink) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/announce", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.reqs = append(s.reqs, body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"interval_s": 1})
	})
	return mux
}

func (s *announceSink) calls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.reqs...)
}

// TestDaemon_AnnounceLoop proves the daemon announces when fleet.cell +
// registry_url are configured: a heartbeat lands with the right cell,
// intent, and models block — and a drain stamps local intent into the
// NEXT announce's echo.
func TestDaemon_AnnounceLoop(t *testing.T) {
	setupXDG(t)
	sink := &announceSink{}
	registry := httptest.NewServer(sink.handler())
	t.Cleanup(registry.Close)

	// Fake llama-swap with one resident model.
	cellMux := http.NewServeMux()
	cellMux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"running":[{"model":"qwen","state":"ready"}]}`))
	})
	cellMux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	cell := httptest.NewServer(cellMux)
	t.Cleanup(cell.Close)

	// A def for the fingerprint block.
	backendDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "vibe", "backends")
	if err := os.MkdirAll(backendDir, 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "qwen.gguf")
	if err := os.WriteFile(model, []byte("GGUF"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := fmt.Sprintf(`
name: qwen
cell: gpu-cell
backend:
  external: true
  llama_server: {path: %s, alias: qwen, context: 4096}
`, model)
	if err := os.WriteFile(filepath.Join(backendDir, "qwen.yaml"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := daemon.Config{
		ProxyPort:    cell.Listener.Addr().(*net.TCPAddr).Port,
		DisableProxy: true,
		HTTPAddr:     fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:  fakeBinary,
		CellCmds:     daemon.CellCmds{Drain: "true", Resume: "true"},
		Fleet:        daemon.FleetConfig{Cell: "gpu-cell", RegistryURL: registry.URL},
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	deadline := time.Now().Add(5 * time.Second)
	for len(sink.calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	calls := sink.calls()
	if len(calls) == 0 {
		t.Fatal("no announce landed within 5s")
	}
	first := calls[0]
	if first["cell"] != "gpu-cell" || first["v"] != float64(1) {
		t.Errorf("announce = %v", first)
	}
	intent, _ := first["intent"].(map[string]any)
	if intent["state"] != "serving" {
		t.Errorf("intent = %v", intent)
	}
	models, _ := first["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["id"] != "qwen" {
		t.Errorf("models = %v", models)
	}
	if models[0].(map[string]any)["state"] != "ready" {
		t.Errorf("model state = %v, want ready (from /running)", models[0])
	}
	if models[0].(map[string]any)["flags_sha256"] == "" {
		t.Error("flags_sha256 missing")
	}
	vr, _ := first["versions"].(map[string]any)
	if vr["vibe"] == "" {
		t.Errorf("versions = %v", vr)
	}

	// A local drain stamps the echo: a subsequent announce carries
	// intent.state=drained with a fresh timestamp.
	client := unixClient(t)
	if _, err := client.CellDrain(t.Context(), "gaming", "23:00", 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		calls := sink.calls()
		if len(calls) > 0 {
			last := calls[len(calls)-1]
			if intent, _ := last["intent"].(map[string]any); intent["state"] == "drained" {
				if intent["since"] == nil || intent["since"] == "" {
					t.Errorf("drained echo missing timestamp: %v", intent)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no drained echo within 5s; announces: %v", sink.calls())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDaemon_AnnounceNotStartedWithoutCell guards the default: no
// fleet.cell, no announce loop.
func TestDaemon_AnnounceNotStartedWithoutCell(t *testing.T) {
	setupXDG(t)
	sink := &announceSink{}
	registry := httptest.NewServer(sink.handler())
	t.Cleanup(registry.Close)

	cfg := daemon.Config{
		ProxyPort:    pickFreePort(t),
		DisableProxy: true,
		HTTPAddr:     fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:  fakeBinary,
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	time.Sleep(300 * time.Millisecond)
	if got := sink.calls(); len(got) != 0 {
		t.Errorf("announces without a cell identity: %v", got)
	}
}
