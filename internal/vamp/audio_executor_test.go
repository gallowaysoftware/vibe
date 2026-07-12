package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingRunner is a test double for audioRunner. It records every Run call
// (binary, args, stdin) and lets the test choose what to return. Concurrency
// support is included because the executor framework may invoke Run from
// multiple goroutines for foreach stages.
type recordingRunner struct {
	mu      sync.Mutex
	calls   []recordedRunCall
	wantErr error
	// onRun, when set, is invoked synchronously inside Run after recording
	// the call. Tests use it to simulate piper writing the WAV file (the
	// real binary does this; the stub can't without filesystem side
	// effects).
	onRun func(call recordedRunCall) error
}

type recordedRunCall struct {
	Binary string
	Args   []string
	Stdin  string
}

func (r *recordingRunner) Run(ctx context.Context, binary string, args []string, stdin string) error {
	call := recordedRunCall{Binary: binary, Args: append([]string(nil), args...), Stdin: stdin}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	hook := r.onRun
	wantErr := r.wantErr
	r.mu.Unlock()
	if hook != nil {
		if err := hook(call); err != nil {
			return err
		}
	}
	return wantErr
}

func (r *recordingRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingRunner) call(i int) recordedRunCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

// makeVoiceFile creates a stub <voicesDir>/<voice>.onnx file so the executor's
// stat check passes. Returns the resolved path.
func makeVoiceFile(t *testing.T, voicesDir, voice string) string {
	t.Helper()
	if err := os.MkdirAll(voicesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(voicesDir, voice+".onnx")
	if err := os.WriteFile(p, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// argByFlag looks up the value following a long flag (e.g. "--model") in
// args. Returns the value and a boolean indicating whether the flag was
// present.
func argByFlag(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestAudioExecutor_RendersTextAndCallsPiper(t *testing.T) {
	voicesDir := t.TempDir()
	voicePath := makeVoiceFile(t, voicesDir, "en_US-lessac-medium")
	runDir := t.TempDir()

	runner := &recordingRunner{}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "voiceover",
		Type:      StageTypeAudio,
		Voice:     "en_US-lessac-medium",
		Text:      "Hello {{.inputs.name}}, welcome.",
		VoicesDir: voicesDir,
		Output:    "voiceover.wav",
	}
	in := StageInput{
		Stage:  stage,
		Inputs: map[string]string{"name": "Kyle"},
		RunDir: runDir,
	}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("expected 1 piper invocation, got %d", runner.callCount())
	}
	call := runner.call(0)
	if call.Binary != "piper" {
		t.Errorf("binary = %q, want %q", call.Binary, "piper")
	}
	gotModel, ok := argByFlag(call.Args, "--model")
	if !ok {
		t.Fatalf("--model missing from args %v", call.Args)
	}
	if gotModel != voicePath {
		t.Errorf("--model = %q, want %q", gotModel, voicePath)
	}
	gotOut, ok := argByFlag(call.Args, "--output-file")
	if !ok {
		t.Fatalf("--output-file missing from args %v", call.Args)
	}
	wantOut := filepath.Join(runDir, "voiceover.wav")
	if gotOut != wantOut {
		t.Errorf("--output-file = %q, want %q", gotOut, wantOut)
	}
	if call.Stdin != "Hello Kyle, welcome." {
		t.Errorf("stdin = %q, want %q", call.Stdin, "Hello Kyle, welcome.")
	}
	wantFile := filepath.Join(runDir, "voiceover.wav")
	if len(out.Files) != 1 || out.Files[0] != wantFile {
		t.Errorf("out.Files = %v, want [%s]", out.Files, wantFile)
	}
}

func TestAudioExecutor_MissingVoiceFile(t *testing.T) {
	voicesDir := t.TempDir() // intentionally empty
	runDir := t.TempDir()
	runner := &recordingRunner{}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "voiceover",
		Type:      StageTypeAudio,
		Voice:     "en_US-lessac-medium",
		Text:      "hi",
		VoicesDir: voicesDir,
		Output:    "voiceover.wav",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for missing voice model")
	}
	msg := err.Error()
	expectedPath := filepath.Join(voicesDir, "en_US-lessac-medium.onnx")
	if !strings.Contains(msg, expectedPath) {
		t.Errorf("error does not mention expected voice path %q: %v", expectedPath, msg)
	}
	if !strings.Contains(msg, "huggingface.co/rhasspy/piper-voices") {
		t.Errorf("error does not mention download hint: %v", msg)
	}
	if runner.callCount() != 0 {
		t.Errorf("piper should not have been invoked on missing voice; got %d calls", runner.callCount())
	}
}

func TestAudioExecutor_TemplateRendersFromPriorStage(t *testing.T) {
	voicesDir := t.TempDir()
	makeVoiceFile(t, voicesDir, "en_US-lessac-medium")
	runDir := t.TempDir()

	runner := &recordingRunner{}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "voiceover",
		Type:      StageTypeAudio,
		Voice:     "en_US-lessac-medium",
		Text:      "{{.stages.script.output}}",
		VoicesDir: voicesDir,
		Output:    "voiceover.wav",
		Inputs:    []string{"script"},
	}
	prior := map[string]*stageResult{
		"script": {Output: "A short narration about robots."},
	}
	in := StageInput{
		Stage:  stage,
		Prior:  prior,
		RunDir: runDir,
	}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("expected 1 call, got %d", runner.callCount())
	}
	got := runner.call(0).Stdin
	want := "A short narration about robots."
	if got != want {
		t.Errorf("stdin = %q, want %q", got, want)
	}
}

