package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	vampcache "github.com/gallowaysoftware/vibe/internal/vamp/cache"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// stubControl is a minimal ControlServiceHandler that satisfies vamp's needs:
// Status reflects the most recent Start, and EnsureActive's Start path lands
// here. parallel, when non-zero, is surfaced as Status.Parallel so tests can
// exercise the foreach concurrency cap that's bounded by the active profile's
// llama_server parallel-slot count.
type stubControl struct {
	mu       sync.Mutex
	profile  string
	proxyURL string
	parallel int32
	stops    int32 // count of Stop calls (FreeProfileAfter assertions)
}

func (s *stubControl) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&vibev1.StatusResponse{Status: &vibev1.Status{
		Running: s.profile != "", Ready: s.profile != "", Profile: s.profile, ProxyAddr: s.proxyURL, Parallel: s.parallel,
	}}), nil
}
func (s *stubControl) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Capability activation sends Backend; interactive/profile activation sends
	// Profile. Either becomes the active identity reported by Status.
	if req.Msg.Backend != "" {
		s.profile = req.Msg.Backend
	} else {
		s.profile = req.Msg.Profile
	}
	// Build the response under the same lock so parallel Starts (the --warm
	// path activates every capability concurrently) each see their own
	// profile in the response instead of whichever Start landed last.
	return connect.NewResponse(&vibev1.StartResponse{Status: &vibev1.Status{
		Running: true, Ready: true, Profile: s.profile, ProxyAddr: s.proxyURL, Parallel: s.parallel,
	}}), nil
}
func (s *stubControl) Stop(_ context.Context, _ *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	s.mu.Lock()
	s.profile = ""
	s.stops++
	s.mu.Unlock()
	return connect.NewResponse(&vibev1.StopResponse{Status: &vibev1.Status{}}), nil
}

func (s *stubControl) stopCount() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
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
func (s *stubControl) CellDrain(context.Context, *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no cell verbs"))
}
func (s *stubControl) CellResume(context.Context, *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no cell verbs"))
}
func (s *stubControl) CellSuspend(context.Context, *connect.Request[vibev1.CellSuspendRequest]) (*connect.Response[vibev1.CellSuspendResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no cell verbs"))
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

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
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
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
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

// TestExecutor_FreeProfileAfter verifies that a text stage marked
// FreeProfileAfter causes the runner to Stop the active profile after that
// stage's capability group completes — and that an unmarked pipeline never
// stops the profile mid-run. This is the LLM analogue of the ComfyUI
// FreeMemoryAfter wiring: the lever that keeps a big LLM from staying
// resident while a downstream non-LLM GPU phase (TTS, image gen) runs.
func TestExecutor_FreeProfileAfter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		freeAfter bool
		wantStops int32
	}{
		{"marked frees profile", true, 1},
		{"unmarked leaves profile", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubControl{}
			mux := http.NewServeMux()
			path, handler := vibev1connect.NewControlServiceHandler(stub)
			mux.Handle(path, handler)
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
			})
			mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
				})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			stub.proxyURL = srv.URL

			// plan -> script (script depends on plan, so each is its own
			// single-stage group). The LAST stage carries the marker.
			exec := &Executor{
				Pipeline: &Pipeline{
					Name: "test",
					Stages: []Stage{
						{ID: "plan", Capability: "reasoning", Prompt: "x", Output: "plan.txt"},
						{ID: "script", Capability: "reasoning", Prompt: "y", Inputs: []string{"plan"}, Output: "script.txt", FreeProfileAfter: tc.freeAfter},
					},
				},
				Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
				Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
				RunDir:       t.TempDir(),
			}
			if err := exec.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := stub.stopCount(); got != tc.wantStops {
				t.Errorf("Stop calls = %d, want %d", got, tc.wantStops)
			}
		})
	}
}

func TestExecutor_MissingCapabilityErrors(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	exec := &Executor{
		Pipeline: &Pipeline{
			Name:   "t",
			Stages: []Stage{{ID: "a", Capability: "vision", Prompt: "x", Output: "a.txt"}},
		},
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
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
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
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
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"r": {Profile: "code"}}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
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

// stubExecutor wires up a real *Executor with a stub control plane so the
// scheduler exercises its real EnsureActive/Status paths. Inference is left
// mocked by the caller so tests can control latency and content.
func stubExecutor(t *testing.T, pipeline *Pipeline, caps *Capabilities, inf InferenceFunc) (*Executor, string) {
	t.Helper()
	ex, runDir, _ := stubExecutorWithControl(t, pipeline, caps, inf)
	return ex, runDir
}

// stubExecutorWithControl is the variant that also returns the underlying
// stubControl so tests can program profile-derived Status fields (notably
// Parallel for the foreach-cap test). Other tests stick with the simpler
// stubExecutor helper.
func stubExecutorWithControl(t *testing.T, pipeline *Pipeline, caps *Capabilities, inf InferenceFunc) (*Executor, string, *stubControl) {
	t.Helper()
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	stub.proxyURL = srv.URL
	runDir := t.TempDir()
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       runDir,
		Inference:    inf,
	}
	return exec, runDir, stub
}

