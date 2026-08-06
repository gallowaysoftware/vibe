package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The catalog contract, from the outside: whatever shape an upstream
// answers /v1/models in, vibe's proxy answers in the OpenAI shape, on
// every path it serves that endpoint.
//
// The failure this guards is not a 500 anybody notices. It is a cell that
// advertises a model id nobody can route to — or, in the Ollama case, a
// cell whose catalog reads as EMPTY to every consumer in this repo while
// its /v1/chat/completions works perfectly. Consumers pin ids from the
// catalog, so a catalog they cannot read is worse than an absent peer.

// ollamaBody is the exact shape observed on the novodoo cell.
const ollamaBody = `{"models":[{"name":"qwen3.6-35b-a3b"}]}`

// catalogClient decodes a /v1/models body the way every consumer in this
// repo does — vamp's ResolveModelID, `vibe cell`'s direct probe,
// fleetannounce's catalogIDs, the daemon's external-backend check. None of
// them look at anything but data[].id.
func catalogIDs(t *testing.T, body string) []string {
	t.Helper()
	var wrap struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &wrap); err != nil {
		t.Fatalf("catalog is not decodable by an OpenAI client: %v\n%s", err, body)
	}
	if wrap.Object != "list" {
		t.Errorf(`catalog object = %q, want "list": %s`, wrap.Object, body)
	}
	out := make([]string, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		out = append(out, m.ID)
	}
	return out
}

func serveBody(t *testing.T, body string) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func getModels(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	return w
}

// TestProxy_OllamaShapedUpstreamIsServedAsOpenAI is the regression test for
// the defect: a single upstream, no routes, no rewrite — the fall-through
// path, which forwarded the upstream's body verbatim.
func TestProxy_OllamaShapedUpstreamIsServedAsOpenAI(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, ollamaBody))

	w := getModels(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	got := catalogIDs(t, w.Body.String())
	if len(got) != 1 || got[0] != "qwen3.6-35b-a3b" {
		t.Fatalf("catalog ids = %v, want [qwen3.6-35b-a3b]: %s", got, w.Body.String())
	}
}

func TestProxy_OllamaShapedUpstream_DropsOllamaOnlyFields(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, `{"models":[{"name":"m","model":"m","size":123,`+
		`"digest":"sha256:beef","modified_at":"2026-01-01T00:00:00Z"}]}`))

	body := getModels(t, p).Body.String()
	for _, field := range []string{"size", "digest", "modified_at"} {
		if strings.Contains(body, field) {
			t.Errorf("catalog still carries the Ollama-only field %q: %s", field, body)
		}
	}
}

// TestProxy_OllamaShapedUpstreamWithRewrite covers the mlx_lm.server path:
// the upstream insists on its own id, the client is handed an alias, and
// the two halves of the catalog have to name the same thing.
func TestProxy_OllamaShapedUpstreamWithRewrite(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, `{"models":[{"name":"/models/Qwen.gguf","model":"/models/Qwen.gguf"}]}`))
	p.SetModelRewrite("qwen-alias", "/models/Qwen.gguf")

	w := getModels(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := catalogIDs(t, w.Body.String())
	if len(got) != 1 || got[0] != "qwen-alias" {
		t.Fatalf("catalog ids = %v, want [qwen-alias]: %s", got, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "/models/Qwen.gguf") {
		t.Errorf("the upstream id leaked into the catalog: %s", w.Body.String())
	}
}

// TestProxy_OllamaShapedUpstreamAggregates is the same fix on the
// routes-present path, where an Ollama body used to decode "successfully"
// into a data[]-shaped struct and contribute NOTHING — a 200 whose catalog
// silently omits the active model.
func TestProxy_OllamaShapedUpstreamAggregates(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, ollamaBody))
	p.AddRoute("tiny-classifier", serveBody(t, `{"object":"list","data":[{"id":"tiny-classifier"}]}`))

	w := getModels(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := catalogIDs(t, w.Body.String())
	want := map[string]bool{"qwen3.6-35b-a3b": false, "tiny-classifier": false}
	for _, id := range got {
		if _, ok := want[id]; ok {
			want[id] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("aggregated catalog is missing %q: %s", id, w.Body.String())
		}
	}
}

// TestProxy_OllamaShapedRoutedUpstreamAggregates: an Ollama-shaped SERVICE
// backend is the same defect one hop over. It used to be dropped silently
// because the route "succeeded" with zero rows.
func TestProxy_OllamaShapedRoutedUpstreamAggregates(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, `{"object":"list","data":[{"id":"qwen-coder"}]}`))
	p.AddRoute("tiny-classifier", serveBody(t, `{"models":[{"name":"tiny-classifier"}]}`))

	got := catalogIDs(t, getModels(t, p).Body.String())
	found := false
	for _, id := range got {
		if id == "tiny-classifier" {
			found = true
		}
	}
	if !found {
		t.Errorf("catalog ids = %v, want the routed service's model", got)
	}
}

