package vamp

import (
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
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/comfyui"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// fakeComfyServer is a minimal in-process double of ComfyUI's REST API used
// for executor tests. It records the submitted workflow, completes the
// returned prompt_id with the configured outputs, and serves view/<filename>
// requests with the configured payload.
//
// outputs is the default "images" bucket. videos / gifs let a test exercise
// the non-image code paths without altering the default behaviour for the
// existing image-only tests.
type fakeComfyServer struct {
	t           *testing.T
	mu          sync.Mutex
	submitted   []map[string]any // every workflow body posted to /prompt
	outputs     []comfyui.OutputFile
	videos      []comfyui.OutputFile
	gifs        []comfyui.OutputFile
	viewPayload []byte
	// viewPayloads maps a fetched filename to a per-file payload, used when a
	// test needs to verify that two different output files produce the right
	// bytes at their destinations. When nil or missing, viewPayload is served.
	viewPayloads map[string][]byte
	promptID     string
}

func newFakeComfyServer(t *testing.T, promptID string, outputs []comfyui.OutputFile, viewPayload []byte) (*fakeComfyServer, *httptest.Server) {
	t.Helper()
	fcs := &fakeComfyServer{
		t:           t,
		outputs:     outputs,
		viewPayload: viewPayload,
		promptID:    promptID,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt map[string]any `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode /prompt: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fcs.mu.Lock()
		fcs.submitted = append(fcs.submitted, body.Prompt)
		fcs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"prompt_id":   fcs.promptID,
			"number":      1,
			"node_errors": map[string]any{},
		})
	})
	mux.HandleFunc("GET /history/", func(w http.ResponseWriter, r *http.Request) {
		// /history/<id>. Return a completed result with the configured outputs
		// keyed on a single node id "9" so the executor can find them.
		id := strings.TrimPrefix(r.URL.Path, "/history/")
		if id != fcs.promptID {
			_, _ = w.Write([]byte("{}"))
			return
		}
		fcs.mu.Lock()
		imgs := fcs.outputs
		vids := fcs.videos
		gifs := fcs.gifs
		fcs.mu.Unlock()
		node9 := map[string]any{}
		// Only attach buckets that actually have entries — ComfyUI omits empty
		// buckets in real responses, and emitting empty arrays would defeat the
		// `omitempty` round-trip the decoder relies on.
		if len(imgs) > 0 {
			node9["images"] = imgs
		}
		if len(vids) > 0 {
			node9["videos"] = vids
		}
		if len(gifs) > 0 {
			node9["gifs"] = gifs
		}
		resp := map[string]any{
			id: map[string]any{
				"status":  map[string]any{"completed": true},
				"outputs": map[string]any{"9": node9},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		fcs.mu.Lock()
		payloads := fcs.viewPayloads
		def := fcs.viewPayload
		fcs.mu.Unlock()
		if payloads != nil {
			if p, ok := payloads[r.URL.Query().Get("filename")]; ok {
				_, _ = w.Write(p)
				return
			}
		}
		_, _ = w.Write(def)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fcs, srv
}

// submittedAt returns the workflow body recorded for the call at index idx.
func (f *fakeComfyServer) submittedAt(idx int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx < 0 || idx >= len(f.submitted) {
		f.t.Fatalf("submittedAt(%d): only %d submissions recorded", idx, len(f.submitted))
	}
	return f.submitted[idx]
}

// submitCount returns how many /prompt calls were made.
func (f *fakeComfyServer) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submitted)
}

// stubComfyRuntime wires a stub vibe control plane + fake ComfyUI server into
// an *Executor. Returns the executor, the run dir, the pipeline dir
// (where workflow JSON files are written), and the fakeComfyServer for
// post-run assertions.
func stubComfyRuntime(t *testing.T, pipeline *Pipeline, workflowJSON string) (*Executor, string, string, *fakeComfyServer) {
	t.Helper()

	pipelineDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pipelineDir, "workflow.json"), []byte(workflowJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Set every stage's workflow path to the file we just wrote.
	for i := range pipeline.Stages {
		if pipeline.Stages[i].Type == StageTypeComfyUI && pipeline.Stages[i].Workflow == "" {
			pipeline.Stages[i].Workflow = "workflow.json"
		}
	}

	// Fake ComfyUI server with one output by default.
	fcs, comfySrv := newFakeComfyServer(t, "p1",
		[]comfyui.OutputFile{{Filename: "out.png", Subfolder: "", Type: "output"}},
		[]byte("PNGDATA12"))

	// Vibe control plane stub: Status reports BackendAddr pointing at the
	// fake ComfyUI server.
	stub := &stubControlWithBackend{proxyURL: "", backendURL: comfySrv.URL}
	mux := http.NewServeMux()
	path, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(path, handler)
	vibeSrv := httptest.NewServer(mux)
	t.Cleanup(vibeSrv.Close)
	// proxyURL has to be set to something that responds to /v1/models even
	// for pure-comfyui pipelines — we never resolve a model id in that case,
	// but the field is still copied into BaseURL. Use the same server.
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub"}}})
	})
	stub.proxyURL = vibeSrv.URL

	runDir := t.TempDir()
	exec := &Executor{
		Pipeline:     pipeline,
		PipelineDir:  pipelineDir,
		Capabilities: &Capabilities{Mapping: map[string]string{"image": "comfyui-flux", "reasoning": "code"}},
		Vibe:         vibeclient.NewWithHTTPClient(vibeSrv.URL, vibeSrv.Client(), ""),
		RunDir:       runDir,
	}
	return exec, runDir, pipelineDir, fcs
}

// stubControlWithBackend extends the test control plane to expose a configurable
// BackendAddr separate from ProxyAddr.
type stubControlWithBackend struct {
	mu         sync.Mutex
	profile    string
	proxyURL   string
	backendURL string
}

func (s *stubControlWithBackend) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&vibev1.StatusResponse{Status: &vibev1.Status{
		Running:     s.profile != "",
		Ready:       s.profile != "",
		Profile:     s.profile,
		ProxyAddr:   s.proxyURL,
		BackendAddr: s.backendURL,
	}}), nil
}
func (s *stubControlWithBackend) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	s.mu.Lock()
	s.profile = req.Msg.Profile
	s.mu.Unlock()
	r, _ := s.Status(nil, nil)
	return connect.NewResponse(&vibev1.StartResponse{Status: r.Msg.Status}), nil
}
func (s *stubControlWithBackend) Stop(_ context.Context, _ *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	s.mu.Lock()
	s.profile = ""
	s.mu.Unlock()
	return connect.NewResponse(&vibev1.StopResponse{Status: &vibev1.Status{}}), nil
}
func (s *stubControlWithBackend) ListProfiles(context.Context, *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	return connect.NewResponse(&vibev1.ListProfilesResponse{}), nil
}
func (s *stubControlWithBackend) Shutdown(context.Context, *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	return connect.NewResponse(&vibev1.ShutdownResponse{}), nil
}
func (s *stubControlWithBackend) Logs(context.Context, *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return connect.NewResponse(&vibev1.LogsResponse{}), nil
}
func (s *stubControlWithBackend) Pull(_ context.Context, _ *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	return stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_DONE})
}

// TestComfyUIExecutor_RendersAndSubmits exercises the happy path: a single
// comfyui stage renders the parameter template against pipeline inputs and
// submits the mutated workflow.
func TestComfyUIExecutor_RendersAndSubmits(t *testing.T) {
	workflow := `{"6":{"class_type":"CLIPTextEncode","inputs":{"text":"original"}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID:         "render",
			Type:       StageTypeComfyUI,
			Capability: "image",
			Output:     "img.png",
			Parameters: map[string]string{"6.text": "rendered_{{.inputs.topic}}"},
		}},
	}
	exec, _, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	exec.Inputs = map[string]string{"topic": "robots"}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fcs.submitCount() != 1 {
		t.Fatalf("expected 1 submission, got %d", fcs.submitCount())
	}
	body := fcs.submittedAt(0)
	node6, ok := body["6"].(map[string]any)
	if !ok {
		t.Fatalf("submitted body missing node 6: %+v", body)
	}
	inputs, ok := node6["inputs"].(map[string]any)
	if !ok {
		t.Fatalf("node 6 missing inputs: %+v", node6)
	}
	if got := inputs["text"]; got != "rendered_robots" {
		t.Errorf("node 6 text = %v, want %q", got, "rendered_robots")
	}
}

// TestComfyUIExecutor_TypeCoercion verifies every supported coercion lands
// in the submitted workflow with the right JSON type.
func TestComfyUIExecutor_TypeCoercion(t *testing.T) {
	// Workflow has one node "1" with four typed inputs, all initially strings.
	workflow := `{"1":{"class_type":"X","inputs":{"s":"","i":"","f":"","b":""}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output: "img.png",
			Parameters: map[string]string{
				"1.s": "hello",
				"1.i": "42",
				"1.f": "3.14",
				"1.b": "true",
			},
		}},
	}
	exec, _, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := fcs.submittedAt(0)
	inputs := body["1"].(map[string]any)["inputs"].(map[string]any)

	// JSON traversal through encoding/json yields float64 for numbers.
	// fakeComfyServer decodes via json.NewDecoder without UseNumber, so
	// the int 42 lands as float64(42) on the test side — but we want to
	// prove the wire encoding was a JSON int, not a string. Re-encode the
	// node and inspect the raw JSON to assert the right token shapes.
	encoded, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(encoded)
	// String: quoted.
	if !strings.Contains(raw, `"s":"hello"`) {
		t.Errorf("string coercion missing: %s", raw)
	}
	// Int: unquoted, no decimal point.
	if !strings.Contains(raw, `"i":42`) {
		t.Errorf("int coercion missing: %s", raw)
	}
	if strings.Contains(raw, `"i":42.0`) || strings.Contains(raw, `"i":"42"`) {
		t.Errorf("int should not be float or string: %s", raw)
	}
	// Float: contains a decimal point.
	if !strings.Contains(raw, `"f":3.14`) {
		t.Errorf("float coercion missing: %s", raw)
	}
	// Bool: unquoted true/false.
	if !strings.Contains(raw, `"b":true`) {
		t.Errorf("bool coercion missing: %s", raw)
	}
}

// TestComfyUIExecutor_CopiesOutputFile verifies the rendered output file
// lands at the rendered `output:` path under RunDir with the served bytes.
func TestComfyUIExecutor_CopiesOutputFile(t *testing.T) {
	workflow := `{"6":{"class_type":"CLIPTextEncode","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "assets/{{.inputs.slug}}.png",
			Parameters: map[string]string{"6.text": "hello"},
		}},
	}
	exec, runDir, _, _ := stubComfyRuntime(t, pipeline, workflow)
	exec.Inputs = map[string]string{"slug": "robot"}
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(runDir, "assets", "robot.png"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "PNGDATA12" {
		t.Errorf("output bytes = %q, want %q", got, "PNGDATA12")
	}
}

// TestComfyUIExecutor_RejectsUnknownNode verifies a parameter key pointing
// at a missing node id yields an error mentioning that id.
func TestComfyUIExecutor_RejectsUnknownNode(t *testing.T) {
	workflow := `{"6":{"class_type":"X","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output: "out.png",
			// 99 doesn't exist in the workflow.
			Parameters: map[string]string{"99.text": "hi"},
		}},
	}
	exec, _, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown node id")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("expected error to mention node id 99, got: %v", err)
	}
	if fcs.submitCount() != 0 {
		t.Errorf("expected no submissions on parameter error, got %d", fcs.submitCount())
	}
}

// TestComfyUIExecutor_RejectsMultipleOutputs verifies that a workflow whose
// history contains more than one OutputFile fails with a clear, actionable
// message pointing at batch_size=1.
func TestComfyUIExecutor_RejectsMultipleOutputs(t *testing.T) {
	workflow := `{"6":{"class_type":"X","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "out.png",
			Parameters: map[string]string{"6.text": "x"},
		}},
	}
	exec, _, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	// Reconfigure the server to return two output files.
	fcs.mu.Lock()
	fcs.outputs = []comfyui.OutputFile{
		{Filename: "a.png", Type: "output"},
		{Filename: "b.png", Type: "output"},
	}
	fcs.mu.Unlock()
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for multi-output workflow")
	}
	if !strings.Contains(err.Error(), "batch_size") {
		t.Errorf("expected batch_size hint in error, got: %v", err)
	}
}

