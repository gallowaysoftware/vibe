package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vibe/proxy"
)

// llamaCppDouble is an upstream that behaves the way the novodoo cell's
// did: /v1/models answers in the Ollama shape, /v1/chat/completions is
// fine — and, like every real inference server, it refuses a model id it
// does not have.
func llamaCppDouble(t *testing.T, id string) *url.URL {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"`+id+`","model":"`+id+`"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"`+id+`"`) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestOllamaShapedCell_CatalogIDIsRoutable is the defect stated as the
// repo owner states it: a catalog id that 404s on use.
//
// vamp reads the proxy's catalog to learn what to send. Against an
// Ollama-shaped body it decodes data[], finds nothing, and substitutes the
// literal id "vibe" — which the upstream has never heard of. So the cell
// advertises a model, a consumer pins an id from that catalog, and the
// completion carrying it comes back 404 while the cell serves perfectly.
//
// This test drives the whole chain: resolve an id from the proxy exactly
// as vamp does, then send it back to the proxy and require the completion
// to land.
func TestOllamaShapedCell_CatalogIDIsRoutable(t *testing.T) {
	const id = "qwen3.6-35b-a3b"
	p := proxy.New("127.0.0.1:0")
	p.SetBackend(llamaCppDouble(t, id))
	front := httptest.NewServer(p)
	defer front.Close()

	got, err := vamp.ResolveModelID(context.Background(), nil, front.URL, "")
	if err != nil {
		t.Fatalf("ResolveModelID: %v", err)
	}
	if got != id {
		t.Errorf("ResolveModelID = %q, want %q — a consumer that pins this id "+
			"sends it back and gets a 404 from a cell that serves fine", got, id)
	}

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"`+got+`","messages":[]}`))
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a completion carrying the id the catalog advertised came back %d: %s",
			resp.StatusCode, body)
	}
}

// TestOllamaShapedCell_PreferredIDStillWins: ResolveModelID's `prefer`
// argument exists because a router's catalog lists every configured model
// and "first id" would name an arbitrary one. It has to keep working when
// the id came in through the Ollama shape.
func TestOllamaShapedCell_PreferredIDStillWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"model":"first"},{"model":"wanted"}]}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	p := proxy.New("127.0.0.1:0")
	p.SetBackend(u)
	front := httptest.NewServer(p)
	defer front.Close()

	got, err := vamp.ResolveModelID(context.Background(), nil, front.URL, "wanted")
	if err != nil {
		t.Fatalf("ResolveModelID: %v", err)
	}
	if got != "wanted" {
		t.Errorf("ResolveModelID = %q, want wanted", got)
	}
}
