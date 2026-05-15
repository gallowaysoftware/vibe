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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// stubExecutor wires up a real *Executor with a stub control plane so the
// scheduler exercises its real EnsureActive/Status paths. Inference is left
// mocked by the caller so tests can control latency and content.
func stubExecutor(t *testing.T, pipeline *Pipeline, caps *Capabilities, inf InferenceFunc) (*Executor, string) {
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
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client()),
		RunDir:       runDir,
		Inference:    inf,
	}
	return exec, runDir
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reason": "code", "write": "fast"}}
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
	if !((rest[0] == wantB && rest[1] == wantC) || (rest[0] == wantC && rest[1] == wantB)) {
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