// TestComfyUIExecutor_CopiesVideoOutput verifies a workflow whose history
// surfaces an output under the `videos` bucket (modern SaveVideo /
// VHS_VideoCombine) is copied to the rendered output path with the served
// bytes preserved. The image bucket stays empty.
func TestComfyUIExecutor_CopiesVideoOutput(t *testing.T) {
	workflow := `{"6":{"class_type":"CLIPTextEncode","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "vid",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "assets/{{.inputs.slug}}.mp4",
			Parameters: map[string]string{"6.text": "hello"},
		}},
	}
	exec, runDir, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	exec.Inputs = map[string]string{"slug": "clip"}
	// Reconfigure: drop the default image, surface a video instead.
	fcs.mu.Lock()
	fcs.outputs = nil
	fcs.videos = []comfyui.OutputFile{{Filename: "out.mp4", Type: "output"}}
	fcs.viewPayload = []byte("MP4DATA42")
	fcs.mu.Unlock()
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(runDir, "assets", "clip.mp4"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "MP4DATA42" {
		t.Errorf("output bytes = %q, want %q", got, "MP4DATA42")
	}
}

// TestComfyUIExecutor_CopiesGifOutput verifies the same plumbing for the
// legacy `gifs` bucket (SaveAnimatedWEBP / older VHS variants).
func TestComfyUIExecutor_CopiesGifOutput(t *testing.T) {
	workflow := `{"6":{"class_type":"CLIPTextEncode","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "gif",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "assets/{{.inputs.slug}}.webp",
			Parameters: map[string]string{"6.text": "hello"},
		}},
	}
	exec, runDir, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	exec.Inputs = map[string]string{"slug": "anim"}
	fcs.mu.Lock()
	fcs.outputs = nil
	fcs.gifs = []comfyui.OutputFile{{Filename: "out.webp", Type: "output"}}
	fcs.viewPayload = []byte("WEBPDATA7")
	fcs.mu.Unlock()
	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(runDir, "assets", "anim.webp"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "WEBPDATA7" {
		t.Errorf("output bytes = %q, want %q", got, "WEBPDATA7")
	}
}