func TestAudioExecutor_PiperFails(t *testing.T) {
	voicesDir := t.TempDir()
	makeVoiceFile(t, voicesDir, "en_US-lessac-medium")
	runDir := t.TempDir()

	runner := &recordingRunner{
		wantErr: errors.New("exit status 1: failed to load model: shape mismatch"),
	}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "voiceover",
		Type:      StageTypeAudio,
		Voice:     "en_US-lessac-medium",
		Text:      "hi",
		VoicesDir: voicesDir,
		Output:    "voiceover.wav",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected error from piper failure")
	}
	if !strings.Contains(err.Error(), "shape mismatch") {
		t.Errorf("error does not surface piper stderr: %v", err)
	}
	// The error should also identify the stage so a multi-stage failure can
	// be diagnosed without inspecting goroutine state.
	if !strings.Contains(err.Error(), "voiceover") {
		t.Errorf("error does not mention stage id: %v", err)
	}
}

// TestAudioExecutor_DefaultBinaryAndVoicesDir verifies the production fallback
// resolution: when a stage doesn't pin a binary or voices_dir, the executor
// uses "piper" + ~/.local/share/piper-voices. The audioExecutor test hooks
// (defaultBinary / defaultVoicesDir) let us point the latter at a temp dir
// without touching the real home.
func TestAudioExecutor_DefaultBinaryAndVoicesDir(t *testing.T) {
	voicesDir := t.TempDir()
	voicePath := makeVoiceFile(t, voicesDir, "en_US-amy-low")
	runDir := t.TempDir()

	runner := &recordingRunner{}
	exec := &audioExecutor{
		runner:           runner,
		defaultVoicesDir: voicesDir,
		// defaultBinary left empty -> falls back to "piper"
	}
	stage := &Stage{
		ID:     "voiceover",
		Type:   StageTypeAudio,
		Voice:  "en_US-amy-low",
		Text:   "hello",
		Output: "voiceover.wav",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	call := runner.call(0)
	if call.Binary != "piper" {
		t.Errorf("binary = %q, want piper", call.Binary)
	}
	gotModel, _ := argByFlag(call.Args, "--model")
	if gotModel != voicePath {
		t.Errorf("--model = %q, want %q", gotModel, voicePath)
	}
}

// TestAudioExecutor_TildeExpansionInVoicesDir verifies a "~/" prefix in
// voices_dir resolves to $HOME at execution time. We swap HOME for a temp
// dir for the duration of the test so the assertion doesn't depend on the
// host's real home.
func TestAudioExecutor_TildeExpansionInVoicesDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	voicesDir := filepath.Join(tmpHome, "piper-voices")
	makeVoiceFile(t, voicesDir, "en_US-lessac-medium")
	runDir := t.TempDir()

	runner := &recordingRunner{}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "voiceover",
		Type:      StageTypeAudio,
		Voice:     "en_US-lessac-medium",
		Text:      "hi",
		VoicesDir: "~/piper-voices",
		Output:    "voiceover.wav",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	gotModel, _ := argByFlag(runner.call(0).Args, "--model")
	wantModel := filepath.Join(voicesDir, "en_US-lessac-medium.onnx")
	if gotModel != wantModel {
		t.Errorf("--model = %q, want %q", gotModel, wantModel)
	}
}