func TestExecutor_ParallelStagesSameProfile(t *testing.T) {
	const stageLatency = 200 * time.Millisecond
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		select {
		case <-time.After(stageLatency):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "out:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "parallel",
		Stages: []Stage{
			{ID: "a", Capability: "reasoning", Prompt: "A", Output: "a.txt"},
			{ID: "b", Capability: "reasoning", Prompt: "B {{.stages.a.output}}", Inputs: []string{"a"}, Output: "b.txt"},
			{ID: "c", Capability: "reasoning", Prompt: "C {{.stages.a.output}}", Inputs: []string{"a"}, Output: "c.txt"},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	start := time.Now()
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Wall-clock: a runs alone (~200ms) then b+c run concurrently (~200ms)
	// for a parallel total of ~400ms. Fully sequential would be ~600ms.
	// 500ms gives ~25% scheduler headroom on a busy CI host.
	if elapsed >= 500*time.Millisecond {
		t.Errorf("parallel run took %s, expected < 500ms (sequential baseline ~600ms)", elapsed)
	}
	t.Logf("parallel-same-profile wall-clock: %s", elapsed)

	for name, want := range map[string]string{
		"a.txt": "out:A",
		"b.txt": "out:B out:A",
		"c.txt": "out:C out:A",
	} {
		got, err := os.ReadFile(filepath.Join(runDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestExecutor_ParallelGroupedByCapability(t *testing.T) {
	// activations records EnsureActive transitions on the stub control plane.
	// We can observe profile changes via stubControl.profile after each
	// inference call by recording snapshots from inside the mocked inference
	// function itself.
	var (
		mu          sync.Mutex
		activations []string
	)
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		// Pull the active profile via the /v1/models route's host; here we
		// rely on the prompt prefix to discriminate. We instead record
		// invocation order via a stage tag in the prompt.
		mu.Lock()
		activations = append(activations, prompt)
		mu.Unlock()
		return "ok:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reason": {Profile: "code"}, "write": {Profile: "fast"}}}
	pipeline := &Pipeline{
		Name: "grouped",
		Stages: []Stage{
			{ID: "a", Capability: "reason", Prompt: "A", Output: "a.txt"},
			{ID: "b", Capability: "reason", Prompt: "B {{.stages.a.output}}", Inputs: []string{"a"}, Output: "b.txt"},
			{ID: "c", Capability: "write", Prompt: "C {{.stages.a.output}}", Inputs: []string{"a"}, Output: "c.txt"},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// We expect three invocations total. The first must be "A" (wave 1).
	// The next two are "B ..." and "C ..." in some order — but they must
	// run serially because they belong to different capabilities and a
	// profile swap can't be parallelized.
	mu.Lock()
	defer mu.Unlock()
	if len(activations) != 3 {
		t.Fatalf("expected 3 inference calls, got %d (%v)", len(activations), activations)
	}
	if activations[0] != "A" {
		t.Errorf("first call should be A, got %q", activations[0])
	}
	rest := []string{activations[1], activations[2]}
	wantB, wantC := "B ok:A", "C ok:A"
	if (rest[0] != wantB || rest[1] != wantC) && (rest[0] != wantC || rest[1] != wantB) {
		t.Errorf("wave 2 ordering = %v, expected some permutation of [%q,%q]", rest, wantB, wantC)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, err := os.ReadFile(filepath.Join(runDir, name)); err != nil {
			t.Errorf("missing output %s: %v", name, err)
		}
	}
}

func TestExecutor_PerStageBufferingInParallel(t *testing.T) {
	// Each "token" sleeps briefly so the goroutines would interleave at
	// the byte level if buffering didn't isolate them.
	emit := func(prefix string, onToken StreamFunc, count int) string {
		var acc strings.Builder
		for i := 0; i < count; i++ {
			tok := strings.Repeat(prefix, 1)
			if onToken != nil {
				onToken(tok)
			}
			acc.WriteString(tok)
			time.Sleep(10 * time.Millisecond)
		}
		return acc.String()
	}
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch prompt {
		case "A":
			return emit("A", onToken, 3), nil
		case "B":
			return emit("B", onToken, 3), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	// Both stages must run in the SAME wave (no inputs) and SAME capability
	// to land in a multi-stage parallel group, which triggers buffering.
	pipeline := &Pipeline{
		Name: "buffered",
		Stages: []Stage{
			{ID: "stage_a", Capability: "reasoning", Prompt: "A", Output: "a.txt"},
			{ID: "stage_b", Capability: "reasoning", Prompt: "B", Output: "b.txt"},
		},
	}
	var logBuf bytes.Buffer
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	exec.Log = &logBuf
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	logged := logBuf.String()
	idxAHeader := strings.Index(logged, "=== stage stage_a ===")
	idxBHeader := strings.Index(logged, "=== stage stage_b ===")
	if idxAHeader < 0 || idxBHeader < 0 {
		t.Fatalf("expected both stage headers in log, got:\n%s", logged)
	}
	// The body for each stage must be contiguous "AAA" / "BBB" — no
	// interleaving. Find the first "AAA" / "BBB" occurrence and check.
	if !strings.Contains(logged, "AAA") {
		t.Errorf("expected contiguous AAA in log:\n%s", logged)
	}
	if !strings.Contains(logged, "BBB") {
		t.Errorf("expected contiguous BBB in log:\n%s", logged)
	}
	// And specifically: the stage_a body region should not contain a B,
	// and the stage_b body region should not contain an A. We bound the
	// body region as the bytes between this header and the next header
	// (or end-of-log).
	bodyBetween := func(start int, headerLen int) string {
		region := logged[start+headerLen:]
		// Skip the newline after the header.
		region = strings.TrimPrefix(region, "\n")
		// Body runs until the next "=== stage" marker.
		if next := strings.Index(region, "=== stage "); next >= 0 {
			region = region[:next]
		}
		return region
	}
	// Trim the body to just the token-output line (first line after the
	// header) — anything after that is unrelated scheduler logf output
	// such as the "pipeline finished" status line that may legitimately
	// contain other letters.
	tokenLine := func(body string) string {
		body = strings.TrimPrefix(body, "\n")
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			return body[:nl]
		}
		return body
	}
	aTokens := tokenLine(bodyBetween(idxAHeader, len("=== stage stage_a ===")))
	bTokens := tokenLine(bodyBetween(idxBHeader, len("=== stage stage_b ===")))
	if aTokens != "AAA" {
		t.Errorf("stage_a token line = %q, want exactly %q (interleaving?)", aTokens, "AAA")
	}
	if bTokens != "BBB" {
		t.Errorf("stage_b token line = %q, want exactly %q (interleaving?)", bTokens, "BBB")
	}
}

func TestExecutor_DAGErrorAggregation(t *testing.T) {
	var calls int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		atomic.AddInt32(&calls, 1)
		// Sleep briefly so both goroutines are guaranteed to be in flight
		// before either returns; otherwise the first failure could cancel
		// the second before it runs and we'd only see one error.
		time.Sleep(50 * time.Millisecond)
		return "", fmt.Errorf("boom from %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "fail",
		Stages: []Stage{
			{ID: "stage_x", Capability: "reasoning", Prompt: "X", Output: "x.txt"},
			{ID: "stage_y", Capability: "reasoning", Prompt: "Y", Output: "y.txt"},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stage_x") || !strings.Contains(msg, "stage_y") {
		t.Errorf("expected both stage ids in aggregated error, got: %v", err)
	}
	// errors.Join is what we used; ensure unwrapping behaves.
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Logf("note: top-level error did not unwrap to []error; that's fine if the join is nested")
	}
}

// TestExecutor_ForeachFansOut verifies a foreach consumer runs once per item
// in its producer's JSON array, writes one file per item, and exposes the
// per-item outputs to downstream stages via .stages.<id>.outputs.
func TestExecutor_ForeachFansOut(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		// Two roles: the producer prompt asks for "TITLES" and gets back a
		// JSON array; the consumer prompts are of the shape "UP: <item>" and
		// uppercase the embedded item; the joiner prompt embeds the slice of
		// outputs so we can confirm `.outputs` resolves to a []string.
		switch {
		case prompt == "TITLES":
			return `["a","b","c"]`, nil
		case strings.HasPrefix(prompt, "UP:"):
			return strings.ToUpper(strings.TrimPrefix(prompt, "UP:")), nil
		case strings.HasPrefix(prompt, "JOIN:"):
			return strings.TrimPrefix(prompt, "JOIN:"), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "fan",
		Stages: []Stage{
			{
				ID: "titles", Capability: "reasoning",
				Prompt: "TITLES", Output: "titles.json", OutputFormat: "json",
			},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Prompt:  "UP:{{.title}}",
				Output:  "items/{{.title | slugify}}.txt",
			},
			{
				ID: "joiner", Capability: "reasoning",
				Inputs: []string{"consumer"},
				// Range over .outputs to prove it's a []string slice.
				Prompt: "JOIN:{{range $i, $o := .stages.consumer.outputs}}{{if $i}}|{{end}}{{$o}}{{end}}",
				Output: "joined.txt",
			},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Per-item files. The producer emits ["a","b","c"], so consumer writes
	// items/a.txt, items/b.txt, items/c.txt with contents A,B,C.
	for _, tc := range []struct{ path, want string }{
		{"items/a.txt", "A"},
		{"items/b.txt", "B"},
		{"items/c.txt", "C"},
	} {
		got, err := os.ReadFile(filepath.Join(runDir, tc.path))
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.path, got, tc.want)
		}
	}

	// Downstream .outputs must be a 3-element slice in input order. The
	// joiner stage receives "A|B|C" exactly.
	joined, err := os.ReadFile(filepath.Join(runDir, "joined.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(joined) != "A|B|C" {
		t.Errorf("joined output = %q, want %q", joined, "A|B|C")
	}
}

// TestExecutor_ForeachParseError verifies that a foreach consumer errors
// clearly when its producer's JSON-array template doesn't resolve to a JSON
// array.
func TestExecutor_ForeachParseError(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if prompt == "PRODUCE" {
			// Valid JSON but not an array; the foreach resolver must reject
			// this with a clear message rather than crashing on a type
			// assertion.
			return `"not an array"`, nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "fail",
		Stages: []Stage{
			{ID: "src", Capability: "reasoning", Prompt: "PRODUCE", Output: "src.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"src"},
				Foreach: &ForeachSpec{From: "src", Var: "item"},
				Prompt:  "irrelevant",
				Output:  "out/{{.item}}.txt",
			},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "foreach") || !strings.Contains(msg, "array") {
		t.Errorf("expected foreach/array in error, got: %v", err)
	}
}

// TestExecutor_ForeachSingleItemStaticOutput verifies that a foreach stage
// whose upstream resolves to exactly ONE item accepts a non-templated
// Stage.Output. The static {{...}}-required check used to live in Validate;
// it was moved to runtime so the single-item case (where there's nothing to
// collide with) loads and runs cleanly. Regression test for the morning
// smoke "foreach with single item still requires templated output" bug.
func TestExecutor_ForeachSingleItemStaticOutput(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ONE":
			// Single-element array — the moved-to-runtime check sees
			// len(items) == 1 and must NOT reject the static Output.
			return `["solo"]`, nil
		case strings.HasPrefix(prompt, "UP:"):
			return strings.ToUpper(strings.TrimPrefix(prompt, "UP:")), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "solo",
		Stages: []Stage{
			{
				ID: "titles", Capability: "reasoning",
				Prompt: "ONE", Output: "titles.json", OutputFormat: "json",
			},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Prompt:  "UP:{{.title}}",
				// Intentionally non-templated: a single-item foreach
				// cannot collide with itself, so this MUST be accepted.
				Output: "only.txt",
			},
		},
	}
	// Validate first — the static {{ check is gone, so this pipeline must
	// load cleanly. (Previously this would have failed with
	// "templated output path" before Run was reached.)
	if err := pipeline.Validate(); err != nil {
		t.Fatalf("Validate: %v (single-item foreach with static output must pass static validation now)", err)
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(runDir, "only.txt"))
	if err != nil {
		t.Fatalf("read only.txt: %v", err)
	}
	if string(got) != "SOLO" {
		t.Errorf("only.txt = %q, want %q", got, "SOLO")
	}
}

// TestExecutor_ForeachMultiItemStaticOutputRejected verifies that the
// validator-message-preserving runtime check still fires when the foreach
// upstream actually resolves to >1 items and the Output template is static.
// The error message MUST match the original validator wording so users who
// hit the rejection see the same hint they used to.
func TestExecutor_ForeachMultiItemStaticOutputRejected(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if prompt == "MANY" {
			return `["a","b","c"]`, nil
		}
		t.Errorf("consumer must not run when static-output collision is detected, got prompt %q", prompt)
		return "", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "static_collide",
		Stages: []Stage{
			{
				ID: "titles", Capability: "reasoning",
				Prompt: "MANY", Output: "titles.json", OutputFormat: "json",
			},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Prompt:  "x",
				Output:  "all.txt", // 3 items + static output = guaranteed collision
			},
		},
	}
	// Static validation must accept this; the rejection is runtime now.
	if err := pipeline.Validate(); err != nil {
		t.Fatalf("Validate: %v (static check is deferred; this must load and only fail at runtime)", err)
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected runtime rejection of multi-item foreach with static output")
	}
	// The runtime message MUST contain the original validator wording so
	// users hitting this for the first time read the same hint.
	if !strings.Contains(err.Error(), "templated output path") {
		t.Errorf("error must surface the original validator wording, got: %v", err)
	}
}

// TestExecutor_ForeachOutputCollision verifies that two items whose templated
// output paths collide cause an error before any inference runs.
func TestExecutor_ForeachOutputCollision(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		// Producer returns two items that slugify identically; the consumer
		// must never be invoked because collision detection happens during
		// per-item pre-rendering.
		if prompt == "PRODUCE" {
			return `["Hello, world!","hello world"]`, nil
		}
		t.Errorf("consumer should not run on collision, but got prompt %q", prompt)
		return "", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "collide",
		Stages: []Stage{
			{ID: "src", Capability: "reasoning", Prompt: "PRODUCE", Output: "src.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"src"},
				Foreach: &ForeachSpec{From: "src", Var: "title"},
				Prompt:  "hi {{.title}}",
				Output:  "out/{{.title | slugify}}.txt",
			},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("expected collision in error, got: %v", err)
	}
}

// TestExecutor_ParallelForeach_RunsInParallel verifies that a foreach stage
// runs items concurrently when MaxForeachConcurrency >= n. Wall-clock for 4
// items each sleeping 200ms should land near 200ms (parallel) and well under
// the 800ms sequential baseline.
func TestExecutor_ParallelForeach_RunsInParallel(t *testing.T) {
	const itemLatency = 200 * time.Millisecond
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["a","b","c","d"]`, nil
		case strings.HasPrefix(prompt, "ITEM:"):
			select {
			case <-time.After(itemLatency):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return strings.TrimPrefix(prompt, "ITEM:"), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "par",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "ITEM:{{.x}}",
				Output:  "out/{{.x}}.txt",
			},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	exec.MaxForeachConcurrency = 4
	start := time.Now()
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// Sequential lower bound = 4*200ms = 800ms. With concurrency 4 the foreach
	// itself should be ~200ms; add the producer stage's near-zero latency.
	// 400ms gives plenty of CI headroom while still failing on a sequential
	// regression.
	if elapsed >= 400*time.Millisecond {
		t.Errorf("parallel foreach took %s, expected < 400ms (sequential baseline ~800ms)", elapsed)
	}
	t.Logf("parallel-foreach wall-clock: %s", elapsed)
}

// TestExecutor_ParallelForeach_RespectsCap verifies that MaxForeachConcurrency
// actually caps in-flight items. 8 items at 200ms each with cap=2 should run
// in 4 batches of 2, landing near 800ms (not the 200ms a cap of 8 would
// produce, and not the 1.6s a cap of 1 would).
func TestExecutor_ParallelForeach_RespectsCap(t *testing.T) {
	const itemLatency = 200 * time.Millisecond
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["a","b","c","d","e","f","g","h"]`, nil
		case strings.HasPrefix(prompt, "ITEM:"):
			select {
			case <-time.After(itemLatency):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return strings.TrimPrefix(prompt, "ITEM:"), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "cap",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "ITEM:{{.x}}",
				Output:  "out/{{.x}}.txt",
			},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	exec.MaxForeachConcurrency = 2
	start := time.Now()
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// 8 items / 2 in flight = 4 batches * 200ms = 800ms nominal. Allow > 700ms
	// to prove the cap actually serializes (cap=8 would finish in ~200ms) and
	// < 1000ms to prove the cap doesn't degrade to fully sequential.
	if elapsed <= 700*time.Millisecond {
		t.Errorf("cap=2 finished in %s, expected > 700ms (would imply cap not enforced)", elapsed)
	}
	if elapsed >= 1000*time.Millisecond {
		t.Errorf("cap=2 finished in %s, expected < 1000ms (would imply too-tight serialization)", elapsed)
	}
	t.Logf("cap=2 wall-clock for 8 items: %s", elapsed)
}

// TestExecutor_ParallelForeach_IndependentItems verifies that an erroring item
// does NOT cancel its siblings. Each item runs independently; a single failure
// is collected but the remaining items complete normally. The stage aggregates
// partial output from the successful items and reports the joined error.
func TestExecutor_ParallelForeach_IndependentItems(t *testing.T) {
	const longLatency = 100 * time.Millisecond
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["fail","slow1","slow2","slow3"]`, nil
		case prompt == "ITEM:fail":
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "", fmt.Errorf("intentional failure")
		case strings.HasPrefix(prompt, "ITEM:"):
			select {
			case <-time.After(longLatency):
				return strings.TrimPrefix(prompt, "ITEM:"), nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "independent",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "ITEM:{{.x}}",
				Output:  "out/{{.x}}.txt",
			},
		},
	}
	exec, dir := stubExecutor(t, pipeline, caps, inf)
	exec.MaxForeachConcurrency = 4
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "intentional failure") {
		t.Errorf("expected 'intentional failure' in aggregated error, got: %v", err)
	}
	// Siblings should have completed successfully despite item 0 failing.
	// Check that the 3 successful items wrote their output files.
	for _, name := range []string{"slow1.txt", "slow2.txt", "slow3.txt"} {
		data, err := os.ReadFile(filepath.Join(dir, "out", name))
		if err != nil {
			t.Errorf("expected output file out/%s after sibling completion: %v", name, err)
		} else if string(data) != strings.TrimPrefix(name, "")[:len(name)-4] {
			t.Errorf("out/%s = %q, want %q", name, string(data), strings.TrimPrefix(name, "")[:len(name)-4])
		}
	}
	// The failing item's output should not exist.
	if _, err := os.Stat(filepath.Join(dir, "out", "fail.txt")); !os.IsNotExist(err) {
		t.Error("expected out/fail.txt to not exist after item failure")
	}
}

// TestExecutor_ParallelForeach_OutputsInDeclaredOrder verifies that even when
// items finish in reverse arrival order, stageResult.Outputs (and the
// templated `.outputs` slice it powers) is keyed by input-array index, not by
// finish time.
func TestExecutor_ParallelForeach_OutputsInDeclaredOrder(t *testing.T) {
	// Latencies arranged so item 0 finishes LAST and item 3 finishes FIRST.
	latencies := []time.Duration{300 * time.Millisecond, 200 * time.Millisecond, 100 * time.Millisecond, 50 * time.Millisecond}
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["0","1","2","3"]`, nil
		case strings.HasPrefix(prompt, "ITEM:"):
			tok := strings.TrimPrefix(prompt, "ITEM:")
			idx, err := strconv.Atoi(tok)
			if err != nil {
				return "", err
			}
			select {
			case <-time.After(latencies[idx]):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "out-" + tok, nil
		case strings.HasPrefix(prompt, "JOIN:"):
			return strings.TrimPrefix(prompt, "JOIN:"), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "order",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "ITEM:{{.x}}",
				Output:  "out/{{.x}}.txt",
			},
			{
				ID: "joiner", Capability: "reasoning",
				Inputs: []string{"consumer"},
				Prompt: "JOIN:{{range $i, $o := .stages.consumer.outputs}}{{if $i}}|{{end}}{{$o}}{{end}}",
				Output: "joined.txt",
			},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	exec.MaxForeachConcurrency = 4
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(runDir, "joined.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "out-0|out-1|out-2|out-3"
	if string(got) != want {
		t.Errorf(".stages.consumer.outputs = %q, want %q (must be input-array order, not finish order)", got, want)
	}
}

func TestExecutor_DAGCycleDetected(t *testing.T) {
	yaml := `name: cyc
stages:
- id: a
  capability: r
  prompt: x
  inputs: [b]
  output: a.md
- id: b
  capability: r
  prompt: y
  inputs: [a]
  output: b.md`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
	}
}

// resumePipelineSource is the canonical pipeline YAML used by the resume
// suite. It's intentionally short and stable so the snapshot-hash drift test
// has a known "different" string to compare against.
const resumePipelineSource = `name: resume
stages:
- id: one
  capability: reasoning
  prompt: P1
  output: one.txt
- id: two
  capability: reasoning
  prompt: P2 {{.stages.one.output}}
  inputs: [one]
  output: two.txt
`

// resumePipeline is the parsed form of resumePipelineSource. Keep these in
// sync: the tests rely on the snapshot bytes matching what the Executor sees.
func resumePipeline() *Pipeline {
	return &Pipeline{
		Name: "resume",
		Stages: []Stage{
			{ID: "one", Capability: "reasoning", Prompt: "P1", Output: "one.txt"},
			{ID: "two", Capability: "reasoning", Prompt: "P2 {{.stages.one.output}}", Inputs: []string{"one"}, Output: "two.txt"},
		},
	}
}

// TestExecutor_ResumeSkipsCompletedStages confirms that a pre-existing
// stage-1 output in the run dir is reused on the second run: the inference
// function must NOT be called for stage "one", but MUST be called for
// stage "two" (whose output is absent).
func TestExecutor_ResumeSkipsCompletedStages(t *testing.T) {
	var called sync.Map
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		// Record every invocation by prompt so the assertion below is
		// independent of stage scheduling order.
		called.Store(prompt, true)
		return "ran:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}

	// Set up a run dir that already has stage "one"'s output from a
	// hypothetical prior run, plus the original pipeline snapshot.
	exec, runDir := stubExecutor(t, resumePipeline(), caps, inf)
	if err := os.WriteFile(filepath.Join(runDir, "one.txt"), []byte("prior-one-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(resumePipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(resumePipelineSource)
	exec.ResumeDir = runDir
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}

	if _, ran := called.Load("P1"); ran {
		t.Errorf("stage one inference was called despite output file existing")
	}
	// Stage two depends on stage one's output. The resumed contents must
	// flow into stage two's prompt, so the recorded inference prompt is
	// the freshly-rendered "P2 prior-one-output".
	if _, ran := called.Load("P2 prior-one-output"); !ran {
		t.Errorf("stage two inference was not called with the resumed upstream output (saw calls: %v)", dumpSyncMapKeys(&called))
	}
	got, err := os.ReadFile(filepath.Join(runDir, "two.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ran:P2 prior-one-output" {
		t.Errorf("two.txt = %q, want %q", got, "ran:P2 prior-one-output")
	}
}

// dumpSyncMapKeys returns the keys of a sync.Map for use in error messages.
func dumpSyncMapKeys(m *sync.Map) []string {
	var out []string
	m.Range(func(k, _ any) bool {
		out = append(out, fmt.Sprint(k))
		return true
	})
	return out
}

// foreachResumePipeline returns a 4-item foreach pipeline reused by the
// per-item resume tests below. The upstream "titles" stage emits a static
// JSON array so the resume helper can rely on a stable item order; the
// consumer stage's Output template embeds {{.title}} so each per-item file
// has a unique on-disk name.
func foreachResumePipeline() *Pipeline {
	return &Pipeline{
		Name: "fan",
		Stages: []Stage{
			{ID: "titles", Capability: "reasoning", Prompt: "TITLES", Output: "titles.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Prompt:  "UP:{{.title}}",
				Output:  "items/{{.title | slugify}}.txt",
			},
		},
	}
}

// foreachResumeInfFn is the inference stub shared by the per-item resume
// tests: it returns a fixed JSON array for the upstream stage and uppercases
// each consumer item's token, recording every consumer invocation by token
// into calls so the assertions can verify which items actually ran.
func foreachResumeInfFn(calls *sync.Map) InferenceFunc {
	return func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if prompt == "TITLES" {
			return `["a","b","c","d"]`, nil
		}
		if strings.HasPrefix(prompt, "UP:") {
			tok := strings.TrimPrefix(prompt, "UP:")
			calls.Store(tok, true)
			return strings.ToUpper(tok), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
}

// TestExecutor_ResumeForeach_PartialPresentRerunsMissingOnly verifies that a
// foreach stage with some per-item outputs already on disk reruns ONLY the
// missing items on resume — the previously-completed ones are loaded straight
// from disk into stageResult.Outputs at their original input-array index.
func TestExecutor_ResumeForeach_PartialPresentRerunsMissingOnly(t *testing.T) {
	var consumerCalls sync.Map
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := foreachResumePipeline()
	pipelineSource := "name: fan\n" // placeholder; resume's hash check just needs snapshot bytes to match PipelineSource
	exec, runDir := stubExecutor(t, pipeline, caps, foreachResumeInfFn(&consumerCalls))
	// Pre-populate upstream JSON output + 3 of 4 per-item files.
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b","c","d"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "items"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "items", "a.txt"), []byte("A-prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "items", "b.txt"), []byte("B-prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	// items/c.txt deliberately missing.
	if err := os.WriteFile(filepath.Join(runDir, "items", "d.txt"), []byte("D-prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(pipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(pipelineSource)
	exec.ResumeDir = runDir
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	// Only the missing item should have been invoked; the rest must have
	// resumed straight from disk.
	for _, tok := range []string{"a", "b", "d"} {
		if _, ran := consumerCalls.Load(tok); ran {
			t.Errorf("foreach item %q was rerun despite its output existing on disk", tok)
		}
	}
	if _, ran := consumerCalls.Load("c"); !ran {
		t.Errorf("expected missing foreach item %q to rerun, but it did not (saw: %v)", "c", dumpSyncMapKeys(&consumerCalls))
	}
	// The previously-missing c.txt must now exist with the freshly-computed
	// body.
	got, err := os.ReadFile(filepath.Join(runDir, "items", "c.txt"))
	if err != nil {
		t.Fatalf("expected items/c.txt to be written on rerun: %v", err)
	}
	if string(got) != "C" {
		t.Errorf("items/c.txt = %q, want %q", got, "C")
	}
	// Resumed items must NOT have been overwritten — their prior body
	// survives the rerun.
	for _, c := range []struct {
		path string
		want string
	}{
		{"items/a.txt", "A-prior"},
		{"items/b.txt", "B-prior"},
		{"items/d.txt", "D-prior"},
	} {
		got, err := os.ReadFile(filepath.Join(runDir, c.path))
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q (resumed file was overwritten)", c.path, got, c.want)
		}
	}
	// stageResult.Outputs must be input-array ordered: A-prior, B-prior, C, D-prior.
	exec.mu.Lock()
	res, ok := exec.stageOutputs["consumer"]
	exec.mu.Unlock()
	if !ok {
		t.Fatalf("consumer stage produced no stageResult")
	}
	want := []string{"A-prior", "B-prior", "C", "D-prior"}
	if got := res.Outputs; !reflect.DeepEqual(got, want) {
		t.Errorf("consumer.Outputs = %#v, want %#v", got, want)
	}
}

// TestExecutor_ResumeForeach_PerItem verifies the same partial-resume
// invariants on a 4-item foreach with 2 missing items in non-contiguous
// positions: only the missing indices run, the resumed slots keep their
// on-disk bodies, and the final per-item Outputs slice is in input-array
// order regardless of which items ran in this pass.
func TestExecutor_ResumeForeach_PerItem(t *testing.T) {
	var consumerCalls sync.Map
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := foreachResumePipeline()
	pipelineSource := "name: fan\n"
	exec, runDir := stubExecutor(t, pipeline, caps, foreachResumeInfFn(&consumerCalls))
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b","c","d"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "items"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Present: a (idx 0) and c (idx 2). Missing: b (idx 1) and d (idx 3).
	if err := os.WriteFile(filepath.Join(runDir, "items", "a.txt"), []byte("A-prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "items", "c.txt"), []byte("C-prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(pipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(pipelineSource)
	exec.ResumeDir = runDir
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	// Only b and d should have invoked the executor.
	for _, tok := range []string{"a", "c"} {
		if _, ran := consumerCalls.Load(tok); ran {
			t.Errorf("foreach item %q was rerun despite its output existing on disk", tok)
		}
	}
	for _, tok := range []string{"b", "d"} {
		if _, ran := consumerCalls.Load(tok); !ran {
			t.Errorf("expected missing foreach item %q to rerun, but it did not (saw: %v)", tok, dumpSyncMapKeys(&consumerCalls))
		}
	}
	// Outputs slice must stay input-array ordered: A-prior, B (rerun), C-prior, D (rerun).
	exec.mu.Lock()
	res, ok := exec.stageOutputs["consumer"]
	exec.mu.Unlock()
	if !ok {
		t.Fatalf("consumer stage produced no stageResult")
	}
	want := []string{"A-prior", "B", "C-prior", "D"}
	if got := res.Outputs; !reflect.DeepEqual(got, want) {
		t.Errorf("consumer.Outputs = %#v, want %#v", got, want)
	}
	// Resumed files survive intact; missing files were written with fresh bodies.
	for _, c := range []struct {
		path string
		want string
	}{
		{"items/a.txt", "A-prior"},
		{"items/b.txt", "B"},
		{"items/c.txt", "C-prior"},
		{"items/d.txt", "D"},
	} {
		got, err := os.ReadFile(filepath.Join(runDir, c.path))
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestExecutor_ResumeForeach_AllPresent verifies the all-resumed shortcut is
// preserved: when every per-item output is present on disk, the foreach
// stage's executor is never invoked, but its stageResult.Outputs is fully
// populated from the on-disk files.
func TestExecutor_ResumeForeach_AllPresent(t *testing.T) {
	var consumerCalls sync.Map
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := foreachResumePipeline()
	pipelineSource := "name: fan\n"
	exec, runDir := stubExecutor(t, pipeline, caps, foreachResumeInfFn(&consumerCalls))
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b","c","d"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "items"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ name, body string }{
		{"items/a.txt", "A-prior"},
		{"items/b.txt", "B-prior"},
		{"items/c.txt", "C-prior"},
		{"items/d.txt", "D-prior"},
	} {
		if err := os.WriteFile(filepath.Join(runDir, p.name), []byte(p.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(pipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(pipelineSource)
	exec.ResumeDir = runDir
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	// Not a single consumer call should have happened.
	for _, tok := range []string{"a", "b", "c", "d"} {
		if _, ran := consumerCalls.Load(tok); ran {
			t.Errorf("foreach item %q was rerun despite all outputs existing on disk", tok)
		}
	}
	// Outputs slice must reflect the on-disk bodies in input-array order.
	exec.mu.Lock()
	res, ok := exec.stageOutputs["consumer"]
	exec.mu.Unlock()
	if !ok {
		t.Fatalf("consumer stage produced no stageResult")
	}
	want := []string{"A-prior", "B-prior", "C-prior", "D-prior"}
	if got := res.Outputs; !reflect.DeepEqual(got, want) {
		t.Errorf("consumer.Outputs = %#v, want %#v", got, want)
	}
}

// TestExecutor_ResumeForeach_NonePresent verifies the all-missing branch is
// unchanged: when no per-item outputs exist on disk, every item runs through
// the executor exactly as a fresh run would (preserving the pre-per-item-
// resume behaviour for the "first crash on item 0" case).
func TestExecutor_ResumeForeach_NonePresent(t *testing.T) {
	var consumerCalls sync.Map
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := foreachResumePipeline()
	pipelineSource := "name: fan\n"
	exec, runDir := stubExecutor(t, pipeline, caps, foreachResumeInfFn(&consumerCalls))
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b","c","d"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	// No per-item files on disk.
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(pipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(pipelineSource)
	exec.ResumeDir = runDir
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	for _, tok := range []string{"a", "b", "c", "d"} {
		if _, ran := consumerCalls.Load(tok); !ran {
			t.Errorf("expected foreach item %q to run (no outputs on disk), but it did not", tok)
		}
	}
	// All per-item files now exist with fresh bodies.
	for _, c := range []struct {
		path string
		want string
	}{
		{"items/a.txt", "A"},
		{"items/b.txt", "B"},
		{"items/c.txt", "C"},
		{"items/d.txt", "D"},
	} {
		got, err := os.ReadFile(filepath.Join(runDir, c.path))
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestExecutor_ResumePipelineChangedRejects verifies the safety check fires
// when the pipeline file content differs from the snapshot left in the run
// dir. The error message must mention "pipeline file changed" so the CLI
// surface stays grep-able.
func TestExecutor_ResumePipelineChangedRejects(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		t.Errorf("inference should not be called when resume aborts on snapshot mismatch")
		return "", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	exec, runDir := stubExecutor(t, resumePipeline(), caps, inf)
	// Snapshot says the run started against version V1; current pipeline
	// source is V2 (a single extra comment line is enough to change the
	// SHA-256).
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(resumePipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(resumePipelineSource + "# extra comment\n")
	exec.ResumeDir = runDir
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected resume to reject mismatched pipeline file")
	}
	if !strings.Contains(err.Error(), "pipeline file changed") {
		t.Errorf("expected 'pipeline file changed' in error, got: %v", err)
	}
}

// TestExecutor_ResumeForceAllowsChanged verifies that --resume-force bypasses
// the snapshot drift check: the run proceeds, and any existing outputs are
// still reused.
func TestExecutor_ResumeForceAllowsChanged(t *testing.T) {
	var called sync.Map
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		called.Store(prompt, true)
		return "ran:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	exec, runDir := stubExecutor(t, resumePipeline(), caps, inf)
	// Pre-stage one's output exists; snapshot bytes differ from current
	// PipelineSource. Without --resume-force this would error.
	if err := os.WriteFile(filepath.Join(runDir, "one.txt"), []byte("prior-one-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(resumePipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(resumePipelineSource + "# divergent comment\n")
	exec.ResumeDir = runDir
	exec.ResumeForce = true
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume --resume-force run failed: %v", err)
	}
	// Stage one was already done, so its inference must NOT have been called.
	if _, ran := called.Load("P1"); ran {
		t.Errorf("stage one inference was called despite --resume-force seeing the existing output")
	}
	// Stage two ran fresh and consumed the resumed upstream output.
	if _, ran := called.Load("P2 prior-one-output"); !ran {
		t.Errorf("stage two inference was not called (saw: %v)", dumpSyncMapKeys(&called))
	}
}

// fastRetry is the per-test retry policy used by the retry suite. The
// numbers are deliberately tiny (1ms initial, 4ms cap) so the tests stay
// well under a second of wall-clock even when every retry-arm fires.
// Multiplier 2.0 mirrors the production default.
func fastRetry(maxAttempts int) *RetryPolicy {
	return &RetryPolicy{
		MaxAttempts:    maxAttempts,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     4 * time.Millisecond,
		Multiplier:     2.0,
		RetryOn:        []string{"transient"},
	}
}

// TestExecutor_RetriesTransientError verifies that a stage configured with
// max_attempts=3 succeeds when the executor returns a 503 on the first two
// attempts and success on the third.
func TestExecutor_RetriesTransientError(t *testing.T) {
	var attempts int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			// Match the production inference error shape so the 5xx
			// regex hits.
			return "", fmt.Errorf("chat completion 503 Service Unavailable: backend down")
		}
		return "ok", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "retry",
		Stages: []Stage{
			{ID: "a", Capability: "reasoning", Prompt: "X", Output: "a.txt", Retry: fastRetry(3)},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3 (initial + 2 retries)", got)
	}
	body, err := os.ReadFile(filepath.Join(runDir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Errorf("a.txt = %q, want %q", body, "ok")
	}
}

// TestExecutor_RespectsMaxAttempts verifies that the stage fails after
// exactly MaxAttempts tries when the underlying error is persistently
// transient.
func TestExecutor_RespectsMaxAttempts(t *testing.T) {
	var attempts int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		atomic.AddInt32(&attempts, 1)
		return "", fmt.Errorf("chat completion 502 Bad Gateway: upstream gone")
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "retry_cap",
		Stages: []Stage{
			{ID: "a", Capability: "reasoning", Prompt: "X", Output: "a.txt", Retry: fastRetry(2)},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (max_attempts cap)", got)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want underlying 502 message preserved", err)
	}
}

// TestExecutor_DoesNotRetryUserCancel verifies that a context.Canceled
// error short-circuits the retry loop: the executor is invoked exactly
// once and ctx.Err() is propagated.
func TestExecutor_DoesNotRetryUserCancel(t *testing.T) {
	var attempts int32
	ctx, cancel := context.WithCancel(context.Background())
	inf := func(_ context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		atomic.AddInt32(&attempts, 1)
		// Simulate the host ctx being canceled mid-inference. Returning
		// the bare ctx.Err() is the standard pattern; the retry wrapper
		// must short-circuit even though "context canceled" could
		// plausibly look like a network error to a naive matcher.
		cancel()
		return "", ctx.Err()
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "cancel_retry",
		Stages: []Stage{
			{ID: "a", Capability: "reasoning", Prompt: "X", Output: "a.txt", Retry: fastRetry(5)},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx.Canceled, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancel must not retry)", got)
	}
}

// TestExecutor_DoesNotRetryNonTransient verifies that a non-transient
// error (here, a JSON-validation failure that the text executor wraps
// after a successful inference) bypasses retry entirely.
func TestExecutor_DoesNotRetryNonTransient(t *testing.T) {
	var attempts int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		atomic.AddInt32(&attempts, 1)
		// Inference itself succeeds. The text executor then runs JSON
		// validation against the body; because OutputFormat="json" is
		// set on the stage, "not json" trips validateJSON and the
		// executor returns "stage output is not valid JSON: ...".
		// That error has no transient markers, so retry must NOT fire.
		return "not json", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "non_transient",
		Stages: []Stage{
			{
				ID: "a", Capability: "reasoning", Prompt: "X",
				Output: "a.json", OutputFormat: "json",
				Retry: fastRetry(5),
			},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected JSON-validation error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (non-transient must not retry)", got)
	}
}

// TestExecutor_ForeachPerItemRetry verifies that the retry policy applies
// per-item inside a foreach stage: 3 items with item 0 erroring transiently
// once and items 1 and 2 succeeding on first try should produce 4 total
// inference calls (3 items + 1 retry on item 0) and three written files.
func TestExecutor_ForeachPerItemRetry(t *testing.T) {
	// Per-item attempt counters; the producer stage is keyed separately.
	var producerCalls int32
	itemCalls := make(map[string]*int32)
	for _, k := range []string{"a", "b", "c"} {
		var c int32
		itemCalls[k] = &c
	}
	var mu sync.Mutex
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if prompt == "ITEMS" {
			atomic.AddInt32(&producerCalls, 1)
			return `["a","b","c"]`, nil
		}
		if strings.HasPrefix(prompt, "DO:") {
			tok := strings.TrimPrefix(prompt, "DO:")
			mu.Lock()
			c := itemCalls[tok]
			mu.Unlock()
			n := atomic.AddInt32(c, 1)
			// Fail item "a" on its first attempt; succeed on retry.
			// Items "b" and "c" succeed first try.
			if tok == "a" && n == 1 {
				return "", fmt.Errorf("chat completion 503 Service Unavailable")
			}
			return strings.ToUpper(tok), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "foreach_retry",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "DO:{{.x}}",
				Output:  "out/{{.x}}.txt",
				Retry:   fastRetry(3),
			},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	exec.MaxForeachConcurrency = 3
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("expected success after per-item retry, got: %v", err)
	}
	if got := atomic.LoadInt32(&producerCalls); got != 1 {
		t.Errorf("producer attempts = %d, want 1", got)
	}
	// Item "a" should have run twice; "b" and "c" once each.
	wantItem := map[string]int32{"a": 2, "b": 1, "c": 1}
	for k, want := range wantItem {
		got := atomic.LoadInt32(itemCalls[k])
		if got != want {
			t.Errorf("item %q attempts = %d, want %d", k, got, want)
		}
	}
	// All three files written with uppercase contents.
	for _, tc := range []struct{ k, want string }{{"a", "A"}, {"b", "B"}, {"c", "C"}} {
		body, err := os.ReadFile(filepath.Join(runDir, "out", tc.k+".txt"))
		if err != nil {
			t.Fatalf("read item %s: %v", tc.k, err)
		}
		if string(body) != tc.want {
			t.Errorf("out/%s.txt = %q, want %q", tc.k, body, tc.want)
		}
	}
}

// TestExecutor_ExponentialBackoffTiming verifies that the wait between
// successive attempts grows by Multiplier and caps at MaxBackoff. The
// stub records the wall-clock between attempts; we assert the second
// gap is ~Multiplier * first, capped at MaxBackoff. Wide tolerances
// keep CI green even on busy hosts.
func TestExecutor_ExponentialBackoffTiming(t *testing.T) {
	// Backoffs chosen so the geometric progression is visible:
	//   attempt 1 -> attempt 2: ~10ms
	//   attempt 2 -> attempt 3: ~20ms (10 * 2.0)
	//   attempt 3 -> attempt 4: capped at 25ms (40 would exceed cap)
	policy := &RetryPolicy{
		MaxAttempts:    4,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     25 * time.Millisecond,
		Multiplier:     2.0,
		RetryOn:        []string{"transient"},
	}
	var (
		mu        sync.Mutex
		callTimes []time.Time
	)
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		mu.Lock()
		callTimes = append(callTimes, time.Now())
		n := len(callTimes)
		mu.Unlock()
		if n < 4 {
			return "", fmt.Errorf("chat completion 503 Service Unavailable")
		}
		return "ok", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "backoff_timing",
		Stages: []Stage{
			{ID: "a", Capability: "reasoning", Prompt: "X", Output: "a.txt", Retry: policy},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("expected success on attempt 4, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(callTimes) != 4 {
		t.Fatalf("recorded %d call times, want 4", len(callTimes))
	}
	gap := func(i int) time.Duration { return callTimes[i].Sub(callTimes[i-1]) }
	g1, g2, g3 := gap(1), gap(2), gap(3)
	t.Logf("gaps: %s, %s, %s", g1, g2, g3)
	// First gap: ~10ms (the initial backoff). Allow some slack for
	// goroutine scheduling.
	if g1 < 8*time.Millisecond {
		t.Errorf("gap1 = %s, expected >= 8ms (initial backoff 10ms)", g1)
	}
	if g1 > 50*time.Millisecond {
		t.Errorf("gap1 = %s, expected <= 50ms (initial backoff 10ms, CI headroom)", g1)
	}
	// Second gap: ~20ms (10 * 2.0). Must be strictly larger than the
	// first gap minus a small jitter window — that's what proves
	// "exponential", not just "constant".
	if g2 < g1 {
		t.Errorf("gap2 (%s) should be >= gap1 (%s); backoff is not growing", g2, g1)
	}
	if g2 > 60*time.Millisecond {
		t.Errorf("gap2 = %s, expected <= 60ms (target ~20ms + CI headroom)", g2)
	}
	// Third gap: capped at MaxBackoff = 25ms. Without the cap it would
	// be ~40ms; assert it landed at or below the cap (with slack).
	if g3 > 50*time.Millisecond {
		t.Errorf("gap3 = %s, expected <= 50ms (cap is 25ms, would be 40ms uncapped)", g3)
	}
}

// TestExecutor_WritesPipelineJSON asserts that a successful run leaves a
// pipeline.json file in the run dir with one stage record per declared
// stage and the start/end timestamps populated.
func TestExecutor_WritesPipelineJSON(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "hi"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	runDir := t.TempDir()
	pipeline := &Pipeline{
		Name: "metadata-demo",
		Stages: []Stage{
			{ID: "first", Capability: "reasoning", Prompt: "hi", Output: "a.txt"},
			{ID: "second", Capability: "reasoning", Prompt: "hi", Inputs: []string{"first"}, Output: "b.txt"},
		},
	}
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       runDir,
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(runDir, "pipeline.json"))
	if err != nil {
		t.Fatalf("read pipeline.json: %v", err)
	}
	var rec RunRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal pipeline.json: %v", err)
	}
	if rec.Name != "metadata-demo" {
		t.Errorf("Name = %q, want metadata-demo", rec.Name)
	}
	if rec.Status != "ok" {
		t.Errorf("Status = %q, want ok", rec.Status)
	}
	if len(rec.Stages) != 2 {
		t.Fatalf("Stages = %v, want 2 entries", rec.Stages)
	}
}

// TestExecutor_PipelineJSONOnError asserts that a failed run still leaves
// a pipeline.json record behind.
func TestExecutor_PipelineJSONOnError(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kaboom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	runDir := t.TempDir()
	pipeline := &Pipeline{
		Name: "fail-demo",
		Stages: []Stage{
			{ID: "boom", Capability: "reasoning", Prompt: "hi", Output: "a.txt"},
		},
	}
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       runDir,
	}
	if err := exec.Run(context.Background()); err == nil {
		t.Fatal("Run: expected error from 500 response")
	}
	data, err := os.ReadFile(filepath.Join(runDir, "pipeline.json"))
	if err != nil {
		t.Fatalf("read pipeline.json: %v", err)
	}
	var rec RunRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal pipeline.json: %v", err)
	}
	if rec.Status != "error" {
		t.Errorf("Status = %q, want error", rec.Status)
	}
}

// TestExecutor_WritesPipelineTimingJSON verifies that a successful run leaves
// pipeline_timing.json next to pipeline.json and that the file contains the
// expected pipeline name + stage entry.
func TestExecutor_WritesPipelineTimingJSON(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "hello"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	runDir := t.TempDir()
	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline: &Pipeline{
			Name: "timing_smoke",
			Stages: []Stage{
				{ID: "only", Capability: "reasoning", Prompt: "hi", Output: "only.txt"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       runDir,
		Log:          &logBuf,
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "pipeline_timing.json"))
	if err != nil {
		t.Fatalf("read pipeline_timing.json: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rep["pipeline"] != "timing_smoke" {
		t.Errorf("pipeline = %v, want timing_smoke", rep["pipeline"])
	}
	stages, ok := rep["stages"].([]any)
	if !ok || len(stages) != 1 {
		t.Fatalf("stages = %v, want 1 entry", rep["stages"])
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "only") {
		t.Errorf("missing stage row in log:\n%s", logged)
	}
}

// TestExecutor_RunWhenFailureFiresOnError verifies that a stage marked
// run_when: failure runs only when an upstream input has errored, that
// the run still returns a non-nil error from the original failure, and
// that the {{ .failure_summary }} binding is populated for the failure
// stage's prompt.
func TestExecutor_RunWhenFailureFiresOnError(t *testing.T) {
	var notifyPrompt string
	var notifyCalled bool
	var notifyMu sync.Mutex
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "heavy":
			return "ok", nil
		case strings.HasPrefix(prompt, "BOOM"):
			return "", fmt.Errorf("stage 2 exploded")
		case strings.HasPrefix(prompt, "NOTIFY:"):
			notifyMu.Lock()
			notifyPrompt = prompt
			notifyCalled = true
			notifyMu.Unlock()
			return "sent", nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_failure",
		Stages: []Stage{
			{ID: "heavy", Capability: "reasoning", Prompt: "heavy", Output: "a.txt"},
			{ID: "boom", Capability: "reasoning", Prompt: "BOOM", Inputs: []string{"heavy"}, Output: "b.txt"},
			{ID: "notify", Capability: "reasoning",
				Prompt: "NOTIFY: status={{.pipeline_status}} summary={{.failure_summary}}",
				Inputs: []string{"boom"}, Output: "notify.txt", RunWhen: "failure"},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected pipeline error after stage 2 failure")
	}
	if !strings.Contains(err.Error(), "stage 2 exploded") {
		t.Errorf("aggregated error %v should contain stage 2's message", err)
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if !notifyCalled {
		t.Fatal("notify stage did not run after upstream failure")
	}
	if !strings.Contains(notifyPrompt, "status=error") {
		t.Errorf("notify prompt missing pipeline_status=error: %q", notifyPrompt)
	}
	if !strings.Contains(notifyPrompt, "summary=boom: ") {
		t.Errorf("notify prompt missing failure_summary referencing boom: %q", notifyPrompt)
	}
	if !strings.Contains(notifyPrompt, "stage 2 exploded") {
		t.Errorf("notify prompt missing original error text: %q", notifyPrompt)
	}
}

// TestExecutor_RunWhenFailureSkippedOnSuccess verifies that a stage
// marked run_when: failure is skipped when its inputs all succeeded.
// The pipeline returns nil and the failure stage's output file is never
// written.
func TestExecutor_RunWhenFailureSkippedOnSuccess(t *testing.T) {
	var notifyCalled atomic.Bool
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if strings.HasPrefix(prompt, "NOTIFY") {
			notifyCalled.Store(true)
			return "sent", nil
		}
		return "ok", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_failure_skip",
		Stages: []Stage{
			{ID: "heavy", Capability: "reasoning", Prompt: "heavy", Output: "a.txt"},
			{ID: "follow", Capability: "reasoning", Prompt: "follow", Inputs: []string{"heavy"}, Output: "b.txt"},
			{ID: "notify", Capability: "reasoning", Prompt: "NOTIFY", Inputs: []string{"follow"}, Output: "notify.txt", RunWhen: "failure"},
		},
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notifyCalled.Load() {
		t.Fatal("notify stage should not have run when upstream succeeded")
	}
	if _, err := os.Stat(filepath.Join(runDir, "notify.txt")); !os.IsNotExist(err) {
		t.Errorf("notify.txt should not exist (got err=%v)", err)
	}
}

// TestExecutor_RunWhenAlwaysFiresOnSuccess verifies that a stage marked
// run_when: always runs after a successful pipeline and that its
// pipeline_status template binding reports "ok".
func TestExecutor_RunWhenAlwaysFiresOnSuccess(t *testing.T) {
	var cleanupPrompt string
	var cleanupCalled bool
	var cleanupMu sync.Mutex
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if strings.HasPrefix(prompt, "CLEANUP:") {
			cleanupMu.Lock()
			cleanupPrompt = prompt
			cleanupCalled = true
			cleanupMu.Unlock()
			return "cleaned", nil
		}
		return "ok", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_always_success",
		Stages: []Stage{
			{ID: "work", Capability: "reasoning", Prompt: "work", Output: "a.txt"},
			{ID: "cleanup", Capability: "reasoning",
				Prompt: "CLEANUP: status={{.pipeline_status}} summary={{.failure_summary}}",
				Inputs: []string{"work"}, Output: "cleanup.txt", RunWhen: "always"},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	if !cleanupCalled {
		t.Fatal("cleanup stage with run_when=always did not run")
	}
	if !strings.Contains(cleanupPrompt, "status=ok") {
		t.Errorf("cleanup prompt missing pipeline_status=ok: %q", cleanupPrompt)
	}
	if !strings.Contains(cleanupPrompt, "summary=") || strings.Contains(cleanupPrompt, "summary=boom") {
		t.Errorf("cleanup prompt failure_summary should be empty on a successful run: %q", cleanupPrompt)
	}
}

// TestExecutor_RunWhenAlwaysFiresOnFailure verifies that a stage marked
// run_when: always runs after a failed pipeline, that the pipeline still
// returns the aggregated error, and that the cleanup stage observes the
// failure via {{ .pipeline_status }} / {{ .failure_summary }}.
func TestExecutor_RunWhenAlwaysFiresOnFailure(t *testing.T) {
	var cleanupPrompt string
	var cleanupCalled bool
	var cleanupMu sync.Mutex
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case strings.HasPrefix(prompt, "CLEANUP:"):
			cleanupMu.Lock()
			cleanupPrompt = prompt
			cleanupCalled = true
			cleanupMu.Unlock()
			return "cleaned", nil
		case prompt == "BOOM":
			return "", fmt.Errorf("kaboom")
		}
		return "ok", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_always_failure",
		Stages: []Stage{
			{ID: "work", Capability: "reasoning", Prompt: "BOOM", Output: "a.txt"},
			{ID: "cleanup", Capability: "reasoning",
				Prompt: "CLEANUP: status={{.pipeline_status}} summary={{.failure_summary}}",
				Inputs: []string{"work"}, Output: "cleanup.txt", RunWhen: "always"},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	var logBuf bytes.Buffer
	exec.Log = &logBuf
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected aggregated error from work stage")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("err = %v, want kaboom", err)
	}
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	if !cleanupCalled {
		t.Fatalf("cleanup stage with run_when=always did not run after failure\nlog:\n%s", logBuf.String())
	}
	if !strings.Contains(cleanupPrompt, "status=error") {
		t.Errorf("cleanup prompt missing pipeline_status=error: %q", cleanupPrompt)
	}
	if !strings.Contains(cleanupPrompt, "summary=work: ") {
		t.Errorf("cleanup prompt missing failure_summary referencing work: %q", cleanupPrompt)
	}
}

// TestRunWhen_TemplateTrue verifies that a stage whose run_when renders
// to "true" executes normally. The downstream stage references the
// upstream's output to confirm it actually ran (vs. being silently
// skipped by an over-eager template gate).
func TestRunWhen_TemplateTrue(t *testing.T) {
	var downstreamCalled atomic.Bool
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if strings.HasPrefix(prompt, "DOWN") {
			downstreamCalled.Store(true)
			return "down", nil
		}
		return "rainy", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_template_true",
		Stages: []Stage{
			{ID: "cover", Capability: "reasoning", Prompt: "cover", Output: "cover.txt"},
			{ID: "shoot", Capability: "reasoning", Prompt: "DOWN",
				Inputs:  []string{"cover"},
				Output:  "shoot.txt",
				RunWhen: `{{ contains .stages.cover.output "rainy" }}`,
			},
		},
	}
	// Validate explicitly so we exercise the LoadPipeline-time parse check
	// on the template form.
	if err := pipeline.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !downstreamCalled.Load() {
		t.Fatal("downstream stage with template run_when=true did not run")
	}
}

// TestRunWhen_TemplateFalse verifies that a stage whose run_when renders
// to "false" is skipped. Output file should never be written.
func TestRunWhen_TemplateFalse(t *testing.T) {
	var downstreamCalled atomic.Bool
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if strings.HasPrefix(prompt, "DOWN") {
			downstreamCalled.Store(true)
			return "down", nil
		}
		return "sunny", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_template_false",
		Stages: []Stage{
			{ID: "cover", Capability: "reasoning", Prompt: "cover", Output: "cover.txt"},
			{ID: "shoot", Capability: "reasoning", Prompt: "DOWN",
				Inputs:  []string{"cover"},
				Output:  "shoot.txt",
				RunWhen: `{{ contains .stages.cover.output "rainy" }}`,
			},
		},
	}
	if err := pipeline.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if downstreamCalled.Load() {
		t.Fatal("downstream stage with template run_when=false should not have run")
	}
	if _, err := os.Stat(filepath.Join(runDir, "shoot.txt")); !os.IsNotExist(err) {
		t.Errorf("shoot.txt should not exist (err=%v)", err)
	}
}

// TestRunWhen_TemplateBadShape verifies that a template whose rendered
// output is not in the accepted true/false set surfaces as a pipeline
// error naming the stage and the rendered value.
func TestRunWhen_TemplateBadShape(t *testing.T) {
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		return "ok", nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "runwhen_template_bad",
		Stages: []Stage{
			{ID: "cover", Capability: "reasoning", Prompt: "cover", Output: "cover.txt"},
			{ID: "shoot", Capability: "reasoning", Prompt: "down",
				Inputs:  []string{"cover"},
				Output:  "shoot.txt",
				RunWhen: `maybe`,
			},
		},
	}
	// "maybe" doesn't contain "{{" — it should be rejected at Validate
	// time as not-a-keyword-and-not-a-template. Make sure that error
	// fires; otherwise we'd never reach the rendered-value gate at all.
	if err := pipeline.Validate(); err == nil {
		t.Fatal("Validate: expected error for non-keyword, non-template run_when")
	} else if !strings.Contains(err.Error(), "run_when") {
		t.Errorf("Validate err = %v, want one mentioning run_when", err)
	}

	// Now exercise the runtime branch: a template that renders to an
	// unaccepted value. Pipeline.Validate accepts the parse; Run should
	// then error pointing at the stage and the rendered value.
	pipeline.Stages[1].RunWhen = `{{ printf "%s" "maybe" }}`
	if err := pipeline.Validate(); err != nil {
		t.Fatalf("Validate (template form): %v", err)
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("Run: expected pipeline error from bad-shape run_when template")
	}
	if !strings.Contains(err.Error(), "shoot") {
		t.Errorf("err %v should mention stage id 'shoot'", err)
	}
	if !strings.Contains(err.Error(), "maybe") {
		t.Errorf("err %v should mention rendered value 'maybe'", err)
	}
}

// startCall is one Start the executor made, and whether it asked for a
// strict pre-flight. The flag is half the contract: without it the
// daemon warns and proceeds on a short free read, so a walk that never
// sets it can only ever land on candidate 1.
type startCall struct {
	name   string
	strict bool
}

// vramFallbackControl is a ControlServiceHandler tuned for the candidate
// fallback tests. It reproduces the daemon's ACTUAL pre-flight verdicts
// rather than a message shaped like one:
//
//   - a name in shortOfFree is short of currently-free memory. That is a
//     refusal only when the caller asked for a strict pre-flight, and a
//     warning-and-proceed otherwise — daemon.go's `case res.Warn &&
//     req.Msg.StrictVram` and the `case res.Warn` below it.
//   - a name in exceedsCapacity is bigger than the machine. That refuses
//     every caller, strict or not.
//
// Both refusals carry the typed vibev1.StartRejection detail, because
// that is what vibeclient.IsVRAMRejection reads. The previous version of
// this stub returned a hand-written sentence containing "free VRAM" and
// a comment explaining that the classifier grepped for that substring;
// it kept passing for the four months in which no daemon anywhere
// emitted the string, which is the defect this file now guards.
//
// Records every Start call so the tests can verify both the order of the
// walk and what it asked for at each rung.
type vramFallbackControl struct {
	mu              sync.Mutex
	profile         string
	proxyURL        string
	shortOfFree     map[string]bool
	exceedsCapacity map[string]bool
	startedProfiles []string
	startCalls      []startCall
}

func (s *vramFallbackControl) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&vibev1.StatusResponse{Status: &vibev1.Status{
		Running: s.profile != "", Ready: s.profile != "", Profile: s.profile, ProxyAddr: s.proxyURL,
	}}), nil
}
func (s *vramFallbackControl) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	// Capability activation now names a backend; fall back to Profile for the
	// interactive path. The verdict maps are keyed on the candidate name.
	name := req.Msg.Profile
	if req.Msg.Backend != "" {
		name = req.Msg.Backend
	}
	strict := req.Msg.GetStrictVram()

	var detail *vibev1.StartRejection
	switch {
	case s.exceedsCapacity[name]:
		total := 16.0
		detail = &vibev1.StartRejection{
			Reason:       vibev1.StartRejection_REASON_VRAM_EXCEEDS_CAPACITY,
			Profile:      name,
			EstimatedGib: 24,
			FreeGib:      8,
			TotalGib:     &total,
		}
	case s.shortOfFree[name] && strict:
		detail = &vibev1.StartRejection{
			Reason:       vibev1.StartRejection_REASON_VRAM_INSUFFICIENT_FREE,
			Profile:      name,
			EstimatedGib: 24,
			FreeGib:      8,
		}
	}

	s.mu.Lock()
	s.startedProfiles = append(s.startedProfiles, name)
	s.startCalls = append(s.startCalls, startCall{name: name, strict: strict})
	if detail == nil {
		s.profile = name
	}
	s.mu.Unlock()

	if detail != nil {
		ce := connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("profile %q needs ~24.0 GiB, refused by the pre-flight", name))
		d, err := connect.NewErrorDetail(detail)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		ce.AddDetail(d)
		return nil, ce
	}
	r, _ := s.Status(context.TODO(), nil)
	return connect.NewResponse(&vibev1.StartResponse{Status: r.Msg.Status}), nil
}

// calls returns a copy of the recorded Start calls.
func (s *vramFallbackControl) calls() []startCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]startCall(nil), s.startCalls...)
}
func (s *vramFallbackControl) Stop(_ context.Context, _ *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	s.mu.Lock()
	s.profile = ""
	s.mu.Unlock()
	return connect.NewResponse(&vibev1.StopResponse{Status: &vibev1.Status{}}), nil
}
func (s *vramFallbackControl) ListProfiles(context.Context, *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	return connect.NewResponse(&vibev1.ListProfilesResponse{}), nil
}
func (s *vramFallbackControl) Shutdown(context.Context, *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	return connect.NewResponse(&vibev1.ShutdownResponse{}), nil
}
func (s *vramFallbackControl) Logs(context.Context, *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return connect.NewResponse(&vibev1.LogsResponse{}), nil
}
func (s *vramFallbackControl) CellDrain(context.Context, *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no cell verbs"))
}
func (s *vramFallbackControl) CellResume(context.Context, *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no cell verbs"))
}
func (s *vramFallbackControl) CellSuspend(context.Context, *connect.Request[vibev1.CellSuspendRequest]) (*connect.Response[vibev1.CellSuspendResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("no cell verbs"))
}
func (s *vramFallbackControl) Pull(_ context.Context, _ *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	return stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_DONE})
}

// TestExecutor_VRAMFallbackPicksNextCandidate runs the executor against a
// capability with two candidates [code, code_small]. Candidate 1 is short
// of currently-free memory — the "something else is resident right now"
// case the whole walk exists for. The executor must:
//  1. attempt candidate 1 asking for a STRICT pre-flight (without which
//     the daemon warns and proceeds, and the walk never moves),
//  2. see the typed rejection and fall back to candidate 2, asking for it
//     the ordinary way because there is nothing behind it,
//  3. write a "skipping <candidate>" line naming the typed reason, so the
//     operator can tell "busy right now" from "too big for this box"
//     after the fact.
//
// Step 1 is the end-to-end proof that candidate 2 is reachable at all.
// Between 4a4c5ea and this change it was not: insufficient free memory
// had stopped being an error, so candidate 1 always won or the pipeline
// died, and candidates 2..N of every capability were dead config.
func TestExecutor_VRAMFallbackPicksNextCandidate(t *testing.T) {
	stub := &vramFallbackControl{shortOfFree: map[string]bool{"code": true}}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{
		"reasoning": {Candidates: []string{"code", "code_small"}},
	}}
	pipeline := &Pipeline{
		Name: "vram-fallback",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "hi", Output: "plan.txt"},
		},
	}
	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both candidates should have been attempted, in order — and each with
	// the pre-flight strictness its position calls for. Candidate 1 is
	// asked strictly BECAUSE a smaller one is behind it; the last is not,
	// because refusing it would only turn a start that usually works into
	// a dead pipeline.
	calls := stub.calls()
	want := []startCall{{name: "code", strict: true}, {name: "code_small", strict: false}}
	if len(calls) != len(want) {
		t.Fatalf("Start calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("Start call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}

	// And the log must record the fallback in human-readable form so
	// after-the-fact debugging shows why we landed on candidate 2.
	logTxt := logBuf.String()
	if !strings.Contains(logTxt, `skipping "code"`) {
		t.Errorf("log missing skip notice for 'code': %s", logTxt)
	}
	// The typed reason, rendered from the enum — not a substring of the
	// daemon's sentence. "Insufficient free memory" and "exceeds this
	// machine's capacity" want different fixes from the operator.
	if !strings.Contains(logTxt, "vram: insufficient free memory") {
		t.Errorf("log missing the typed rejection reason: %s", logTxt)
	}
	if !strings.Contains(logTxt, `activating backend "code_small"`) {
		t.Errorf("log missing activation notice for 'code_small': %s", logTxt)
	}

	// End-of-run summary should name the capability + the profile that
	// actually answered, AND flag that 1st-choice 'code' was skipped.
	// Surfacing this at run end keeps the fallback visible after the
	// scrolling skip lines drift past in long runs.
	if !strings.Contains(logTxt, `capability "reasoning" resolved to backend "code_small"`) {
		t.Errorf("log missing resolution summary line: %s", logTxt)
	}
	if !strings.Contains(logTxt, `fallback`) || !strings.Contains(logTxt, `"code"`) {
		t.Errorf("log missing [fallback] marker naming the skipped 1st-choice: %s", logTxt)
	}
}

// TestExecutor_VRAMFallbackAllCandidatesFail confirms that when EVERY
// candidate VRAM-rejects, the executor surfaces the last error (so the
// operator sees a real "needs N GiB" message) rather than silently
// proceeding with no active profile.
//
// Both candidates exceed the machine's capacity here, deliberately: that
// is the verdict that refuses regardless of strictness, so it is the only
// way EVERY rung of the walk can fail — the last candidate is asked
// non-strictly and a merely-short-of-free reading would let it through.
func TestExecutor_VRAMFallbackAllCandidatesFail(t *testing.T) {
	stub := &vramFallbackControl{exceedsCapacity: map[string]bool{"code": true, "code_small": true}}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{
		"reasoning": {Candidates: []string{"code", "code_small"}},
	}}
	pipeline := &Pipeline{
		Name: "vram-all-fail",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "hi", Output: "plan.txt"},
		},
	}
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
	}
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error when all candidates VRAM-reject")
	}
	// The LAST candidate's refusal is what surfaces, and it must still be
	// classifiable after the executor's wrapping — a caller upstack that
	// re-inspects it has to reach the same verdict this walk did.
	if !strings.Contains(err.Error(), "code_small") {
		t.Errorf("err = %v, want the last candidate's refusal", err)
	}
	rej, ok := vibeclient.VRAMRejection(err)
	if !ok {
		t.Fatalf("err = %v, want a classifiable rejection to survive the executor's wrapping", err)
	}
	if rej.GetReason() != vibev1.StartRejection_REASON_VRAM_EXCEEDS_CAPACITY {
		t.Errorf("reason = %v, want REASON_VRAM_EXCEEDS_CAPACITY", rej.GetReason())
	}
}

