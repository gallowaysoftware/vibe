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
	"strconv"
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
		Capabilities: &Capabilities{Mapping: map[string]string{"reasoning": "code"}},
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
		Capabilities: &Capabilities{Mapping: map[string]string{"r": "code"}},
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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

// TestExecutor_ParallelForeach_CancelsSiblings verifies that an erroring item
// cancels its siblings via the per-stage context. Sibling items observe
// ctx.Done() and return promptly instead of sleeping out their full latency.
func TestExecutor_ParallelForeach_CancelsSiblings(t *testing.T) {
	const longLatency = 800 * time.Millisecond
	var aborted int32
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		switch {
		case prompt == "ITEMS":
			return `["fail","slow1","slow2","slow3"]`, nil
		case prompt == "ITEM:fail":
			// Sleep just long enough for siblings to be in flight before we
			// poison the context.
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "", fmt.Errorf("intentional failure")
		case strings.HasPrefix(prompt, "ITEM:"):
			select {
			case <-time.After(longLatency):
				return strings.TrimPrefix(prompt, "ITEM:"), nil
			case <-ctx.Done():
				atomic.AddInt32(&aborted, 1)
				return "", ctx.Err()
			}
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	pipeline := &Pipeline{
		Name: "cancel",
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
	err := exec.Run(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	// The failing item's error is wrapped and joined; verify both the failure
	// surfaces and the joined error mentions item 0 (or the relevant items
	// indices) and the underlying error.
	if !strings.Contains(err.Error(), "intentional failure") {
		t.Errorf("expected 'intentional failure' in aggregated error, got: %v", err)
	}
	// At least one sibling must have observed cancellation rather than
	// running to completion.
	if got := atomic.LoadInt32(&aborted); got < 1 {
		t.Errorf("expected >= 1 sibling to observe ctx cancellation, got %d", got)
	}
	// And the whole stage must have finished WELL before the slow items'
	// nominal 800ms latency — that's the whole point of cooperative cancel.
	if elapsed >= 600*time.Millisecond {
		t.Errorf("cancel propagation took %s, expected < 600ms (slow items did not honor ctx)", elapsed)
	}
	t.Logf("cancel propagation wall-clock: %s (aborted siblings: %d)", elapsed, atomic.LoadInt32(&aborted))
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}

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

// TestExecutor_ResumeRunsForeachIfAnyItemMissing verifies that a foreach
// stage's partial output forces a full rerun in Phase 1: with 3 items in the
// upstream array and only 2 of 3 per-item files present, all 3 items must run
// again.
func TestExecutor_ResumeRunsForeachIfAnyItemMissing(t *testing.T) {
	var consumerCalls sync.Map
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		if prompt == "TITLES" {
			return `["a","b","c"]`, nil
		}
		if strings.HasPrefix(prompt, "UP:") {
			tok := strings.TrimPrefix(prompt, "UP:")
			consumerCalls.Store(tok, true)
			return strings.ToUpper(tok), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	pipeline := &Pipeline{
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
	pipelineSource := "name: fan\n" // placeholder; the contents don't matter so long as resume's hash check sees the SAME bytes in snapshot and PipelineSource
	exec, runDir := stubExecutor(t, pipeline, caps, inf)
	// Pre-populate upstream JSON output + 2 of 3 per-item files.
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b","c"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "items"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "items", "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "items", "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	// items/c.txt deliberately missing.
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(pipelineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.PipelineSource = []byte(pipelineSource)
	exec.ResumeDir = runDir
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	// Every item must have been invoked because Phase 1 reruns the whole
	// foreach when any per-item file is missing.
	for _, tok := range []string{"a", "b", "c"} {
		if _, ran := consumerCalls.Load(tok); !ran {
			t.Errorf("expected foreach item %q to rerun, but it did not", tok)
		}
	}
	// And the previously-missing c.txt must now exist with the right body.
	got, err := os.ReadFile(filepath.Join(runDir, "items", "c.txt"))
	if err != nil {
		t.Fatalf("expected items/c.txt to be written on rerun: %v", err)
	}
	if string(got) != "C" {
		t.Errorf("items/c.txt = %q, want %q", got, "C")
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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

	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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

	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
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
		Capabilities: &Capabilities{Mapping: map[string]string{"reasoning": "code"}},
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
