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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// fleetCellFake is a minimal llama-swap cell for fleet_registry tests.
func fleetCellFake(t *testing.T, models string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"running":[]}`))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// TestDaemon_FleetRegistry_Role activates the fleetd role over a two-cell
// hosts.yaml and exercises the registry, the intent endpoint, and the MCP
// facade through the daemon's real TCP auth boundary.
func TestDaemon_FleetRegistry_Role(t *testing.T) {
	cfgHome, _, _ := setupXDG(t)

	front := fleetCellFake(t, `{"object":"list","data":[{"id":"kimi-k3","object":"model"}]}`)
	gpu := fleetCellFake(t, `{"object":"list","data":[{"id":"qwen3.6-27b","object":"model"}]}`)

	hostsYAML := fmt.Sprintf(`
fleetd_url: "http://127.0.0.1:1"
cells:
  front:    { url: "%s", class: always_on }
  gpu-cell: { url: "%s", class: opportunistic }
`, front.URL, gpu.URL)
	if err := os.WriteFile(filepath.Join(cfgHome, "vibe", "hosts.yaml"), []byte(hostsYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	httpAddr := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	cfg := daemon.Config{
		ProxyPort:     pickFreePort(t),
		DisableProxy:  true,
		HTTPAddr:      httpAddr,
		LlamaBinary:   fakeBinary,
		FleetRegistry: true,
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	tokenBytes, err := os.ReadFile(paths.TokenFile())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	req := func(method, url, tok, body string) *http.Response {
		t.Helper()
		// No timeout context here: defer-cancel would fire at helper
		// return and kill the in-flight body stream (multi-segment
		// bodies — the fleet page — truncate to whatever arrived first).
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		r, err := http.NewRequest(method, url, rdr)
		if err != nil {
			t.Fatal(err)
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	// The registry has both cells with class and display state.
	resp := req(http.MethodGet, "http://"+httpAddr+"/api/fleet/state", token, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state: HTTP %d: %s", resp.StatusCode, body)
	}
	var st struct {
		Cells []struct {
			Name    string `json:"name"`
			Class   string `json:"class"`
			Display string `json:"display"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(st.Cells) != 2 {
		t.Fatalf("cells = %+v, want 2", st.Cells)
	}
	byName := map[string]string{}
	for _, c := range st.Cells {
		byName[c.Name] = c.Display
		if c.Class == "" {
			t.Errorf("cell %s missing class", c.Name)
		}
	}
	if byName["front"] != "SERVING" || byName["gpu-cell"] != "SERVING" {
		t.Errorf("displays = %v, want both SERVING", byName)
	}

	// Intent: drain gpu-cell, read it back in the state.
	resp = req(http.MethodPost, "http://"+httpAddr+"/api/fleet/intent", token,
		`{"cell":"gpu-cell","state":"drained","reason":"gaming","eta":"23:00"}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("intent POST: HTTP %d: %s", resp.StatusCode, body)
	}
	resp = req(http.MethodGet, "http://"+httpAddr+"/api/fleet/state", token, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var st2 struct {
		Cells []struct {
			Name    string `json:"name"`
			Display string `json:"display"`
			Intent  *struct {
				Reason string `json:"reason"`
			} `json:"intent"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(body, &st2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	found := false
	for _, c := range st2.Cells {
		if c.Name == "gpu-cell" {
			found = true
			if c.Intent == nil || c.Intent.Reason != "gaming" {
				t.Errorf("gpu-cell intent = %+v", c.Intent)
			}
			// Cell answers but intent says drained: INCONSISTENT nags.
			if c.Display != "INCONSISTENT" {
				t.Errorf("gpu-cell display = %s, want INCONSISTENT", c.Display)
			}
		}
	}
	if !found {
		t.Fatal("gpu-cell missing from state")
	}

	// MCP: initialize + fleet_status through the same auth boundary.
	resp = req(http.MethodPost, "http://"+httpAddr+"/mcp", token,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "vibe-fleet") {
		t.Fatalf("mcp initialize: HTTP %d: %s", resp.StatusCode, body)
	}
	resp = req(http.MethodPost, "http://"+httpAddr+"/mcp", token,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}`)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "gpu-cell") || !strings.Contains(string(body), "INCONSISTENT") {
		t.Errorf("mcp fleet_status missing derived state: %s", body)
	}

	// The MCP endpoint enforces the same bearer boundary as the RPCs.
	resp = req(http.MethodPost, "http://"+httpAddr+"/mcp", "", `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("mcp without token: HTTP %d, want 401", resp.StatusCode)
	}
	// And intent without a token likewise.
	resp = req(http.MethodPost, "http://"+httpAddr+"/api/fleet/intent", "", `{"cell":"gpu-cell","state":"serving"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("intent without token: HTTP %d, want 401", resp.StatusCode)
	}
	// The fleet page is the single static exemption (no data in it);
	// everything else stays gated.
	resp = req(http.MethodGet, "http://"+httpAddr+"/ui/fleet", "", "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("page response: status=%d headers=%v bodylen=%d", resp.StatusCode, resp.Header, len(body))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "/api/fleet/state") {
		t.Errorf("page: HTTP %d, want 200 with the page", resp.StatusCode)
	}
	// The exemption is exact-match and GET-only, evaluated before mux
	// path-cleaning. Pin the boundary so nobody "hardens" it into a
	// prefix match or a path.Clean — either would WIDEN the one hole.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/ui/fleet"},
		{http.MethodGet, "/ui/fleet/"},
		{http.MethodGet, "/ui/fleet/../api/fleet/state"},
		{http.MethodGet, "/ui/fleet%2f"},
		{http.MethodGet, "//ui/fleet"},
	} {
		resp = req(tc.method, "http://"+httpAddr+tc.path, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without token: HTTP %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestDaemon_FleetRegistry_RequiresCells: the role is explicit — set with
// no cells: section in hosts.yaml, startup fails loudly rather than
// silently running a one-cell registry.
func TestDaemon_FleetRegistry_RequiresCells(t *testing.T) {
	setupXDG(t) // no hosts.yaml at all

	cfg := daemon.Config{
		ProxyPort:     pickFreePort(t),
		DisableProxy:  true,
		HTTPAddr:      fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:   fakeBinary,
		FleetRegistry: true,
	}
	d := daemon.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "fleet_registry") {
			t.Errorf("Run error = %v, want fleet_registry cells failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not fail within 5s despite fleet_registry with no hosts.yaml")
	}
}

// TestDaemon_FleetRegistryOff_NoMCP guards the regression gate: an
// ordinary daemon (no fleet_registry) has no /mcp and no intent route.
func TestDaemon_FleetRegistryOff_NoMCP(t *testing.T) {
	setupXDG(t)
	cell := fleetCellFake(t, `{"object":"list","data":[]}`)
	cfg := daemon.Config{
		ProxyPort:    cell.Listener.Addr().(*net.TCPAddr).Port,
		DisableProxy: true,
		HTTPAddr:     fmt.Sprintf("127.0.0.1:%d", pickFreePort(t)),
		LlamaBinary:  fakeBinary,
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	tokenBytes, _ := os.ReadFile(paths.TokenFile())
	token := strings.TrimSpace(string(tokenBytes))
	httpAddr := cfg.HTTPAddr
	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/mcp"},
		{http.MethodPost, "/api/fleet/intent"},
	} {
		req, _ := http.NewRequest(probe.method, "http://"+httpAddr+probe.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s without fleet_registry: HTTP %d, want 404", probe.path, resp.StatusCode)
		}
	}
}