// TestExecutor_SingleCandidateIsNotAskedForAStrictPreFlight: a capability
// with ONE candidate must be started the ordinary way, and a candidate
// that is merely short of free memory must therefore still run.
//
// This is the guard on 4a4c5ea's verdict from vamp's side. Strictness is
// only ever justified by having somewhere else to go; asking for it with
// no fallback would convert every tight-but-workable pipeline start into
// a hard failure, which is the false-negative behaviour 4a4c5ea removed
// on the evidence that the same profile on the same box read 15.2 and
// 23.5 GiB free minutes apart.
func TestExecutor_SingleCandidateIsNotAskedForAStrictPreFlight(t *testing.T) {
	stub := &vramFallbackControl{shortOfFree: map[string]bool{"code": true}}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{
		"reasoning": {Candidates: []string{"code"}},
	}}
	pipeline := &Pipeline{
		Name: "vram-single-candidate",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "hi", Output: "plan.txt"},
		},
	}
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: a lone candidate short of free memory must still start, got: %v", err)
	}
	calls := stub.calls()
	if len(calls) != 1 || calls[0] != (startCall{name: "code", strict: false}) {
		t.Errorf("Start calls = %+v, want exactly [{code false}]", calls)
	}
}

// TestExecutor_VRAMFallbackAbortsOnNonVRAMError asserts that a
// non-VRAM-rejection error from the daemon stops the candidate walk
// immediately. If we kept retrying past genuine failures we'd mask real
// problems (auth errors, profile-not-found, etc.) behind a fallback
// chain.
func TestExecutor_VRAMFallbackAbortsOnNonVRAMError(t *testing.T) {
	// custom handler that returns a non-VRAM FailedPrecondition for the
	// first candidate, so the executor must NOT fall back.
	stub := &vramFallbackControl{shortOfFree: map[string]bool{}}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(&abortControl{inner: stub})
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{
		"reasoning": {Candidates: []string{"code", "code_small"}},
	}}
	pipeline := &Pipeline{
		Name: "vram-abort",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "hi", Output: "plan.txt"},
		},
	}
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       t.TempDir(),
	}
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error on non-VRAM activation failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the upstream 'boom' message", err)
	}
}