// TestAudioExecutor_TemplatedOutputPath verifies the Output template (e.g.
// "audio/{{.i}}.wav") renders with the foreach binding and the resolved path
// flows through to --output-file.
func TestAudioExecutor_TemplatedOutputPath(t *testing.T) {
	voicesDir := t.TempDir()
	makeVoiceFile(t, voicesDir, "v")
	runDir := t.TempDir()

	runner := &recordingRunner{}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "voice",
		Type:      StageTypeAudio,
		Voice:     "v",
		Text:      "item {{.item}}",
		VoicesDir: voicesDir,
		Output:    "audio/clip_{{.i}}.wav",
		Foreach:   &ForeachSpec{From: "src", Var: "item"},
	}
	in := StageInput{
		Stage:   stage,
		RunDir:  runDir,
		Item:    "hello",
		ItemIdx: 3,
	}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	gotOut, _ := argByFlag(runner.call(0).Args, "--output-file")
	wantOut := filepath.Join(runDir, "audio", "clip_3.wav")
	if gotOut != wantOut {
		t.Errorf("--output-file = %q, want %q", gotOut, wantOut)
	}
	wantFile := filepath.Join(runDir, "audio", "clip_3.wav")
	if len(out.Files) != 1 || out.Files[0] != wantFile {
		t.Errorf("out.Files = %v, want [%s]", out.Files, wantFile)
	}
	if runner.call(0).Stdin != "item hello" {
		t.Errorf("stdin = %q, want %q", runner.call(0).Stdin, "item hello")
	}
	// The output directory should have been created so piper can write to it.
	if _, err := os.Stat(filepath.Join(runDir, "audio")); err != nil {
		t.Errorf("output dir not created: %v", err)
	}
}

