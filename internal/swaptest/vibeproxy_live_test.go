package swaptest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/proxy"
)

// TestSwapBehaviourVibeProxyCatalogThroughARealFront stands the real
// topology the novodoo defect appeared in — a real llama-swap front, a
// peers: stanza, and a vibe proxy on the far end fronting an upstream that
// answers /v1/models in the Ollama shape — and checks the one invariant
// that was violated: the id the FRONT advertises must be an id the PEER's
// catalog confirms.
//
// It also settles, against both shipped wire versions, a question the
// double cannot answer. llama-swap does NOT read a peer's /v1/models: the
// peer stanza carries an explicit models: list (v239 refuses to load a
// config without one), the front's catalog is built from that list, and a
// completion is forwarded on the strength of the config alone. Measured on
// v239 and v247: an Ollama-shaped peer routes exactly as well as an
// OpenAI-shaped one. So llama-swap's routing is not what the Ollama shape
// broke — what it broke is every consumer that reads the peer's catalog to
// find out what the peer serves, which is how a front comes to advertise
// an id that nothing downstream can confirm and no `vibe` command can see.
//
// The name is deliberate: CI's conformance job selects on
// `TestSwapContract|TestSwapBehaviour` as an unanchored pattern, so this
// runs in the same matrix, against both real binaries, with no workflow
// change.
func TestSwapBehaviourVibeProxyCatalogThroughARealFront(t *testing.T) {
	swapBin := os.Getenv("LLAMA_SWAP_BIN")
	if swapBin == "" {
		found, err := exec.LookPath("llama-swap")
		if err != nil {
			t.Skip("no llama-swap: set LLAMA_SWAP_BIN or put one on PATH")
		}
		swapBin = found
	}

	const modelID = "qwen3.6-35b-a3b"

	// The cell's upstream: Ollama-shaped catalog, working completions, and
	// — like every real inference server — a 404 for an id it does not
	// have. No llama-server and no GGUF: a peer's models are never started
	// by the front, so this needs neither.
	served := make(chan string, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"`+modelID+`","model":"`+modelID+`"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Model != modelID {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
			return
		}
		served <- probe.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	uu, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	// The cell: a vibe proxy, exactly as a cell runs one.
	p := proxy.New("127.0.0.1:0")
	p.SetBackend(uu)
	cell := httptest.NewServer(p)
	defer cell.Close()

	front := execPeerFront(t, swapBin, map[string]string{"vibecell": cell.URL}, modelID)

	// 1. The front advertises the model (from its config).
	frontIDs := catalogIDs(t, front+"/v1/models")
	if len(frontIDs) == 0 {
		t.Fatal("the front advertises no models at all")
	}

	// 2. The peer's own catalog confirms every id the front advertises.
	//    This is the assertion the defect fails: against an Ollama-shaped
	//    cell the peer catalog reads as EMPTY to any data[] consumer, so
	//    the front is advertising an id nothing downstream can confirm.
	//    v247 namespaces a peer's ids as "<peer>/<id>" where v239 does not,
	//    so compare on the trailing segment.
	peerIDs := catalogIDs(t, cell.URL+"/v1/models")
	have := map[string]bool{}
	for _, id := range peerIDs {
		have[id] = true
	}
	for _, id := range frontIDs {
		bare := id
		if i := strings.LastIndex(bare, "/"); i >= 0 {
			bare = bare[i+1:]
		}
		if !have[bare] {
			t.Errorf("the front advertises %q; the cell's own catalog says it serves %v.\n"+
				"An id in the front's catalog that the cell cannot confirm is an id a "+
				"client pins and then 404s on.", id, peerIDs)
		}
	}

	// 3. And the chain actually carries a completion end to end.
	resp, err := http.Post(front+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"`+frontIDs[0]+`","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("completion through the front: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("front -> vibe proxy -> upstream completion answered %d: %s", resp.StatusCode, body)
	}
	select {
	case got := <-served:
		if got != modelID {
			t.Errorf("the upstream was asked for %q, want %q", got, modelID)
		}
	case <-time.After(5 * time.Second):
		t.Error("the completion never reached the upstream behind the vibe proxy")
	}
}

// catalogIDs reads a /v1/models the way every consumer in this repo does:
// data[] and the id inside each row, and nothing else.
func catalogIDs(t *testing.T, url string) []string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d: %s", url, resp.StatusCode, body)
	}
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		t.Fatalf("GET %s: catalog is not decodable by an OpenAI client: %v\n%s", url, err, body)
	}
	out := make([]string, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		out = append(out, m.ID)
	}
	return out
}

// execPeerFront starts a real llama-swap holding nothing but peer stanzas
// — the front render's shape (internal/vibe/router: "the front owns no
// models"). No llama-server, no GGUF, no model ever started.
func execPeerFront(t *testing.T, swapBin string, peers map[string]string, model string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	fmt.Fprintf(&b, "healthCheckTimeout: 30\nlogLevel: info\nstore:\n  path: %s\npeers:\n",
		filepath.Join(dir, "store.db"))
	for name, u := range peers {
		fmt.Fprintf(&b, "  %s:\n    proxy: %s\n    models: [%s]\n", name, u, model)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, swapBin, "-config", cfgPath, "-listen", fmt.Sprintf("127.0.0.1:%d", port))
	// SIGTERM for the same reason execLlamaSwap uses it: llama-swap owns
	// whatever it spawned and must be given the chance to reap it.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdout = &testWriter{t: t, prefix: "llama-swap"}
	cmd.Stderr = &testWriter{t: t, prefix: "llama-swap!"}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start llama-swap: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, base+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		rcancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("llama-swap did not answer /health on %s within 30s", base)
	return ""
}