// TestComfyUIExecutor_RejectsMultipleVideos verifies the existing
// "exactly 1 output" guard fires for a video workflow that emits two files.
// The error message points the user at batch_size since both files are
// the same kind.
func TestComfyUIExecutor_RejectsMultipleVideos(t *testing.T) {
	workflow := `{"6":{"class_type":"X","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "vid",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "out.mp4",
			Parameters: map[string]string{"6.text": "x"},
		}},
	}
	exec, _, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	fcs.mu.Lock()
	fcs.outputs = nil
	fcs.videos = []comfyui.OutputFile{
		{Filename: "a.mp4", Type: "output"},
		{Filename: "b.mp4", Type: "output"},
	}
	fcs.mu.Unlock()
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for multi-video workflow")
	}
	if !strings.Contains(err.Error(), "batch_size") {
		t.Errorf("expected batch_size hint in error, got: %v", err)
	}
}

// TestComfyUIExecutor_RejectsMixedKinds verifies a workflow that saves
// outputs of more than one kind (e.g. image + video) hits the guard with
// the per-kind tally in the message so the user knows which save node to
// drop.
func TestComfyUIExecutor_RejectsMixedKinds(t *testing.T) {
	workflow := `{"6":{"class_type":"X","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "mix",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "out.mp4",
			Parameters: map[string]string{"6.text": "x"},
		}},
	}
	exec, _, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	fcs.mu.Lock()
	fcs.outputs = []comfyui.OutputFile{{Filename: "a.png", Type: "output"}}
	fcs.videos = []comfyui.OutputFile{{Filename: "b.mp4", Type: "output"}}
	fcs.mu.Unlock()
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for mixed-kind workflow")
	}
	if !strings.Contains(err.Error(), "1 image") || !strings.Contains(err.Error(), "1 video") {
		t.Errorf("expected per-kind tally in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "multiple output kinds") {
		t.Errorf("expected mixed-kinds prose in error, got: %v", err)
	}
}