// abortControl wraps a vramFallbackControl but always fails Start with a
// non-VRAM error. Used by TestExecutor_VRAMFallbackAbortsOnNonVRAMError.
type abortControl struct {
	inner *vramFallbackControl
}

func (a *abortControl) Status(ctx context.Context, req *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	return a.inner.Status(ctx, req)
}
func (a *abortControl) Start(_ context.Context, _ *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
}
func (a *abortControl) Stop(ctx context.Context, req *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	return a.inner.Stop(ctx, req)
}
func (a *abortControl) ListProfiles(ctx context.Context, req *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	return a.inner.ListProfiles(ctx, req)
}
func (a *abortControl) Shutdown(ctx context.Context, req *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	return a.inner.Shutdown(ctx, req)
}
func (a *abortControl) Logs(ctx context.Context, req *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return a.inner.Logs(ctx, req)
}
func (a *abortControl) Pull(ctx context.Context, req *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	return a.inner.Pull(ctx, req, stream)
}
func (a *abortControl) CellDrain(ctx context.Context, req *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	return a.inner.CellDrain(ctx, req)
}
func (a *abortControl) CellResume(ctx context.Context, req *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	return a.inner.CellResume(ctx, req)
}
func (a *abortControl) CellSuspend(ctx context.Context, req *connect.Request[vibev1.CellSuspendRequest]) (*connect.Response[vibev1.CellSuspendResponse], error) {
	return a.inner.CellSuspend(ctx, req)
}

