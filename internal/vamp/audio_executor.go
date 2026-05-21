package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// audioExecutor implements StageExecutor for type: audio stages. It synthesizes
// speech by shelling out to a Piper TTS binary
// (https://github.com/rhasspy/piper). Piper takes plain text on stdin and
// writes WAV audio to the file passed via --output-file. The stage renders
// Stage.Text as a Go template, resolves Stage.Voice to a voice model file
// under VoicesDir, and invokes:
//
//	<binary> --model <voicesDir>/<voice>.onnx --output-file <runDir>/<output>
//
// piping the rendered text to stdin. The result is reported as a single file
// in StageOutput.Files so the runner skips its text write-back path (the
// binary, not vamp, produced the file).
type audioExecutor struct {
	// runner is the injectable subprocess driver. Tests stub this so they
	// can record argv + stdin without spawning a real piper. nil means use
	// execCommandRunner (the os/exec-backed default).
	runner audioRunner
	// defaultVoicesDir, when non-empty, overrides
	// "~/.local/share/piper-voices" as the fallback when a stage doesn't
	// specify voices_dir. Used by tests; production callers leave it empty.
	defaultVoicesDir string
	// defaultBinary, when non-empty, overrides "piper" as the fallback
	// binary name. Used by tests; production callers leave it empty.
	defaultBinary string
	// httpClient is the injectable HTTP client used by the kokoro engine
	// path. Tests swap this for a recorder; production callers leave it
	// nil and we use http.DefaultClient.
	httpClient *http.Client
}

// AudioEngine* constants name the supported TTS backends for `type: audio`
// stages. Centralised so the validator, executor, and cache key all agree.
const (
	AudioEnginePiper  = "piper"
	AudioEngineKokoro = "kokoro"
)

// defaultKokoroURL is the localhost address Kokoro-FastAPI binds to in its
// shipped docker-compose. Surfaced as a default so a pipeline that doesn't
// pin engine_url still works against a typical local install.
const defaultKokoroURL = "http://127.0.0.1:8880"

// audioRunner is the subprocess driver interface used by audioExecutor. The
// real implementation is execCommandRunner; tests replace it with a recorder
// so they can assert argv + stdin without invoking piper.
type audioRunner interface {
	// Run executes binary with the given args, writing stdin to its standard
	// input. On non-zero exit it should return an error whose message
	// surfaces the captured stderr.
	Run(ctx context.Context, binary string, args []string, stdin string) error
}

// Compile-time guarantee that audioExecutor satisfies StageExecutor.
var _ StageExecutor = (*audioExecutor)(nil)

// defaultVoicesDirPath is the per-user default directory Piper voice files
// (".onnx") are downloaded into. Used when a stage doesn't override
// voices_dir.
const defaultVoicesDirPath = "~/.local/share/piper-voices"

// piperVoicesURL points at the official Hugging Face mirror of Piper voices,
// surfaced in error messages so a user hitting a missing-voice error knows
// where to grab the .onnx file.
const piperVoicesURL = "https://huggingface.co/rhasspy/piper-voices"

// Execute synthesizes one piece of speech audio. For foreach stages the
// runner calls this once per item with StageInput.Item populated; non-foreach
// invocations have Item=nil and ItemIdx=0.
//
// Side effects: writes the rendered .wav file to <RunDir>/<rendered output>
// and reports that path in StageOutput.Files as an ABSOLUTE path. Downstream
// stages render subprocesses (ffmpeg) from the daemon's CWD, so a relative
// path wouldn't resolve there; absolute is the only safe shape.
func (a *audioExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("audio: missing stage")
	}
	if st.Voice == "" {
		return nil, fmt.Errorf("stage %s: voice is required for type: audio", st.ID)
	}
	if st.Text == "" {
		return nil, fmt.Errorf("stage %s: text is required for type: audio", st.ID)
	}

	// Foreach binding goes into both text-template rendering and the
	// output-path render below so users can template both per item.
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}

	// Render the text template. Stage.Text is a separate field from
	// Stage.Prompt because text stages route prompts through a chat
	// completion (with system/user role wrapping conceptually owned by
	// the LLM) while audio stages feed raw rendered text directly to
	// the TTS engine. Reusing Stage.Prompt would conflate "LLM prompt"
	// with "literal speech transcript" and force the validator to
	// accept the same field for two semantically different inputs.
	text, err := renderTemplate(st.ID+":text", st.Text, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render text: %w", st.ID, err)
	}

	// Render the output path. Audio stages own their own output write so
	// we render here and pass the absolute path to whichever engine writes
	// the file (piper subprocess via --output-file, kokoro path via our
	// own os.WriteFile after the HTTP response).
	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	if !strings.HasSuffix(outRel, ".wav") {
		return nil, fmt.Errorf("stage %s: rendered output %q must end in .wav", st.ID, outRel)
	}
	outAbs := filepath.Join(in.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: create output dir: %w", st.ID, err)
	}

	engine := st.Engine
	if engine == "" {
		engine = AudioEnginePiper
	}
	switch engine {
	case AudioEngineKokoro:
		return a.executeKokoro(ctx, in, st, text, outAbs, outRel)
	case AudioEnginePiper:
		// fallthrough to legacy piper path below
	default:
		return nil, fmt.Errorf("stage %s: unknown audio engine %q (allowed: %q, %q)", st.ID, engine, AudioEnginePiper, AudioEngineKokoro)
	}

	// Resolve the voice model path. Stat upfront so a missing file fails
	// before we spawn piper (which would surface a cryptic "model load
	// failed" via stderr).
	voicesDir := st.VoicesDir
	if voicesDir == "" {
		voicesDir = a.defaultVoicesDir
	}
	if voicesDir == "" {
		voicesDir = defaultVoicesDirPath
	}
	voicesDir = expandAudioTilde(voicesDir)
	voicePath := filepath.Join(voicesDir, st.Voice+".onnx")
	if _, err := os.Stat(voicePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stage %s: voice model %q not found at %s (download Piper voices from %s and place the .onnx file in %s)", st.ID, st.Voice, voicePath, piperVoicesURL, voicesDir)
		}
		return nil, fmt.Errorf("stage %s: stat voice model %s: %w", st.ID, voicePath, err)
	}

	binary := st.Binary
	if binary == "" {
		binary = a.defaultBinary
	}
	if binary == "" {
		binary = "piper"
	}
	args := []string{"--model", voicePath, "--output-file", outAbs}

	runner := a.runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	// Stream a status line for the live single-stage path so users see
	// something happen between "queued" and "wav written".
	if in.Log != nil {
		fmt.Fprintf(in.Log, "piper: %s (voice=%s, %d chars)\n", outRel, st.Voice, len(text))
	}
	if err := runner.Run(ctx, binary, args, text); err != nil {
		return nil, fmt.Errorf("stage %s: piper: %w", st.ID, err)
	}
	// Report ABSOLUTE path: downstream {{ .stages.X.output(s) }} references
	// land on argv strings consumed by ffmpeg/etc. subprocesses running from
	// the daemon's CWD, which can't resolve a path relative to RunDir.
	return &StageOutput{Files: []string{outAbs}}, nil
}