// TestCollectOutputs_DeterministicOrdering pins down the ordering contract
// collectOutputs offers callers: by node id ascending, and within a node
// images-then-videos-then-gifs. The multi-output guard will eventually rely
// on this when we expose plural `outputs:`.
func TestCollectOutputs_DeterministicOrdering(t *testing.T) {
	h := &comfyui.History{
		Outputs: map[string]comfyui.NodeOutputs{
			"20": {
				Images: []comfyui.OutputFile{{Filename: "img-20.png"}},
				Videos: []comfyui.OutputFile{{Filename: "vid-20.mp4"}},
				Gifs:   []comfyui.OutputFile{{Filename: "gif-20.webp"}},
			},
			"10": {
				Videos: []comfyui.OutputFile{{Filename: "vid-10.mp4"}},
			},
		},
	}
	files, counts := collectOutputs(h)
	wantNames := []string{"vid-10.mp4", "img-20.png", "vid-20.mp4", "gif-20.webp"}
	if len(files) != len(wantNames) {
		t.Fatalf("collectOutputs returned %d files, want %d (%+v)", len(files), len(wantNames), files)
	}
	for i, want := range wantNames {
		if files[i].Filename != want {
			t.Errorf("files[%d] = %q, want %q", i, files[i].Filename, want)
		}
	}
	if counts.Images != 1 || counts.Videos != 2 || counts.Gifs != 1 {
		t.Errorf("counts = %+v, want {Images:1 Videos:2 Gifs:1}", counts)
	}
}