// TestAudioExecutor_CustomBinary verifies a stage-level binary override is
// honored on the argv.
func TestAudioExecutor_CustomBinary(t *testing.T) {
	voicesDir := t.TempDir()
	makeVoiceFile(t, voicesDir, "v")
	runDir := t.TempDir()
	runner := &recordingRunner{}
	exec := &audioExecutor{runner: runner}
	stage := &Stage{
		ID:        "v",
		Type:      StageTypeAudio,
		Voice:     "v",
		Text:      "hi",
		VoicesDir: voicesDir,
		Binary:    "/opt/piper/piper",
		Output:    "v.wav",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := runner.call(0).Binary; got != "/opt/piper/piper" {
		t.Errorf("binary = %q, want /opt/piper/piper", got)
	}
}

// TestAudioExecutor_RunGroupSkipsProfile verifies the end-to-end scheduler
// path runs a single audio stage without trying to activate any profile.
// This is the executor-level guard against the "audio stages need a vibe
// profile" trap; it also documents the runtime path the example pipeline
// will take when the upstream is purely audio.
func TestAudioExecutor_RunGroupSkipsProfile(t *testing.T) {
	voicesDir := t.TempDir()
	makeVoiceFile(t, voicesDir, "v")

	runner := &recordingRunner{}
	pipeline := &Pipeline{
		Name: "tts",
		Stages: []Stage{{
			ID:        "voice",
			Type:      StageTypeAudio,
			Voice:     "v",
			Text:      "hello",
			VoicesDir: voicesDir,
			Output:    "voice.wav",
		}},
	}
	// Empty Capabilities is fine: the runner short-circuits profile
	// resolution for audio-only groups, so the unmapped capability never
	// trips the scheduler.
	exec, runDir := stubExecutor(t, pipeline, &Capabilities{Mapping: map[string]CapabilityBinding{}}, nil)
	// Inject our recording runner via the registry seam: re-register the
	// audio executor with the stub runner. We can't rely on Run building
	// the registry because it runs before we'd get a chance to override;
	// instead, set Inference to a no-op (won't be called) and rely on a
	// pre-built registry by calling Run after Pipeline assignment but
	// pre-empting the production audio executor through a hook on the
	// stage's binary. Simpler path: replace the default executor by
	// patching the registry post-build is hard. So we use a custom binary
	// that resolves to a script we control.
	//
	// Instead, we use the executor-level injection by overriding the
	// audioExecutor's runner via an exported helper. The simplest way to
	// preserve test hygiene is to run the executor directly (we already
	// have dedicated tests for the Execute method). Here we instead
	// exercise the runGroup short-circuit by constructing a manual run.
	exec.Inference = nil
	// Replace the registry entry. Run() builds it at the start, so we run
	// Run with a hook that swaps the audio executor immediately after
	// build. The cleanest path is to expose registry overrides on the
	// Executor; lacking that, we exercise the short-circuit via a manual
	// invocation of runGroup-equivalent logic: just check that Run
	// completes without a Capabilities lookup, then assert via the stub
	// runner.
	if err := runWithAudioRunner(exec, runner); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("expected 1 piper call, got %d", runner.callCount())
	}
	// And the resulting output path is reported relative to RunDir.
	out := filepath.Join(runDir, "voice.wav")
	gotOut, _ := argByFlag(runner.call(0).Args, "--output-file")
	if gotOut != out {
		t.Errorf("--output-file = %q, want %q", gotOut, out)
	}
}

// runWithAudioRunner runs the executor's pipeline after swapping the audio
// executor entry in the post-Run registry with one driven by the supplied
// audioRunner. Because Run() builds the registry itself, we replicate Run's
// minimal scaffolding here in test-only code: create the run dir, set
// Inference, build a registry that uses our stubbed audio executor, then
// invoke the scheduling loop. This keeps the production registry-construction
// path inside Run() untouched.
func runWithAudioRunner(e *Executor, runner audioRunner) error {
	if err := os.MkdirAll(e.RunDir, 0o755); err != nil {
		return err
	}
	if err := e.snapshot(); err != nil {
		return fmt.Errorf("snapshot run inputs: %w", err)
	}
	e.registry = map[StageType]StageExecutor{
		StageTypeText:  &visionExecutor{inference: e.Inference},
		StageTypeAudio: &audioExecutor{runner: runner},
	}
	// Use the same wave-scheduler the production Run does. We can't call
	// e.Run directly because it would rebuild the registry; instead,
	// inline the minimum loop. Keep this in sync with Run if the
	// scheduler grows new pre-loop setup.
	byID := make(map[string]*Stage, len(e.Pipeline.Stages))
	indeg := make(map[string]int, len(e.Pipeline.Stages))
	dependents := make(map[string][]string, len(e.Pipeline.Stages))
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		byID[st.ID] = st
		indeg[st.ID] = len(st.Inputs)
	}
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		for _, dep := range st.Inputs {
			dependents[dep] = append(dependents[dep], st.ID)
		}
	}
	e.stageOutputs = make(map[string]*stageResult, len(e.Pipeline.Stages))
	remaining := len(e.Pipeline.Stages)
	for remaining > 0 {
		var ready []*Stage
		for id, deg := range indeg {
			if deg == 0 {
				ready = append(ready, byID[id])
			}
		}
		if len(ready) == 0 {
			return fmt.Errorf("scheduler deadlock: %d stages remain", remaining)
		}
		for _, st := range ready {
			delete(indeg, st.ID)
		}
		groups := make(map[string][]*Stage)
		for _, st := range ready {
			groups[st.Capability] = append(groups[st.Capability], st)
		}
		for cap, group := range groups {
			if err := e.runGroup(context.Background(), cap, group); err != nil {
				return err
			}
			for _, st := range group {
				for _, child := range dependents[st.ID] {
					if _, ok := indeg[child]; ok {
						indeg[child]--
					}
				}
				remaining--
			}
		}
	}
	return nil
}

