package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// chatProbeBody is the subset of a chat-completion request the warm/
// heartbeat tests key on.
type chatProbeBody struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens int    `json:"max_tokens"`
}

// serveChat answers both stage requests and warm probes: SSE when streaming
// was requested, plain JSON otherwise.
func serveChat(w http.ResponseWriter, body chatProbeBody) {
	if body.Stream {
		startSSE(w)
		sseChunk(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
		sseChunk(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		sseChunk(w, "[DONE]")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintln(w, `{"choices":[{"message":{"content":"ok"}}]}`)
}

// TestExecutor_KeepWarmHeartbeatDuringIdleStage: while a profileless stage
// (here a slow webhook) holds the run, the Ensured LLM endpoint sits unused
// past keep_warm and must receive 1-token streaming heartbeats.
func TestExecutor_KeepWarmHeartbeatDuringIdleStage(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	var heartbeats atomic.Int64
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body chatProbeBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Stage requests carry no max_tokens; the keep-warm probe always
		// asks for exactly one streamed token.
		if body.MaxTokens == 1 && body.Stream {
			heartbeats.Add(1)
		}
		serveChat(w, body)
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		fmt.Fprintln(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	exec := &Executor{
		Pipeline: &Pipeline{
			Name:     "kw",
			KeepWarm: KeepWarmSetting{Interval: 50 * time.Millisecond},
			Stages: []Stage{
				{ID: "gen", Capability: "reasoning", Prompt: "x", Output: "gen.txt"},
				{ID: "wait", Type: StageTypeWebhook, URL: srv.URL + "/slow", Method: "GET", Inputs: []string{"gen"}, Output: "wait.json"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
		Log:          &bytes.Buffer{},
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if heartbeats.Load() == 0 {
		t.Error("no keep-warm heartbeat fired during the 400ms idle stage")
	}
}

// TestExecutor_KeepWarmDisabled: keep_warm: false must produce zero
// heartbeats no matter how long the idle gap is.
func TestExecutor_KeepWarmDisabled(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	var heartbeats atomic.Int64
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body chatProbeBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.MaxTokens == 1 && body.Stream {
			heartbeats.Add(1)
		}
		serveChat(w, body)
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprintln(w, `{"ok":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	exec := &Executor{
		Pipeline: &Pipeline{
			Name:     "kw-off",
			KeepWarm: KeepWarmSetting{Disabled: true},
			Stages: []Stage{
				{ID: "gen", Capability: "reasoning", Prompt: "x", Output: "gen.txt"},
				{ID: "wait", Type: StageTypeWebhook, URL: srv.URL + "/slow", Method: "GET", Inputs: []string{"gen"}, Output: "wait.json"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
		Log:          &bytes.Buffer{},
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := heartbeats.Load(); got != 0 {
		t.Errorf("keep_warm: false fired %d heartbeat(s)", got)
	}
}

// TestExecutor_WarmCapabilities_ParallelEnsure: --warm fires every declared
// capability up front — LLM capabilities get a streaming 1-token probe
// against their resolved model, non-OpenAI backends (comfyui) stop at
// activation, and the log reports one line per capability.
func TestExecutor_WarmCapabilities_ParallelEnsure(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "m-reason"}, {"id": "m-vision"},
		}})
	})
	var mu sync.Mutex
	var warmed []string
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body chatProbeBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream || body.MaxTokens != 1 {
			t.Errorf("warm probe must be stream=true max_tokens=1, got %+v", body)
		}
		mu.Lock()
		warmed = append(warmed, body.Model)
		mu.Unlock()
		serveChat(w, body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	log := &bytes.Buffer{}
	exec := &Executor{
		Pipeline: &Pipeline{
			Name: "warmall",
			Stages: []Stage{
				{ID: "a", Capability: "reasoning", Prompt: "x", Output: "a.txt"},
				{ID: "b", Capability: "vision", Prompt: "y", Output: "b.txt"},
				{ID: "c", Type: StageTypeComfyUI, Capability: "image_gen", Workflow: "w.json", Output: "c.png"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{
			"reasoning": {Profile: "m-reason"},
			"vision":    {Profile: "m-vision"},
			"image_gen": {Profile: "sdxl"},
		}},
		Vibe:   vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir: t.TempDir(),
		Log:    log,
	}
	if err := exec.WarmCapabilities(context.Background()); err != nil {
		t.Fatalf("WarmCapabilities: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), warmed...)
	mu.Unlock()
	sort.Strings(got)
	want := []string{"m-reason", "m-vision"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("warmed models = %v, want %v", got, want)
	}
	out := log.String()
	for _, snippet := range []string{
		`warm "reasoning": backend "m-reason" model "m-reason" ready (resolve`,
		`warm "vision": backend "m-vision" model "m-vision" ready (resolve`,
		`warm "image_gen": backend "sdxl" active`,
		"no generate probe",
	} {
		if !strings.Contains(out, snippet) {
			t.Errorf("log missing %q; log:\n%s", snippet, out)
		}
	}
}

func TestExecutor_WarmCapabilities_UnmappedCapabilityFails(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	exec := &Executor{
		Pipeline: &Pipeline{
			Name:   "warmfail",
			Stages: []Stage{{ID: "a", Capability: "nope", Prompt: "x", Output: "a.txt"}},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
		Log:          &bytes.Buffer{},
	}
	err := exec.WarmCapabilities(context.Background())
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("err = %v, want unmapped-capability error naming \"nope\"", err)
	}
}