// TestComfyUIExecutor_Foreach verifies a foreach comfyui stage renders one
// file per item with the per-item index threaded into the output template.
func TestComfyUIExecutor_Foreach(t *testing.T) {
	workflow := `{"6":{"class_type":"X","inputs":{"text":""}}}`
	// Upstream is a text stage producing a JSON array; the consumer fans out
	// over it. We use a stub inference so the text stage completes quickly.
	inf := func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error) {
		return `["a","b"]`, nil
	}
	pipeline := &Pipeline{
		Name: "fan",
		Stages: []Stage{
			{
				ID: "titles", Capability: "reasoning",
				Prompt: "list", Output: "titles.json", OutputFormat: "json",
			},
			{
				ID: "render", Type: StageTypeComfyUI, Capability: "image",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				// .i is the per-item index.
				Output:     "assets/img_{{.i}}.png",
				Parameters: map[string]string{"6.text": "render {{.title}}"},
			},
		},
	}
	exec, runDir, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	exec.Inference = inf
	if err := exec.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fcs.submitCount() != 2 {
		t.Fatalf("expected 2 submissions, got %d", fcs.submitCount())
	}
	for _, name := range []string{"assets/img_0.png", "assets/img_1.png"} {
		if _, err := os.ReadFile(filepath.Join(runDir, name)); err != nil {
			t.Errorf("missing output %s: %v", name, err)
		}
	}

	// Each submission's node 6 text should embed the corresponding item.
	var seen []string
	for i := 0; i < 2; i++ {
		body := fcs.submittedAt(i)
		got := body["6"].(map[string]any)["inputs"].(map[string]any)["text"]
		seen = append(seen, fmt.Sprint(got))
	}
	// Order is not guaranteed (per-item runs in parallel), so accept either
	// permutation.
	want1, want2 := "render a", "render b"
	if !((seen[0] == want1 && seen[1] == want2) || (seen[0] == want2 && seen[1] == want1)) {
		t.Errorf("rendered texts = %v, want some permutation of [%q,%q]", seen, want1, want2)
	}
}