// TestExecutor_CacheHit verifies that a second run with byte-identical inputs
// (same prompt, params, model) reads from the cache and never invokes the
// inference function. This is the core promise of the content-addressed
// cache; the inference call count is the cleanest observable for it.
func TestExecutor_CacheHit(t *testing.T) {
	calls := 0
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		calls++
		return "out:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "cache-hit",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "topic-X", Output: "plan.txt"},
		},
	}
	store, err := cacheNew(t)
	if err != nil {
		t.Fatal(err)
	}
	// Run 1: cache miss, executor runs.
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	exec.Cache = store
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("run 1: calls = %d, want 1", calls)
	}
	body, _ := os.ReadFile(filepath.Join(runDir, "plan.txt"))
	if string(body) != "out:topic-X" {
		t.Fatalf("run 1 output = %q", body)
	}
	// Run 2: same pipeline, same inputs — should be a cache hit so the
	// inference function MUST NOT be called again.
	exec2, runDir2 := stubExecutor(t, pipeline, caps, inf)
	exec2.Cache = store
	if err := exec2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("run 2: calls = %d, want 1 (cache should have served the hit)", calls)
	}
	body2, _ := os.ReadFile(filepath.Join(runDir2, "plan.txt"))
	if string(body2) != "out:topic-X" {
		t.Fatalf("run 2 output = %q", body2)
	}
}

