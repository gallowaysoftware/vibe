package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// stubControl is a minimal ControlServiceHandler that satisfies vamp's needs:
// Status reflects the most recent Start, and EnsureActive's Start path lands
// here.
type stubControl struct {
	mu       sync.Mutex
	profile  string
	proxyURL string
}

func (s *stubControl) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&vibev1.StatusResponse{Status: &vibev1.Status{
		Running: s.profile != "", Ready: s.profile != "", Profile: s.profile, ProxyAddr: s.proxyURL,
	}}), nil
}
func (s *stubControl) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	s.mu.Lock()
	s.profile = req.Msg.Profile
	s.mu.Unlock()
	r, _ := s.Status(nil, nil)
	return connect.NewResponse(&vibev1.StartResponse{Status: r.Msg.Status}), nil
}
func (s *stubControl) Stop(_ context.Context, _ *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	s.mu.Lock()
	s.profile = ""
	s.mu.Unlock()
	return connect.NewResponse(&vibev1.StopResponse{Status: &vibev1.Status{}}), nil
}
func (s *stubControl) ListProfiles(context.Context, *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	return connect.NewResponse(&vibev1.ListProfilesResponse{}), nil
}
func (s *stubControl) Shutdown(context.Context, *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	return connect.NewResponse(&vibev1.ShutdownResponse{}), nil
}
func (s *stubControl) Logs(context.Context, *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return connect.NewResponse(&vibev1.LogsResponse{}), nil
}
func (s *stubControl) Pull(_ context.Context, _ *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	return stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_DONE})
}

func TestExecutor_SequentialStagesShareOutputs(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Echo the prompt back so we can verify what each stage sent.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs := body["messages"].([]any)
		prompt := msgs[0].(map[string]any)["content"].(string)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ECHO[" + prompt + "]"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	runDir := t.TempDir()
	pipeline := &Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "topic: {{.inputs.topic}}", Output: "plan.txt"},
			{ID: "script", Capability: "reasoning", Prompt: "from plan: {{.stages.plan.output}}", Inputs: []string{"plan"}, Output: "script.txt"},
		},
	}

	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client()),
		Inputs:       map[string]string{"topic": "robots"},
		RunDir:       runDir,
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	plan, _ := os.ReadFile(filepath.Join(runDir, "plan.txt"))
	if got := string(plan); got != "ECHO[topic: robots]" {
		t.Errorf("plan = %q", got)
	}
	script, _ := os.ReadFile(filepath.Join(runDir, "script.txt"))
	if !strings.Contains(string(script), "ECHO[from plan: ECHO[topic: robots]]") {
		t.Errorf("script = %q", script)
	}
}

func TestExecutor_MissingCapabilityErrors(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	exec := &Executor{
		Pipeline: &Pipeline{
			Name: "t",
			Stages: []Stage{{ID: "a", Capability: "vision", Prompt: "x", Output: "a.txt"}},
		},
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client()),
		RunDir:       t.TempDir(),
	}
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("err = %v", err)
	}
}

func TestExecutor_StreamsTokensToLog(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, _ := body["stream"].(bool); !s {
			t.Errorf("expected stream=true, got body=%v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		writeChunk := func(content string) {
			payload, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{{"delta": map[string]string{"content": content}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// First event: role-only delta (should be skipped by the parser).
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		writeChunk("Hello")
		writeChunk(" world")
		writeChunk("!")
		// Empty-content final stop event (should be skipped).
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":""}}]}`+"\n\n")
		// Heartbeat / keepalive (should be skipped).
		fmt.Fprint(w, "data:\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	var logBuf bytes.Buffer
	runDir := t.TempDir()
	exec := &Executor{
		Pipeline: &Pipeline{
			Name: "stream",
			Stages: []Stage{
				{ID: "only", Capability: "reasoning", Prompt: "hi", Output: "only.txt"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]string{"reasoning": "code"}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client()),
		RunDir:       runDir,
		Log:          &logBuf,
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "Hello world!") {
		t.Errorf("expected streamed tokens %q in log, got:\n%s", "Hello world!", logged)
	}
	// Confirm fragments were written individually (the "Hello" delta should
	// appear before " world").
	if i, j := strings.Index(logged, "Hello"), strings.Index(logged, " world"); i < 0 || j < 0 || i >= j {
		t.Errorf("expected 'Hello' to appear before ' world' in log:\n%s", logged)
	}

	out, err := os.ReadFile(filepath.Join(runDir, "only.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "Hello world!" {
		t.Errorf("output file = %q, want %q", got, "Hello world!")
	}
}

func TestExecutor_JSONOutputFormatRejectsBadJSON(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "not json"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	exec := &Executor{
		Pipeline: &Pipeline{
			Name: "t",
			Stages: []Stage{
				{ID: "a", Capability: "r", Prompt: "x", Output: "a.json", OutputFormat: "json"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]string{"r": "code"}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client()),
		RunDir:       t.TempDir(),
	}
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("err = %v", err)
	}
}
