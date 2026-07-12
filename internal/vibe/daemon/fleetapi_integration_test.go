package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// TestDaemon_FleetAPI_AuthAndState proves the fleet endpoints ride the same
// listener-scoped auth boundary as the Connect handlers: 401 on TCP without
// the bearer token, 200 with it, and 200 over the unix socket with no token.
// The front cell is a fake llama-swap owning the (disabled) proxy port, so
// the state snapshot also exercises the /running + /v1/models merge through
// the daemon's real wiring.
func TestDaemon_FleetAPI_AuthAndState(t *testing.T) {
	setupXDG(t)

	// Fake llama-swap front cell. DisableProxy lets it own the proxy port,
	// mirroring the production A1 topology.
	cellMux := http.NewServeMux()
	cellMux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"running":[{"model":"qwen","state":"ready","ttl":600,"name":"Qwen"}]}`))
	})
	cellMux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen","object":"model"}]}`))
	})
	cellMux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	cell := httptest.NewServer(cellMux)
	t.Cleanup(cell.Close)

	httpAddr := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	cfg := daemon.Config{
		ProxyPort:    cell.Listener.Addr().(*net.TCPAddr).Port,
		DisableProxy: true,
		HTTPAddr:     httpAddr,
		LlamaBinary:  fakeBinary,
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	tokenBytes, err := os.ReadFile(paths.TokenFile())
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	get := func(url, tok string) (*http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return http.DefaultClient.Do(req)
	}

	// TCP without token → 401, for both fleet endpoints.
	for _, path := range []string{"/api/fleet/state", "/api/fleet/events"} {
		resp, err := get("http://"+httpAddr+path, "")
		if err != nil {
			t.Fatalf("GET %s (no token): %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s (no token): status %d; want 401", path, resp.StatusCode)
		}
	}

	// TCP with token → 200 and a merged snapshot.
	resp, err := get("http://"+httpAddr+"/api/fleet/state", token)
	if err != nil {
		t.Fatalf("GET state (token): %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET state (token): status %d body %s", resp.StatusCode, body)
	}
	var st struct {
		Cells []struct {
			Name      string `json:"name"`
			Reachable bool   `json:"reachable"`
			Models    []struct {
				ID    string `json:"id"`
				State string `json:"state"`
				TTL   int    `json:"ttl"`
			} `json:"models"`
		} `json:"cells"`
		Daemon struct {
			ActiveProfile string `json:"active_profile"`
			Services      []any  `json:"services"`
		} `json:"daemon"`
		StartHistory map[string]any `json:"start_history"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode state: %v (%s)", err, body)
	}
	if len(st.Cells) != 1 || st.Cells[0].Name != "front" || !st.Cells[0].Reachable {
		t.Fatalf("cells = %+v; want one reachable front cell", st.Cells)
	}
	if len(st.Cells[0].Models) != 1 || st.Cells[0].Models[0].ID != "qwen" ||
		st.Cells[0].Models[0].State != "ready" || st.Cells[0].Models[0].TTL != 600 {
		t.Errorf("models = %+v; want qwen ready ttl 600", st.Cells[0].Models)
	}
	if st.Daemon.Services == nil || st.StartHistory == nil {
		t.Errorf("daemon.services / start_history missing: %s", body)
	}

	// Unix socket without token → 200.
	sock := paths.Socket()
	unixHC := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var nd net.Dialer
				return nd.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 5 * time.Second,
	}
	uresp, err := unixHC.Get("http://vibe.local/api/fleet/state")
	if err != nil {
		t.Fatalf("GET state (unix): %v", err)
	}
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Errorf("GET state (unix): status %d; want 200", uresp.StatusCode)
	}
}