// TestExecutor_CacheMissOnPromptChange verifies that changing the rendered
// prompt produces a fresh cache key (i.e. a miss) and the executor runs
// again. This is the prompt-engineering use case: tweak one stage's prompt,
// and that stage runs but the others don't.
func TestExecutor_CacheMissOnPromptChange(t *testing.T) {
	calls := 0
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		calls++
		return "out:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	store, err := cacheNew(t)
	if err != nil {
		t.Fatal(err)
	}
	exec1, _ := stubExecutor(t, &Pipeline{
		Name: "cache-prompt",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "prompt v1", Output: "plan.txt"},
		},
	}, caps, inf)
	exec1.Cache = store
	if err := exec1.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same pipeline name, same stage id, different prompt: must miss cache.
	exec2, _ := stubExecutor(t, &Pipeline{
		Name: "cache-prompt",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "prompt v2 (changed)", Output: "plan.txt"},
		},
	}, caps, inf)
	exec2.Cache = store
	if err := exec2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (prompt change must produce a miss)", calls)
	}
}

// TestExecutor_CacheDisabled verifies that a pipeline-level `cache: false`
// produces neither cache reads nor cache writes. The store remains empty
// after the run.
func TestExecutor_CacheDisabled(t *testing.T) {
	calls := 0
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		calls++
		return "out:" + prompt, nil
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	store, err := cacheNew(t)
	if err != nil {
		t.Fatal(err)
	}
	cacheOff := false
	pipeline := &Pipeline{
		Name:  "cache-off",
		Cache: &cacheOff,
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "x", Output: "plan.txt"},
		},
	}
	exec, _ := stubExecutor(t, pipeline, caps, inf)
	exec.Cache = store
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls run 1 = %d", calls)
	}
	// Run again: cache is off, so the executor runs again AND no entry was
	// written.
	exec2, _ := stubExecutor(t, pipeline, caps, inf)
	exec2.Cache = store
	if err := exec2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls run 2 = %d, want 2 (cache disabled must not serve hits)", calls)
	}
	if _, count, _ := store.Size(); count != 0 {
		t.Fatalf("cache entry count = %d, want 0 (no writes when disabled)", count)
	}
}

