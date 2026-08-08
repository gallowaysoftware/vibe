package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxy_NoBackend_Returns503(t *testing.T) {
	p := New("127.0.0.1:0")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	p.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// TestProxy_ForwardsToBackend pins the passthrough contract on every path
// that is not the catalog. It used to use /v1/models, which is exactly the
// behaviour that shipped the novodoo defect: the proxy forwarded whatever
// the upstream said there, in whatever shape, and "hi" is not a catalog.
// Discovery is answered by the proxy now (see TestProxy_Ollama*), so this
// asserts the same passthrough on a path that still has it.
func TestProxy_ForwardsToBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hi"))
	}))
	defer backend.Close()

	bu, _ := url.Parse(backend.URL)
	p := New("127.0.0.1:0")
	p.SetBackend(bu)

	for _, path := range []string{"/health", "/v1/completions", "/v1/models/qwen"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		p.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d", path, w.Code)
		}
		if got := w.Header().Get("X-Echo-Path"); got != path {
			t.Errorf("%s: X-Echo-Path = %q", path, got)
		}
		if w.Body.String() != "hi" {
			t.Errorf("%s: body = %q", path, w.Body.String())
		}
	}
}

// startEcho returns a server that records which upstream served a
// request (via the response header) and echoes the request body so tests
// can confirm it survived model-peeking intact.
func startEcho(t *testing.T, name string) (*httptest.Server, *url.URL) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", name)
		w.Write(body)
	}))
	u, _ := url.Parse(srv.URL)
	return srv, u
}

func TestProxy_RoutesByModel(t *testing.T) {
	defSrv, defURL := startEcho(t, "default")
	defer defSrv.Close()
	clsSrv, clsURL := startEcho(t, "classifier")
	defer clsSrv.Close()

	p := New("127.0.0.1:0")
	p.SetBackend(defURL)
	p.AddRoute("tiny-classifier", clsURL)

	cases := []struct {
		body     string
		wantUp   string
		wantBody string
	}{
		{`{"model":"tiny-classifier","messages":[]}`, "classifier", `{"model":"tiny-classifier","messages":[]}`},
		{`{"model":"qwen-coder","messages":[]}`, "default", `{"model":"qwen-coder","messages":[]}`},
		{`{"messages":[]}`, "default", `{"messages":[]}`}, // no model → default
		{`not json`, "default", `not json`},               // unparseable → default, body intact
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body))
		p.ServeHTTP(w, r)
		if got := w.Header().Get("X-Upstream"); got != tc.wantUp {
			t.Errorf("body %q routed to %q, want %q", tc.body, got, tc.wantUp)
		}
		if got := w.Body.String(); got != tc.wantBody {
			t.Errorf("body %q forwarded as %q, want %q", tc.body, got, tc.wantBody)
		}
	}
}

func TestProxy_RouteRemovedFallsBackToDefault(t *testing.T) {
	defSrv, defURL := startEcho(t, "default")
	defer defSrv.Close()
	clsSrv, clsURL := startEcho(t, "classifier")
	defer clsSrv.Close()

	p := New("127.0.0.1:0")
	p.SetBackend(defURL)
	p.AddRoute("tiny-classifier", clsURL)
	p.RemoveRoute("tiny-classifier")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"tiny-classifier"}`))
	p.ServeHTTP(w, r)
	if got := w.Header().Get("X-Upstream"); got != "default" {
		t.Errorf("after RemoveRoute, served by %q, want default", got)
	}
}

func TestProxy_AggregatesModels(t *testing.T) {
	defSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"qwen-coder"}]}`))
	}))
	defer defSrv.Close()
	clsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"tiny-classifier"}]}`))
	}))
	defer clsSrv.Close()
	defURL, _ := url.Parse(defSrv.URL)
	clsURL, _ := url.Parse(clsSrv.URL)

	p := New("127.0.0.1:0")
	p.SetBackend(defURL)
	p.AddRoute("tiny-classifier", clsURL)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	body := w.Body.String()
	if !strings.Contains(body, "qwen-coder") || !strings.Contains(body, "tiny-classifier") {
		t.Errorf("/v1/models aggregation missing a model: %s", body)
	}
}

func TestProxy_AggregatesModels_NoDefault(t *testing.T) {
	clsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"tiny-classifier"}]}`))
	}))
	defer clsSrv.Close()
	clsURL, _ := url.Parse(clsSrv.URL)

	p := New("127.0.0.1:0")
	p.AddRoute("tiny-classifier", clsURL)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "tiny-classifier") {
		t.Errorf("/v1/models missing routed model: %s", body)
	}
}

func TestProxy_AggregatesModels_NoDefault_AllRoutesDown_Returns503(t *testing.T) {
	// A closed httptest server is not a dead address, it is a FREE one: the
	// port goes back to the ephemeral pool and any other server in this
	// binary — or on the box — can rebind it, at which point the "dead"
	// upstream answers and this test fails for a reason nowhere near its
	// subject. (Observed under concurrent runs of this package.) Port 1 is
	// the house idiom for an address that refuses immediately and can never
	// be bound by an unprivileged process.
	deadURL, _ := url.Parse("http://127.0.0.1:1")

	p := New("127.0.0.1:0")
	p.AddRoute("tiny-classifier", deadURL)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestProxy_SetBackendNilClears(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hi"))
	}))
	defer backend.Close()

	bu, _ := url.Parse(backend.URL)
	p := New("127.0.0.1:0")
	p.SetBackend(bu)
	p.SetBackend(nil)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status after clear = %d, want 503", w.Code)
	}
}