// TestAudioExecutor_KokoroEngine_PostsToEndpointAndWritesWAV covers the
// kokoro audio path: the executor posts a JSON body matching the OpenAI
// /v1/audio/speech shape to EngineURL and writes the response body as the
// stage output WAV. Stubs an httptest server in place of Kokoro-FastAPI.
func TestAudioExecutor_KokoroEngine_PostsToEndpointAndWritesWAV(t *testing.T) {
	var gotBody []byte
	var gotPath string
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		// Minimal sentinel WAV: RIFF header bytes are enough that the test
		// can assert byte-identity without smuggling a real codec in here.
		_, _ = w.Write([]byte("RIFF....WAVEsentinel"))
	}))
	defer srv.Close()

	runDir := t.TempDir()
	exec := &audioExecutor{}
	out, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:        "audio",
			Type:      StageTypeAudio,
			Voice:     "af_bella",
			Text:      "Hello, lecture world.",
			Output:    "out.wav",
			Engine:    AudioEngineKokoro,
			EngineURL: srv.URL,
		},
		RunDir: runDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/audio/speech" {
		t.Errorf("path = %q, want /v1/audio/speech", gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["voice"] != "af_bella" || body["input"] != "Hello, lecture world." || body["model"] != "kokoro" || body["response_format"] != "wav" {
		t.Errorf("unexpected body: %+v", body)
	}
	if len(out.Files) != 1 {
		t.Fatalf("expected one output file, got %v", out.Files)
	}
	got, err := os.ReadFile(out.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	// The body is written through patchRIFFSize, which rewrites bytes
	// 4..8 to the actual (total - 8) size. RIFF magic, WAVE tag, and
	// payload must round-trip verbatim. The original kokoro response
	// in this test was 20 bytes long ("RIFF....WAVEsentinel"); the
	// expected RIFF size is therefore 12 (little-endian u32).
	if string(got[0:4]) != "RIFF" {
		t.Errorf("RIFF magic clobbered: %q", string(got[0:4]))
	}
	if string(got[8:12]) != "WAVE" {
		t.Errorf("WAVE tag clobbered: %q", string(got[8:12]))
	}
	if string(got[12:]) != "sentinel" {
		t.Errorf("payload modified: %q", string(got[12:]))
	}
	wantSize := uint32(len(got) - 8)
	gotSize := uint32(got[4]) | uint32(got[5])<<8 | uint32(got[6])<<16 | uint32(got[7])<<24
	if gotSize != wantSize {
		t.Errorf("RIFF size = %d, want %d (patchRIFFSize should rewrite)", gotSize, wantSize)
	}
}

// TestAudioExecutor_KokoroEngine_400IsAStageError checks that a non-2xx
// response surfaces as a stage error with the server's body included, so
// a misconfigured voice or unreachable server fails loudly rather than
// silently producing an empty WAV.
func TestAudioExecutor_KokoroEngine_400IsAStageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unknown voice"}`))
	}))
	defer srv.Close()
	runDir := t.TempDir()
	exec := &audioExecutor{}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:        "audio",
			Type:      StageTypeAudio,
			Voice:     "af_nope",
			Text:      "x",
			Output:    "out.wav",
			Engine:    AudioEngineKokoro,
			EngineURL: srv.URL,
		},
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("expected error on 400 response")
	}
	if !strings.Contains(err.Error(), "unknown voice") {
		t.Errorf("error should surface server body: %v", err)
	}
}