// cacheNew constructs a fresh cache store under a t.TempDir(). Centralised so
// the three cache tests share the same setup without leaking state across
// tests (each test gets its own temp dir).
func cacheNew(t *testing.T) (*vampcache.Store, error) {
	t.Helper()
	return vampcache.New(t.TempDir())
}

// TestExecutor_RunStageCleanup_RemovesMatchingFiles invokes runStageCleanup
// against a temp run dir containing a mix of files: ones that match the
// declared glob (should be removed) and ones that don't (should survive).
// Driving the helper directly rather than through a full executor run keeps
// the assertion narrow to "cleanup did exactly the disk work we asked for".
func TestExecutor_RunStageCleanup_RemovesMatchingFiles(t *testing.T) {
	runDir := t.TempDir()
	// Seed: two .wav files we expect to be cleaned, plus one .mp3 and one
	// file under a sibling dir that must survive.
	mustMkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(runDir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(p string) {
		if err := os.WriteFile(filepath.Join(runDir, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir("audio")
	mustMkdir("keep")
	mustWrite("audio/a.wav")
	mustWrite("audio/b.wav")
	mustWrite("audio/keep.mp3")
	mustWrite("keep/survivor.bin")
	mustWrite("podcast.mp3")
	exec := &Executor{RunDir: runDir}
	st := &Stage{ID: "cleanup", Cleanup: []string{"audio/*.wav"}}
	exec.runStageCleanup(st)
	if _, err := os.Stat(filepath.Join(runDir, "audio/a.wav")); !os.IsNotExist(err) {
		t.Errorf("audio/a.wav still present (err=%v); cleanup should have removed it", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "audio/b.wav")); !os.IsNotExist(err) {
		t.Errorf("audio/b.wav still present (err=%v); cleanup should have removed it", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "audio/keep.mp3")); err != nil {
		t.Errorf("audio/keep.mp3 missing (err=%v); cleanup pattern should not have matched it", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "keep/survivor.bin")); err != nil {
		t.Errorf("keep/survivor.bin missing (err=%v); cleanup pattern should not have matched it", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "podcast.mp3")); err != nil {
		t.Errorf("podcast.mp3 missing (err=%v); cleanup pattern should not have matched it", err)
	}
}

// TestExecutor_RunStageCleanup_NoOpForEmptyList verifies cleanup is a no-op
// when the stage has no patterns declared. The function is called from
// every stage's success path so this fast path must NOT touch disk or log.
func TestExecutor_RunStageCleanup_NoOpForEmptyList(t *testing.T) {
	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, "keepme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := &Executor{RunDir: runDir}
	exec.runStageCleanup(&Stage{ID: "x"})
	if _, err := os.Stat(filepath.Join(runDir, "keepme.txt")); err != nil {
		t.Errorf("keepme.txt missing after no-op cleanup (err=%v)", err)
	}
}

// TestExecutor_RunStageCleanup_MissingFilesAreFine confirms cleanup's
// best-effort contract: a pattern that doesn't match anything is not an
// error and doesn't blow up the stage. This is the cache-hit path's
// behaviour — the re-run might cleanup-hit on files an earlier run
// already removed.
func TestExecutor_RunStageCleanup_MissingFilesAreFine(t *testing.T) {
	runDir := t.TempDir()
	exec := &Executor{RunDir: runDir}
	st := &Stage{ID: "x", Cleanup: []string{"never_existed/*.tmp"}}
	exec.runStageCleanup(st) // must not panic, must not log an error
}

// TestExecutor_RunStageCleanup_SkipsUnsafePatterns is a defence-in-depth
// check: even if a Pipeline was constructed in-memory (bypassing the
// LoadPipeline validator) with an absolute or ../ pattern, runStageCleanup
// must refuse to act on it. Mirrors the validate-time rejection so the
// invariant holds at both load- and run-time.
func TestExecutor_RunStageCleanup_SkipsUnsafePatterns(t *testing.T) {
	runDir := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "do_not_delete.txt")
	if err := os.WriteFile(sentinel, []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := &Executor{RunDir: runDir}
	st := &Stage{ID: "x", Cleanup: []string{outside + "/*.txt", "../*"}}
	exec.runStageCleanup(st)
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel %q was removed (err=%v); runStageCleanup must refuse absolute/parent-traversal patterns", sentinel, err)
	}
}

// TestExecutor_CleanupRunsOnSuccessfulStage runs a real one-stage pipeline
// against a stub chat-completion server, then asserts the file declared in
// cleanup: was scrubbed from disk after the stage completed. This covers
// the wire-up: executeStage's success exit calls runStageCleanup AFTER the
// stage's own output has been written.
func TestExecutor_CleanupRunsOnSuccessfulStage(t *testing.T) {
	stub := &stubControl{}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	runDir := t.TempDir()
	// Seed an intermediate file the cleanup pattern is expected to match.
	scratch := filepath.Join(runDir, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	doomed := filepath.Join(scratch, "doomed.tmp")
	if err := os.WriteFile(doomed, []byte("trash"), 0o644); err != nil {
		t.Fatal(err)
	}

	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := &Pipeline{
		Name: "cleanup_e2e",
		Stages: []Stage{{
			ID:         "a",
			Capability: "reasoning",
			Prompt:     "hi",
			Output:     "a.txt",
			Cleanup:    []string{"scratch/*.tmp"},
		}},
	}
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       runDir,
	}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "a.txt")); err != nil {
		t.Errorf("stage output missing (err=%v) — cleanup must run AFTER output write", err)
	}
	if _, err := os.Stat(doomed); !os.IsNotExist(err) {
		t.Errorf("doomed file still present (err=%v); cleanup did not run on success", err)
	}
}

// TestExecutor_ForeachConcurrency_CappedByProfileParallel verifies the
// profile-parallel cap is applied to text foreach stages. A synthetic
// profile with parallel=1 backs a 10-item foreach stage; the per-item
// concurrency the runner actually achieves must be 1 even though
// MaxForeachConcurrency is left at the default (4) and 10 items are
// available.
//
// The observation is done at the runner layer (inside the InferenceFunc):
// each item increments a live counter on entry, sleeps a few milliseconds
// (long enough for sibling goroutines to race in if the cap leaked), and
// tracks the max-witnessed counter via atomic.Max. That max is the
// concurrency the runner actually achieved — exactly the value we want
// bounded by profile.parallel.
func TestExecutor_ForeachConcurrency_CappedByProfileParallel(t *testing.T) {
	const itemLatency = 30 * time.Millisecond
	var inflight, maxInflight int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["a","b","c","d","e","f","g","h","i","j"]`, nil
		case strings.HasPrefix(prompt, "ITEM:"):
			n := atomic.AddInt32(&inflight, 1)
			// Lock-free monotonic-max via CAS loop so the witness is
			// thread-safe without an extra mutex.
			for {
				prev := atomic.LoadInt32(&maxInflight)
				if n <= prev || atomic.CompareAndSwapInt32(&maxInflight, prev, n) {
					break
				}
			}
			select {
			case <-time.After(itemLatency):
			case <-ctx.Done():
				atomic.AddInt32(&inflight, -1)
				return "", ctx.Err()
			}
			atomic.AddInt32(&inflight, -1)
			return strings.TrimPrefix(prompt, "ITEM:"), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "serial"}}}
	pipeline := &Pipeline{
		Name: "cap-by-profile",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "ITEM:{{.x}}",
				Output:  "out/{{.x}}.txt",
			},
		},
	}
	exec, _, stub := stubExecutorWithControl(t, pipeline, caps, inf)
	// Program the synthetic profile as parallel=1: a single inference slot
	// on the backend. The executor must cap the foreach concurrency at
	// this value even though MaxForeachConcurrency would otherwise allow
	// 4 (the default).
	stub.parallel = 1
	// Leave MaxForeachConcurrency at zero (default 4) so the cap-from-profile
	// path is exercised independently of any per-Executor override.
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&maxInflight); got != 1 {
		t.Errorf("max observed concurrency = %d, want 1 (cap = profile.parallel)", got)
	}
	t.Logf("max-witnessed concurrency: %d", atomic.LoadInt32(&maxInflight))
}

// TestExecutor_ForeachConcurrency_RespectsExplicitOverrideUnderCap verifies
// the cap is a MINIMUM (lower of (stage concurrency, profile parallel)).
// When MaxForeachConcurrency is set to a value smaller than profile.parallel
// the executor must still honor the smaller value — the cap doesn't lift
// concurrency, it only lowers it.
func TestExecutor_ForeachConcurrency_RespectsExplicitOverrideUnderCap(t *testing.T) {
	const itemLatency = 30 * time.Millisecond
	var inflight, maxInflight int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["a","b","c","d","e","f","g","h"]`, nil
		case strings.HasPrefix(prompt, "ITEM:"):
			n := atomic.AddInt32(&inflight, 1)
			for {
				prev := atomic.LoadInt32(&maxInflight)
				if n <= prev || atomic.CompareAndSwapInt32(&maxInflight, prev, n) {
					break
				}
			}
			select {
			case <-time.After(itemLatency):
			case <-ctx.Done():
				atomic.AddInt32(&inflight, -1)
				return "", ctx.Err()
			}
			atomic.AddInt32(&inflight, -1)
			return strings.TrimPrefix(prompt, "ITEM:"), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "wide"}}}
	pipeline := &Pipeline{
		Name: "explicit-under-cap",
		Stages: []Stage{
			{ID: "items", Capability: "reasoning", Prompt: "ITEMS", Output: "items.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"items"},
				Foreach: &ForeachSpec{From: "items", Var: "x"},
				Prompt:  "ITEM:{{.x}}",
				Output:  "out/{{.x}}.txt",
			},
		},
	}
	exec, _, stub := stubExecutorWithControl(t, pipeline, caps, inf)
	// Profile allows 8 parallel slots; user explicitly capped at 2.
	stub.parallel = 8
	exec.MaxForeachConcurrency = 2
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&maxInflight); got != 2 {
		t.Errorf("max observed concurrency = %d, want 2 (explicit override wins over higher profile.parallel)", got)
	}
}

