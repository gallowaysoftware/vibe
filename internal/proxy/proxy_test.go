package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
