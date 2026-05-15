package vibeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestNew_PrefersExplicitArgument(t *testing.T) {
	t.Setenv("VIBE_API", "http://env:9999")
	c := New("http://arg:1234")
	if c.BaseURL() != "http://arg:1234" {
		t.Errorf("baseURL = %q", c.BaseURL())
	}
}

func TestNew_FallsBackToEnv(t *testing.T) {
	t.Setenv("VIBE_API", "http://env:9999")
	c := New("")
	if c.BaseURL() != "http://env:9999" {
		t.Errorf("baseURL = %q", c.BaseURL())
	}
}

func TestNew_FallsBackToDefault(t *testing.T) {
	t.Setenv("VIBE_API", "")
	c := New("")
	if c.BaseURL() != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.BaseURL(), DefaultBaseURL)
	}
}

func TestStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/status" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "wrong route", 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"running":    true,
			"ready":      true,
			"profile":    "code",
			"proxy_addr": "http://127.0.0.1:9000",
			"pid":        12345,
		})
	}))
	defer srv.Close()

	s, err := New(srv.URL).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Running || !s.Ready || s.Profile != "code" || s.PID != 12345 {
		t.Errorf("got %+v", s)
	}
	if s.ProxyAddr != "http://127.0.0.1:9000" {
		t.Errorf("ProxyAddr = %q", s.ProxyAddr)
	}
}

func TestListProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"profiles": []map[string]any{
				{"name": "chat", "path": "/c.yaml"},
				{"name": "code", "description": "Coding", "path": "/d.yaml"},
			},
		})
	}))
	defer srv.Close()

	profs, err := New(srv.URL).ListProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(profs) != 2 || profs[1].Name != "code" || profs[1].Description != "Coding" {
		t.Errorf("got %+v", profs)
	}
}

func TestStart_ParsesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["profile"] != "code" {
			t.Errorf("profile = %q", req["profile"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": map[string]any{
				"running": true, "ready": true, "profile": req["profile"],
				"proxy_addr": "http://127.0.0.1:9000",
			},
		})
	}))
	defer srv.Close()

	s, err := New(srv.URL).Start(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "code" || !s.Ready {
		t.Errorf("got %+v", s)
	}
}

func TestStart_ErrorPayloadBubblesAsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": `profile "code" is already running; stop first`})
	}))
	defer srv.Close()

	_, err := New(srv.URL).Start(context.Background(), "code")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureActive_NoOpWhenAlreadyReady(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{
			"running": true, "ready": true, "profile": "code",
		})
	}))
	defer srv.Close()

	s, err := New(srv.URL).EnsureActive(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "code" {
		t.Errorf("profile = %q", s.Profile)
	}
	if !reflect.DeepEqual(paths, []string{"GET /status"}) {
		t.Errorf("paths = %v", paths)
	}
}

func TestEnsureActive_StopsAndStartsWhenSwitching(t *testing.T) {
	var (
		mu      sync.Mutex
		calls   []string
		current = "chat" // pretend chat is the active profile
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/status":
			json.NewEncoder(w).Encode(map[string]any{
				"running": current != "", "ready": current != "", "profile": current,
			})
		case "/stop":
			mu.Lock()
			current = ""
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"running": false}})
		case "/start":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			current = req["profile"]
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"running": true, "ready": true, "profile": current},
			})
		default:
			http.Error(w, "wrong route", 404)
		}
	}))
	defer srv.Close()

	s, err := New(srv.URL).EnsureActive(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	if s.Profile != "code" {
		t.Errorf("profile = %q", s.Profile)
	}
	want := []string{"GET /status", "POST /stop", "POST /start"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestEnsureActive_StartsWhenNothingActive(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/status":
			json.NewEncoder(w).Encode(map[string]any{"running": false, "ready": false})
		case "/start":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(map[string]any{
				"status": map[string]any{"running": true, "ready": true, "profile": req["profile"]},
			})
		}
	}))
	defer srv.Close()

	_, err := New(srv.URL).EnsureActive(context.Background(), "code")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /status", "POST /start"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}
