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

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	p.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
	if got := w.Header().Get("X-Echo-Path"); got != "/v1/models" {
		t.Errorf("X-Echo-Path = %q", got)
	}
	if w.Body.String() != "hi" {
		t.Errorf("body = %q", w.Body.String())
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
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL, _ := url.Parse(deadSrv.URL)
	deadSrv.Close()

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