// TestStageCacheable_WebhookOptIn verifies the opt-in caching contract
// for webhook stages: cacheable only when the stage explicitly sets
// `cache: true`. Other non-cacheable types (youtube/confirm) remain
// uncached even if the field is set.
func TestStageCacheable_WebhookOptIn(t *testing.T) {
	trueVal, falseVal := true, false
	cases := []struct {
		name string
		st   *Stage
		want bool
	}{
		{
			name: "webhook_default_off",
			st:   &Stage{Type: StageTypeWebhook},
			want: false,
		},
		{
			name: "webhook_cache_false",
			st:   &Stage{Type: StageTypeWebhook, Cache: &falseVal},
			want: false,
		},
		{
			name: "webhook_cache_true",
			st:   &Stage{Type: StageTypeWebhook, Cache: &trueVal},
			want: true,
		},
		{
			name: "youtube_cache_true_still_uncached",
			st:   &Stage{Type: StageTypeYouTube, Cache: &trueVal},
			want: false,
		},
		{
			name: "confirm_cache_true_still_uncached",
			st:   &Stage{Type: StageTypeConfirm, Cache: &trueVal},
			want: false,
		},
		{
			name: "text_default_on",
			st:   &Stage{Type: StageTypeText},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stageCacheable(c.st)
			if got != c.want {
				t.Errorf("stageCacheable(%+v) = %v, want %v", c.st, got, c.want)
			}
		})
	}
}