// TestCoerceParamValue is a focused unit test for the coercion sniffer so
// regressions on edge cases land in a small, fast test rather than the full
// HTTP-roundtrip executor tests.
func TestCoerceParamValue(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"hello", "hello"},
		{"", ""},
		{"true", true},
		{"false", false},
		{"True", "True"}, // capitalized stays a string
		{"1", int64(1)},  // ints aren't booled
		{"0", int64(0)},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"3.14", 3.14},
		{"-2.5", -2.5},
		{"1e3", 1000.0}, // scientific notation parses as float
		{"42.0", 42.0},  // explicit-decimal stays a float
		{".", "."},      // pathological strconv input stays a string
		{"+", "+"},
		{"-", "-"},
		{"1.2.3", "1.2.3"}, // version-ish string stays a string
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := coerceParamValue(tc.in)
			if got != tc.want {
				t.Errorf("coerceParamValue(%q) = %v (%T), want %v (%T)", tc.in, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestComfyUIExecutor_PropagatesSubmitError verifies that a *SubmitError
// returned by the ComfyUI client surfaces as a wrapped runtime error.
func TestComfyUIExecutor_PropagatesSubmitError(t *testing.T) {
	workflow := `{"6":{"class_type":"X","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "out.png",
			Parameters: map[string]string{"6.text": "x"},
		}},
	}
	exec, _, _, _ := stubComfyRuntime(t, pipeline, workflow)

	// Redirect the backend address to a server that returns a node_errors
	// payload so the comfyui client surfaces a *SubmitError.
	failMux := http.NewServeMux()
	failMux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"prompt_id":"","number":0,"node_errors":{"6":{"class_type":"X","dependent_outputs":[],"errors":[{"type":"bad","message":"bad"}]}}}`))
	})
	failSrv := httptest.NewServer(failMux)
	t.Cleanup(failSrv.Close)

	// Replace the backend addr in the control plane via a fresh
	// stubControlWithBackend wired into a new vibe server. Simplest: reach
	// into exec's Capabilities/Vibe by reconstructing them.
	stub := &stubControlWithBackend{proxyURL: failSrv.URL, backendURL: failSrv.URL}
	vibeMux := http.NewServeMux()
	p, h := vibev1connect.NewControlServiceHandler(stub)
	vibeMux.Handle(p, h)
	vibeMux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub"}}})
	})
	vibeSrv := httptest.NewServer(vibeMux)
	t.Cleanup(vibeSrv.Close)
	exec.Vibe = vibeclient.NewWithHTTPClient(vibeSrv.URL, vibeSrv.Client(), "")

	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("expected submit error")
	}
	if !strings.Contains(err.Error(), "submit") {
		t.Errorf("expected error to mention submit, got: %v", err)
	}
	// errors.As must still find the underlying *SubmitError.
	var se *comfyui.SubmitError
	if !errors.As(err, &se) {
		t.Errorf("expected underlying *comfyui.SubmitError, got: %T %v", err, err)
	}
}

// Sanity check: a minimal client built via the executor's clientFor hook
// roundtrips a real submit/wait/save flow against the fake server.
func TestComfyUIExecutor_ClientHook(t *testing.T) {
	e := &comfyuiExecutor{pollInterval: time.Millisecond}
	fcs, srv := newFakeComfyServer(t, "p1",
		[]comfyui.OutputFile{{Filename: "x.png", Type: "output"}},
		[]byte("DATA"))
	_ = fcs
	e.newClient = func(baseURL string) *comfyui.Client {
		return comfyui.New(baseURL, srv.Client())
	}
	c := e.clientFor(srv.URL)
	id, err := c.Submit(context.Background(), map[string]any{"1": map[string]any{"class_type": "X", "inputs": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if id != "p1" {
		t.Errorf("id = %q", id)
	}
	tmp := filepath.Join(t.TempDir(), "out.png")
	if _, err := c.SaveOutputToFile(context.Background(), comfyui.OutputFile{Filename: "x.png", Type: "output"}, tmp); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(tmp)
	if string(body) != "DATA" {
		t.Errorf("save data = %q", body)
	}
}