// TestProxy_UnrecognisedCatalogShapeIsNotForwarded pins the decision: a
// body in no shape vibe recognises becomes a 502 naming the upstream, NOT
// a 200 with an empty catalog and NOT the upstream's body passed along as
// though it were OpenAI's. Forwarding it unexamined is precisely how the
// defect reached production; an empty 200 is the same lie with better
// manners, and consumers cannot tell it from a cell that genuinely serves
// nothing.
func TestProxy_UnrecognisedCatalogShapeIsNotForwarded(t *testing.T) {
	for _, body := range []string{`hi`, `{"result":[{"id":"m"}]}`, `[{"id":"m"}]`} {
		p := New("127.0.0.1:0")
		p.SetBackend(serveBody(t, body))

		w := getModels(t, p)
		if w.Code != http.StatusBadGateway {
			t.Errorf("upstream body %q: status = %d, want 502", body, w.Code)
		}
		if strings.Contains(w.Body.String(), `"id"`) || w.Body.String() == body {
			t.Errorf("upstream body %q was forwarded to the client: %s", body, w.Body.String())
		}
	}
}

// A recognised-but-empty catalog is a claim the upstream is entitled to
// make, and must survive as one.
func TestProxy_EmptyCatalogIsServedAsEmpty(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, `{"object":"list","data":[]}`))

	w := getModels(t, p)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := catalogIDs(t, w.Body.String()); len(got) != 0 {
		t.Errorf("catalog ids = %v, want none", got)
	}
}

// TestProxy_UpstreamCatalogStatusIsPreserved: an upstream that answers
// /v1/models with a status of its own said something specific (ComfyUI
// does not implement it; llama-swap refuses a missing API key), and that
// status is more use to an operator than a blanket 502.
func TestProxy_UpstreamCatalogStatusIsPreserved(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		u, _ := url.Parse(srv.URL)
		p := New("127.0.0.1:0")
		p.SetBackend(u)

		w := getModels(t, p)
		if w.Code != code {
			t.Errorf("upstream %d: proxy answered %d", code, w.Code)
		}
		srv.Close()
	}
}

// TestProxy_CatalogFetchCarriesCallerCredentials: llama-swap exempts only
// /health from its API key. Forwarding the request (what the no-routes
// path used to do) carried the caller's Authorization header for free;
// fetching it has to do so deliberately, or every keyed cell answers 401
// to discovery and reads as down.
func TestProxy_CatalogFetchCarriesCallerCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer swap-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := New("127.0.0.1:0")
	p.SetBackend(u)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer swap-key")
	p.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := catalogIDs(t, w.Body.String()); len(got) != 1 || got[0] != "m" {
		t.Errorf("catalog ids = %v, want [m]", got)
	}
}

// TestProxy_CatalogFetchDoesNotLeakCredentialsToSidecars pins the scope of
// the header above. The default upstream is the one the caller was
// addressing; a routed service backend is a co-resident process the
// aggregation path has never sent the caller's headers to, and discovery
// is not the place to start.
func TestProxy_CatalogFetchDoesNotLeakCredentialsToSidecars(t *testing.T) {
	seen := make(chan string, 1)
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"tiny-classifier"}]}`))
	}))
	defer side.Close()
	su, _ := url.Parse(side.URL)

	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, `{"object":"list","data":[{"id":"qwen-coder"}]}`))
	p.AddRoute("tiny-classifier", su)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer front-key")
	p.ServeHTTP(w, r)

	if got := <-seen; got != "" {
		t.Errorf("routed sidecar received the caller's credential %q", got)
	}
}

// TestProxy_CatalogKeepsTheResidencyKeyForALlamaCppCell: fleetd reads the
// Ollama models[] key's PRESENCE as residency evidence for a cell with no
// /running to merge against — a llama.cpp-family upstream advertises only
// what it has LOADED. Normalising that key away would be a tidier shape
// and would turn every loaded model on such a cell into a "stopped" one in
// the fleet view, so it is re-emitted (ids only) when and only when the
// upstream sent one. See TestNormalizedOllamaCatalogStaysReady in
// internal/vibe/fleetapi for the other end of this.
func TestProxy_CatalogKeepsTheResidencyKeyForALlamaCppCell(t *testing.T) {
	p := New("127.0.0.1:0")
	p.SetBackend(serveBody(t, ollamaBody))
	if body := getModels(t, p).Body.String(); !strings.Contains(body, `"models":`) {
		t.Errorf("catalog dropped the residency key fleetd reads: %s", body)
	}

	q := New("127.0.0.1:0")
	q.SetBackend(serveBody(t, `{"object":"list","data":[{"id":"m"}]}`))
	if body := getModels(t, q).Body.String(); strings.Contains(body, `"models":`) {
		t.Errorf("catalog invented a residency key for a llama-swap cell: %s", body)
	}
}