// TestValidateWAVResponse covers the WAV-envelope sanity check used by
// the kokoro path to detect intermittent empty-body / truncated-header
// responses. The classifier in exec.go promotes any error containing
// audioInvalidOutputTag to a retry under retry_on: [invalid_output],
// so these failure modes must produce errors carrying that tag.
func TestValidateWAVResponse(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		wantErr bool
	}{
		{"empty", nil, true},
		{"too_short", []byte("RIFF"), true},
		{"missing_riff_magic", []byte("XXXX....WAVE"), true},
		{"missing_wave_tag", []byte("RIFF....XXXX"), true},
		{"valid_header_minimum", []byte("RIFF....WAVE"), false},
		{"valid_header_with_payload", []byte("RIFF....WAVEfmt extra payload"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateWAVResponse(c.body)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateWAVResponse(%q): got err=%v, wantErr=%v", c.body, err, c.wantErr)
			}
		})
	}
}

// TestAudioExecutor_KokoroEngine_EmptyBodyTaggedForRetry verifies that
// a 200-OK with an empty body produces an error carrying the
// audioInvalidOutputTag, which is the substring the retry classifier
// matches under retry_on: [invalid_output]. Without this, the empty-
// body race that bit Module 3 (one chunk out of 1500+ during kokoro
// warm-up) would still fail the whole foreach.
func TestAudioExecutor_KokoroEngine_EmptyBodyTaggedForRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Intentionally write nothing — repro of the kokoro warm-up
		// race condition.
	}))
	defer srv.Close()
	runDir := t.TempDir()
	exec := &audioExecutor{}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:        "audio",
			Type:      StageTypeAudio,
			Voice:     "af_bella",
			Text:      "x",
			Output:    "out.wav",
			Engine:    AudioEngineKokoro,
			EngineURL: srv.URL,
		},
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("expected error on empty-body response")
	}
	if !strings.Contains(err.Error(), audioInvalidOutputTag) {
		t.Errorf("error %q must contain audioInvalidOutputTag (%q) so retry classifier picks it up", err.Error(), audioInvalidOutputTag)
	}
}

// TestPatchRIFFSize verifies the RIFF header normalisation that silences
// ffmpeg "Ignoring maximum wav data size, file may be invalid" warnings
// during downstream concat. Kokoro-FastAPI writes a header whose RIFF
// size doesn't match the streamed body; we rewrite it in place to be
// (total_len - 8) so file inspectors agree with reality.
func TestPatchRIFFSize(t *testing.T) {
	// Craft a 32-byte synthetic WAV with a deliberately-wrong RIFF size.
	wav := make([]byte, 32)
	copy(wav[0:4], "RIFF")
	wav[4], wav[5], wav[6], wav[7] = 0xFF, 0xFF, 0xFF, 0x7F // bogus
	copy(wav[8:12], "WAVE")
	patchRIFFSize(wav)
	want := uint32(len(wav) - 8) // 24
	got := uint32(wav[4]) | uint32(wav[5])<<8 | uint32(wav[6])<<16 | uint32(wav[7])<<24
	if got != want {
		t.Errorf("RIFF size = %d, want %d", got, want)
	}
	// Magic words must be preserved.
	if string(wav[0:4]) != "RIFF" {
		t.Errorf("RIFF magic clobbered: %q", string(wav[0:4]))
	}
	if string(wav[8:12]) != "WAVE" {
		t.Errorf("WAVE tag clobbered: %q", string(wav[8:12]))
	}
}