// executeKokoro routes the stage through an OpenAI-compatible TTS endpoint
// (Kokoro-FastAPI by default — ghcr.io/remsky/kokoro-fastapi-gpu). One HTTP
// call per Execute invocation; chunk size for long lecture segments is the
// upstream pipeline's responsibility (a chunk_for_tts LLM stage that splits
// each segment into ~100-200-token chunks before this stage's foreach runs).
//
// Wire shape: POST {EngineURL}/v1/audio/speech with
// `{"model": "kokoro", "input": "<text>", "voice": "<voice>", "response_format": "wav"}`,
// raw WAV bytes back in the response body. Server timeout is generous (2 min
// per chunk) to tolerate startup latency on cold starts.
func (a *audioExecutor) executeKokoro(ctx context.Context, in StageInput, st *Stage, text, outAbs, outRel string) (*StageOutput, error) {
	// Precedence: explicit engine_url > active vibe profile's proxy URL
	// (when the stage declares a capability) > the localhost
	// Kokoro-FastAPI default. The capability path is the right answer for
	// the vamp+vibe co-managed setup — vibe activates the tts_kokoro
	// http_server backend before this stage runs and points its proxy at
	// the container, so in.BaseURL routes here without the pipeline
	// hardcoding the container's port.
	base := strings.TrimRight(st.EngineURL, "/")
	if base == "" && st.Capability != "" && in.BaseURL != "" {
		base = strings.TrimRight(in.BaseURL, "/")
	}
	if base == "" {
		base = defaultKokoroURL
	}
	body, err := json.Marshal(map[string]any{
		"model":           "kokoro",
		"input":           text,
		"voice":           st.Voice,
		"response_format": "wav",
	})
	if err != nil {
		return nil, fmt.Errorf("stage %s: marshal kokoro request: %w", st.ID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("stage %s: build kokoro request: %w", st.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	if in.Log != nil {
		fmt.Fprintf(in.Log, "kokoro: %s (voice=%s, %d chars) -> %s\n", outRel, st.Voice, len(text), base)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stage %s: kokoro request: %w", st.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("stage %s: kokoro %s: %s", st.ID, resp.Status, strings.TrimSpace(string(msg)))
	}
	wav, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("stage %s: read kokoro response: %w", st.ID, err)
	}
	if len(wav) == 0 {
		return nil, fmt.Errorf("stage %s: kokoro returned empty body", st.ID)
	}
	if err := os.WriteFile(outAbs, wav, 0o644); err != nil {
		return nil, fmt.Errorf("stage %s: write %s: %w", st.ID, outAbs, err)
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// execCommandRunner is the production audioRunner. It spawns the piper binary
// via os/exec, pipes stdin in, captures stderr, and reports any non-zero exit
// with the captured stderr verbatim so users see piper's own error message.
type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, binary string, args []string, stdin string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Discard stdout: piper writes its WAV to --output-file, not stdout.
	// Anything it does emit (progress, model-load messages) is noise we'd
	// otherwise interleave with the scheduler's own status lines.
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// expandAudioTilde expands a leading "~/" against the user's home dir. Kept
// private to the audio executor to avoid coupling vamp to vibe's profile
// package; the surface is identical (anchored prefix only, no general shell
// expansion).
func expandAudioTilde(p string) string {
	if p == "" || !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
