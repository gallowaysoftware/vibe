package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePipeline(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadPipeline_RetryValid parses a stage with an explicit retry block and
// verifies the field values land on Stage.Retry verbatim. The fast path for
// pipelines that omit retry (no Retry field at all) is covered by every
// other LoadPipeline test in this file.
func TestLoadPipeline_RetryValid(t *testing.T) {
	yaml := `
name: r
stages:
  - id: a
    capability: reasoning
    prompt: hi
    output: a.md
    retry:
      max_attempts: 5
      initial_backoff: 250ms
      max_backoff: 10s
      multiplier: 3.0
      retry_on: [transient, timeout]
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	r := p.Stages[0].Retry
	if r == nil {
		t.Fatal("expected Retry to be set")
	}
	if r.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", r.MaxAttempts)
	}
	if r.InitialBackoff != 250*time.Millisecond {
		t.Errorf("InitialBackoff = %s, want 250ms", r.InitialBackoff)
	}
	if r.MaxBackoff != 10*time.Second {
		t.Errorf("MaxBackoff = %s, want 10s", r.MaxBackoff)
	}
	if r.Multiplier != 3.0 {
		t.Errorf("Multiplier = %g, want 3.0", r.Multiplier)
	}
	if len(r.RetryOn) != 2 || r.RetryOn[0] != "transient" || r.RetryOn[1] != "timeout" {
		t.Errorf("RetryOn = %v, want [transient timeout]", r.RetryOn)
	}
}

// TestLoadPipeline_RetryDefaults verifies that omitting fields inside a
// present retry block fills in the documented defaults via Normalize.
func TestLoadPipeline_RetryDefaults(t *testing.T) {
	yaml := `
name: r
stages:
  - id: a
    capability: reasoning
    prompt: hi
    output: a.md
    retry:
      max_attempts: 3
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	r := p.Stages[0].Retry
	if r == nil {
		t.Fatal("expected Retry to be set")
	}
	if r.InitialBackoff != time.Second {
		t.Errorf("InitialBackoff default = %s, want 1s", r.InitialBackoff)
	}
	if r.MaxBackoff != 30*time.Second {
		t.Errorf("MaxBackoff default = %s, want 30s", r.MaxBackoff)
	}
	if r.Multiplier != 2.0 {
		t.Errorf("Multiplier default = %g, want 2.0", r.Multiplier)
	}
	if len(r.RetryOn) != 1 || r.RetryOn[0] != "transient" {
		t.Errorf("RetryOn default = %v, want [transient]", r.RetryOn)
	}
}

func TestLoadPipeline_Valid(t *testing.T) {
	yaml := `
name: youtube-explainer
description: test
inputs:
  topic:
    type: string
    required: true
stages:
  - id: plan
    capability: reasoning
    prompt: "Plan a video about {{.inputs.topic}}"
    output: plan.md
  - id: script
    capability: creative_writing
    prompt_file: prompts/script.tmpl
    inputs: [plan]
    output: script.md
    params:
      temperature: 0.8
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "youtube-explainer" || len(p.Stages) != 2 {
		t.Errorf("got %+v", p)
	}
	if p.Stages[1].Params["temperature"] != 0.8 {
		t.Errorf("temperature param = %v", p.Stages[1].Params["temperature"])
	}
}

// TestLoadPipeline_TypeDefaultsToText verifies that omitting `type:` continues
// to behave as a text stage (Phase 1 default), and that an explicit
// `type: text` is accepted too. This is the back-compat guard for every
// existing pipeline.
func TestLoadPipeline_TypeDefaultsToText(t *testing.T) {
	yaml := `
name: t
stages:
  - id: a
    capability: r
    prompt: hi
    output: a.md
  - id: b
    type: text
    capability: r
    prompt: hi
    output: b.md
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Type != "" {
		t.Errorf("stage 0 type = %q, want empty (defaults to text)", p.Stages[0].Type)
	}
	if p.Stages[1].Type != StageTypeText {
		t.Errorf("stage 1 type = %q, want %q", p.Stages[1].Type, StageTypeText)
	}
}

// TestLoadPipeline_AudioValid sanity-checks a minimal valid audio stage and
// confirms the optional fields land on the parsed Stage.
func TestLoadPipeline_AudioValid(t *testing.T) {
	yaml := `
name: voiceover
stages:
  - id: script
    capability: reasoning
    prompt: "say hi"
    output: script.txt
  - id: voiceover
    type: audio
    voice: en_US-lessac-medium
    text: "{{.stages.script.output}}"
    voices_dir: ~/.local/share/piper-voices
    binary: piper
    inputs: [script]
    output: voiceover.wav
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[1].Type != StageTypeAudio {
		t.Errorf("type = %q, want %q", p.Stages[1].Type, StageTypeAudio)
	}
	if p.Stages[1].Voice != "en_US-lessac-medium" {
		t.Errorf("voice = %q", p.Stages[1].Voice)
	}
	if p.Stages[1].Text != "{{.stages.script.output}}" {
		t.Errorf("text = %q", p.Stages[1].Text)
	}
	if p.Stages[1].VoicesDir != "~/.local/share/piper-voices" {
		t.Errorf("voices_dir = %q", p.Stages[1].VoicesDir)
	}
	if p.Stages[1].Binary != "piper" {
		t.Errorf("binary = %q", p.Stages[1].Binary)
	}
}

// TestLoadPipeline_AudioExampleFile parses the shipped example
// pipeline.yaml so a CI failure flags any drift between the example and the
// validator. (Pipeline files are committed alongside the code; a parse
// failure here means the example doc is out of step.)
func TestLoadPipeline_AudioExampleFile(t *testing.T) {
	// Locate the example relative to this test file's package directory.
	// `go test` runs with the package dir as CWD, so a relative path up
	// two levels lands at the repo root.
	p, err := LoadPipeline(filepath.Join("..", "..", "examples", "voiceover-pipeline", "pipeline.yaml"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if p.Name != "voiceover_smoke" {
		t.Errorf("name = %q, want voiceover_smoke", p.Name)
	}
	if len(p.Stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(p.Stages))
	}
	if p.Stages[1].Type != StageTypeAudio {
		t.Errorf("stage 1 type = %q, want %q", p.Stages[1].Type, StageTypeAudio)
	}
}

// TestLoadPipeline_AudioCapabilityOptional verifies an audio stage parses
// successfully without a `capability:` field — audio stages run as local
// subprocesses and don't activate a vibe profile.
func TestLoadPipeline_AudioCapabilityOptional(t *testing.T) {
	yaml := `
name: voiceover
stages:
  - id: voiceover
    type: audio
    voice: en_US-lessac-medium
    text: "hi"
    output: voiceover.wav
`
	if _, err := LoadPipeline(writePipeline(t, yaml)); err != nil {
		t.Fatalf("expected audio stage with no capability to validate, got: %v", err)
	}
}

// TestLoadPipeline_FFmpegValid sanity-checks a minimal valid ffmpeg stage and
// confirms the optional fields land on the parsed Stage. ffmpeg stages have
// no required `capability:` because they run as a local subprocess, mirroring
// audio stages.
func TestLoadPipeline_FFmpegValid(t *testing.T) {
	yaml := `
name: assemble
stages:
  - id: assemble
    type: ffmpeg
    binary: /usr/local/bin/ffmpeg
    ffmpeg_args:
      - "-loop"
      - "1"
      - "-i"
      - "{{ .stages.render.output }}"
      - "-c:v"
      - "libx264"
    output: final.mp4
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	st := p.Stages[0]
	if st.Type != StageTypeFFmpeg {
		t.Errorf("type = %q, want %q", st.Type, StageTypeFFmpeg)
	}
	if st.Binary != "/usr/local/bin/ffmpeg" {
		t.Errorf("binary = %q", st.Binary)
	}
	if len(st.FFmpegArgs) != 6 {
		t.Fatalf("ffmpeg_args len = %d, want 6", len(st.FFmpegArgs))
	}
	if st.FFmpegArgs[3] != "{{ .stages.render.output }}" {
		t.Errorf("ffmpeg_args[3] = %q", st.FFmpegArgs[3])
	}
	if st.Output != "final.mp4" {
		t.Errorf("output = %q", st.Output)
	}
}

// TestLoadPipeline_FFmpegExampleFile parses the shipped example
// pipeline.yaml so a CI failure flags any drift between the example and the
// validator. (Pipeline files are committed alongside the code; a parse
// failure here means the example doc is out of step.)
func TestLoadPipeline_FFmpegExampleFile(t *testing.T) {
	p, err := LoadPipeline(filepath.Join("..", "..", "examples", "video-pipeline", "pipeline.yaml"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if p.Name != "video_pipeline_smoke" {
		t.Errorf("name = %q, want video_pipeline_smoke", p.Name)
	}
	if len(p.Stages) != 4 {
		t.Fatalf("stages = %d, want 4", len(p.Stages))
	}
	if p.Stages[3].Type != StageTypeFFmpeg {
		t.Errorf("stage 3 type = %q, want %q", p.Stages[3].Type, StageTypeFFmpeg)
	}
	if len(p.Stages[3].FFmpegArgs) == 0 {
		t.Error("stage 3 has empty ffmpeg_args")
	}
}

// TestLoadPipeline_FFmpegCapabilityOptional verifies an ffmpeg stage parses
// successfully without a `capability:` field — like audio stages, ffmpeg runs
// as a local subprocess and doesn't activate a vibe profile.
func TestLoadPipeline_FFmpegCapabilityOptional(t *testing.T) {
	yaml := `
name: assemble
stages:
  - id: assemble
    type: ffmpeg
    ffmpeg_args: ["-i", "in.wav"]
    output: final.mp4
`
	if _, err := LoadPipeline(writePipeline(t, yaml)); err != nil {
		t.Fatalf("expected ffmpeg stage with no capability to validate, got: %v", err)
	}
}

// TestLoadPipeline_YouTubeValid sanity-checks a minimal valid youtube stage
// and confirms the optional fields land on the parsed Stage. Like audio and
// ffmpeg stages, youtube stages have no required `capability:` — they talk
// directly to the YouTube Data API and don't activate a vibe profile.
func TestLoadPipeline_YouTubeValid(t *testing.T) {
	yaml := `
name: pub
stages:
  - id: assemble
    type: ffmpeg
    ffmpeg_args: ["-i", "x"]
    output: final.mp4
  - id: publish
    type: youtube
    inputs: [assemble]
    video: "{{ .stages.assemble.output }}"
    title: "demo {{ .inputs.title }}"
    description: "auto-generated"
    tags: ["ai", "demo", "vibe"]
    privacy: unlisted
    category_id: "22"
    thumbnail: cover.png
    credentials_file: ~/.config/vamp/youtube-credentials.json
    output: youtube_url.txt
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	st := p.Stages[1]
	if st.Type != StageTypeYouTube {
		t.Errorf("type = %q, want %q", st.Type, StageTypeYouTube)
	}
	if st.Video != "{{ .stages.assemble.output }}" {
		t.Errorf("video = %q", st.Video)
	}
	if st.Title != "demo {{ .inputs.title }}" {
		t.Errorf("title = %q", st.Title)
	}
	if st.Privacy != "unlisted" {
		t.Errorf("privacy = %q", st.Privacy)
	}
	if st.CategoryID != "22" {
		t.Errorf("category_id = %q", st.CategoryID)
	}
	if st.Thumbnail != "cover.png" {
		t.Errorf("thumbnail = %q", st.Thumbnail)
	}
	if st.CredentialsFile != "~/.config/vamp/youtube-credentials.json" {
		t.Errorf("credentials_file = %q", st.CredentialsFile)
	}
	if strings.Join(st.Tags, ",") != "ai,demo,vibe" {
		t.Errorf("tags = %v", st.Tags)
	}
}

// TestLoadPipeline_YouTubeCapabilityOptional verifies a youtube stage parses
// successfully without a `capability:` field — youtube stages talk directly
// to the YouTube Data API, not a vibe-managed backend.
func TestLoadPipeline_YouTubeCapabilityOptional(t *testing.T) {
	yaml := `
name: pub
stages:
  - id: publish
    type: youtube
    video: final.mp4
    title: T
    description: D
    output: url.txt
`
	if _, err := LoadPipeline(writePipeline(t, yaml)); err != nil {
		t.Fatalf("expected youtube stage with no capability to validate, got: %v", err)
	}
}

// TestLoadPipeline_YouTubeExampleFile parses the shipped example pipeline so
// CI catches drift between the example YAML and the validator.
func TestLoadPipeline_YouTubeExampleFile(t *testing.T) {
	p, err := LoadPipeline(filepath.Join("..", "..", "examples", "youtube-upload", "pipeline.yaml"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if p.Name != "youtube_upload_smoke" {
		t.Errorf("name = %q, want youtube_upload_smoke", p.Name)
	}
	foundYT := false
	for _, s := range p.Stages {
		if s.Type == StageTypeYouTube {
			foundYT = true
		}
	}
	if !foundYT {
		t.Error("example pipeline contains no youtube stage")
	}
}

// TestLoadPipeline_WebhookValid parses a minimal webhook stage with inline
// body / headers / custom method and verifies the fields round-trip onto
// the Stage struct.
func TestLoadPipeline_WebhookValid(t *testing.T) {
	yaml := `
name: hk
stages:
  - id: notify
    type: webhook
    url: "https://example.com/hooks/abc"
    method: POST
    body:
      text: "done"
    headers:
      Authorization: "Bearer xyz"
    output: webhook_response.txt
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	st := p.Stages[0]
	if st.Type != StageTypeWebhook {
		t.Errorf("type = %q, want %q", st.Type, StageTypeWebhook)
	}
	if st.URL != "https://example.com/hooks/abc" {
		t.Errorf("url = %q", st.URL)
	}
	if st.Method != "POST" {
		t.Errorf("method = %q", st.Method)
	}
	if got, ok := st.Body["text"].(string); !ok || got != "done" {
		t.Errorf("body.text = %v", st.Body["text"])
	}
	if st.Headers["Authorization"] != "Bearer xyz" {
		t.Errorf("Authorization = %q", st.Headers["Authorization"])
	}
}

// TestLoadPipeline_WebhookBodyTemplateFile verifies the alternate body
// source (a templated file) parses without requiring an inline body.
func TestLoadPipeline_WebhookBodyTemplateFile(t *testing.T) {
	yaml := `
name: hk
stages:
  - id: notify
    type: webhook
    url: "https://example.com/hooks/abc"
    body_template_file: ./body.json.tmpl
    output: resp.txt
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].BodyTemplateFile != "./body.json.tmpl" {
		t.Errorf("body_template_file = %q", p.Stages[0].BodyTemplateFile)
	}
}

// TestLoadPipeline_WebhookRetryOn5xxDefault verifies that omitting
// retry_on_5xx (default true) AND retry: causes the validator to
// synthesise a default retry policy so 5xx errors are retried out of the
// box. Explicit retry_on_5xx: false suppresses the synthesised policy.
func TestLoadPipeline_WebhookRetryOn5xxDefault(t *testing.T) {
	yaml := `
name: hk
stages:
  - id: notify
    type: webhook
    url: "https://example.com/hooks/abc"
    body:
      text: "x"
    output: resp.txt
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Retry == nil {
		t.Fatal("expected synthesised Retry policy")
	}
	if p.Stages[0].Retry.MaxAttempts < 2 {
		t.Errorf("MaxAttempts = %d, want > 1 (default webhook retry)", p.Stages[0].Retry.MaxAttempts)
	}
}

// TestLoadPipeline_WebhookRetryOn5xxFalseSkipsSynth verifies that
// explicitly disabling retry_on_5xx leaves Retry nil (the user opted out).
func TestLoadPipeline_WebhookRetryOn5xxFalseSkipsSynth(t *testing.T) {
	yaml := `
name: hk
stages:
  - id: notify
    type: webhook
    url: "https://example.com/hooks/abc"
    body:
      text: "x"
    output: resp.txt
    retry_on_5xx: false
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Retry != nil {
		t.Errorf("expected Retry == nil when retry_on_5xx is false, got %+v", p.Stages[0].Retry)
	}
}

// TestLoadPipeline_WebhookExplicitRetryWins verifies a user-supplied
// retry: block is preserved verbatim — we do not overwrite or merge it.
func TestLoadPipeline_WebhookExplicitRetryWins(t *testing.T) {
	yaml := `
name: hk
stages:
  - id: notify
    type: webhook
    url: "https://example.com/hooks/abc"
    body:
      text: "x"
    output: resp.txt
    retry:
      max_attempts: 5
      retry_on: [timeout]
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Retry == nil {
		t.Fatal("expected explicit Retry policy preserved")
	}
	if p.Stages[0].Retry.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5 (user override)", p.Stages[0].Retry.MaxAttempts)
	}
	if len(p.Stages[0].Retry.RetryOn) != 1 || p.Stages[0].Retry.RetryOn[0] != "timeout" {
		t.Errorf("RetryOn = %v, want [timeout]", p.Stages[0].Retry.RetryOn)
	}
}

// TestLoadPipeline_WebhookCapabilityRejected verifies a webhook stage with
// a capability: is rejected — webhook is a plain HTTP client, no profile
// activation. Mirrors the youtube-stage behaviour.
func TestLoadPipeline_WebhookCapabilityRejected(t *testing.T) {
	yaml := `
name: hk
stages:
  - id: notify
    type: webhook
    capability: reasoning
    url: "https://example.com/hooks/abc"
    body:
      text: "x"
    output: resp.txt
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected error for webhook with capability")
	}
	if !strings.Contains(err.Error(), "capability") {
		t.Errorf("error should mention capability, got: %v", err)
	}
}

// TestLoadPipeline_WebhookExampleFile parses the shipped example pipeline
// so CI catches drift between the example YAML and the validator.
func TestLoadPipeline_WebhookExampleFile(t *testing.T) {
	p, err := LoadPipeline(filepath.Join("..", "..", "examples", "notify-pipeline", "pipeline.yaml"))
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	foundWebhook := false
	for _, s := range p.Stages {
		if s.Type == StageTypeWebhook {
			foundWebhook = true
		}
	}
	if !foundWebhook {
		t.Error("example pipeline contains no webhook stage")
	}
}

// TestLoadPipeline_ComfyUIValid sanity-checks a minimal valid comfyui stage.
func TestLoadPipeline_ComfyUIValid(t *testing.T) {
	yaml := `
name: img
stages:
  - id: render
    type: comfyui
    capability: image
    workflow: workflow.json
    output: out.png
    parameters:
      "6.text": "hello {{.inputs.topic}}"
      "3.seed": "42"
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Type != StageTypeComfyUI {
		t.Errorf("type = %q, want %q", p.Stages[0].Type, StageTypeComfyUI)
	}
	if p.Stages[0].Workflow != "workflow.json" {
		t.Errorf("workflow = %q", p.Stages[0].Workflow)
	}
	if got := p.Stages[0].Parameters["6.text"]; got != "hello {{.inputs.topic}}" {
		t.Errorf("parameter 6.text = %q", got)
	}
}

func TestLoadPipeline_ForeachDefaults(t *testing.T) {
	yaml := `
name: fan
stages:
  - id: titles
    capability: r
    prompt: "list"
    output: titles.json
    output_format: json
  - id: hooks
    capability: r
    inputs: [titles]
    foreach:
      from: titles
    prompt: "hook for {{.item}}"
    output: "hooks/{{.item | slugify}}.md"
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	hooks := p.Stages[1]
	if hooks.Foreach == nil {
		t.Fatalf("foreach was not parsed: %+v", hooks)
	}
	if hooks.Foreach.From != "titles" {
		t.Errorf("foreach.from = %q, want titles", hooks.Foreach.From)
	}
	if hooks.Foreach.Var != DefaultForeachVar {
		t.Errorf("foreach.var default = %q, want %q", hooks.Foreach.Var, DefaultForeachVar)
	}
}

func TestLoadPipeline_ForeachVarExplicit(t *testing.T) {
	yaml := `
name: fan
stages:
  - id: titles
    capability: r
    prompt: list
    output: titles.json
    output_format: json
  - id: hooks
    capability: r
    inputs: [titles]
    foreach:
      from: titles
      var: title
    prompt: "hook for {{.title}}"
    output: "hooks/{{.title | slugify}}.md"
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[1].Foreach.Var; got != "title" {
		t.Errorf("foreach.var = %q, want title", got)
	}
}

// Legacy string-form foreach must be rejected with a migration hint pointing
// users at the new structured syntax. We check both the old templated string
// (cannot unmarshal into ForeachSpec) and the dangling foreach_as field path.
func TestLoadPipeline_RejectsLegacyForeachString(t *testing.T) {
	yaml := `
name: fan
stages:
  - id: titles
    capability: r
    prompt: list
    output: titles.json
    output_format: json
  - id: hooks
    capability: r
    inputs: [titles]
    foreach: "{{.stages.titles.output}}"
    prompt: "hook for {{.item}}"
    output: "hooks/{{.item | slugify}}.md"
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected legacy foreach-string to be rejected")
	}
	if !strings.Contains(err.Error(), "foreach syntax changed") &&
		!strings.Contains(err.Error(), "foreach:\n    from:") {
		t.Errorf("expected migration hint in error, got: %v", err)
	}
}

func TestLoadPipeline_RejectsLegacyForeachAs(t *testing.T) {
	yaml := `
name: fan
stages:
  - id: titles
    capability: r
    prompt: list
    output: titles.json
    output_format: json
  - id: hooks
    capability: r
    inputs: [titles]
    foreach_as: title
    prompt: "hook for {{.title}}"
    output: "hooks/{{.title | slugify}}.md"
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected legacy foreach_as to be rejected")
	}
	if !strings.Contains(err.Error(), "foreach syntax changed") &&
		!strings.Contains(err.Error(), "foreach_as") {
		t.Errorf("expected migration hint in error, got: %v", err)
	}
}

func TestLoadPipeline_RejectsUnknownField(t *testing.T) {
	yaml := `
name: x
weird_field: nope
stages:
  - id: a
    capability: r
    prompt: hi
    output: a.md
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "weird_field") {
		t.Errorf("expected unknown-field error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "no name",
			yaml: `stages:
- id: a
  capability: r
  prompt: hi
  output: a.md`,
			wantErr: "name is required",
		},
		{
			name: "bad name",
			yaml: `name: "has spaces"
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md`,
			wantErr: "name",
		},
		{
			name: "no stages",
			yaml: `name: x
stages: []`,
			wantErr: "at least one stage",
		},
		{
			name: "duplicate stage id",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
- id: a
  capability: r
  prompt: hi
  output: b.md`,
			wantErr: "duplicate",
		},
		{
			name: "stage id with dash (template-unsafe)",
			yaml: `name: x
stages:
- id: bad-id
  capability: r
  prompt: hi
  output: a.md`,
			wantErr: "template-syntax-safe",
		},
		{
			name: "missing capability",
			yaml: `name: x
stages:
- id: a
  prompt: hi
  output: a.md`,
			wantErr: "capability is required",
		},
		{
			name: "both prompt and prompt_file",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  prompt_file: f.tmpl
  output: a.md`,
			wantErr: "exactly one",
		},
		{
			name: "neither prompt nor prompt_file",
			yaml: `name: x
stages:
- id: a
  capability: r
  output: a.md`,
			wantErr: "exactly one",
		},
		{
			name: "unknown input reference",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  inputs: [b]
  output: a.md`,
			wantErr: `input "b"`,
		},
		{
			name: "self dependency",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  inputs: [a]
  output: a.md`,
			wantErr: "depends on itself",
		},
		{
			name: "dependency cycle",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  inputs: [b]
  output: a.md
- id: b
  capability: r
  prompt: hi
  inputs: [a]
  output: b.md`,
			wantErr: "cycle",
		},
		{
			name: "bad output_format",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  output_format: xml`,
			wantErr: "output_format",
		},
		{
			name: "foreach missing from",
			yaml: `name: x
stages:
- id: src
  capability: r
  prompt: hi
  output: src.json
  output_format: json
- id: a
  capability: r
  prompt: hi
  inputs: [src]
  output: "out/{{.item}}.md"
  foreach:
    var: item`,
			wantErr: "foreach.from is required",
		},
		{
			name: "foreach.from dangling reference",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  inputs: [ghost]
  output: "out/{{.item}}.md"
  foreach:
    from: ghost`,
			wantErr: `input "ghost" does not reference`,
		},
		{
			name: "foreach.from not in inputs",
			yaml: `name: x
stages:
- id: src
  capability: r
  prompt: hi
  output: src.json
  output_format: json
- id: a
  capability: r
  prompt: hi
  output: "out/{{.item}}.md"
  foreach:
    from: src`,
			wantErr: "must also appear in inputs",
		},
		{
			name: "foreach with non-json upstream",
			yaml: `name: x
stages:
- id: src
  capability: r
  prompt: hi
  output: src.md
- id: a
  capability: r
  prompt: hi
  inputs: [src]
  output: "out/{{.item}}.md"
  foreach:
    from: src`,
			wantErr: "output_format: json",
		},
		{
			name: "foreach with static output path",
			yaml: `name: x
stages:
- id: src
  capability: r
  prompt: hi
  output: src.json
  output_format: json
- id: a
  capability: r
  prompt: hi
  inputs: [src]
  output: out.md
  foreach:
    from: src`,
			wantErr: "templated output path",
		},
		{
			name: "comfyui without workflow",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  output: a.png
  parameters:
    "6.text": hi`,
			wantErr: "workflow is required",
		},
		{
			name: "comfyui without parameters",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png`,
			wantErr: "parameters",
		},
		{
			name: "comfyui with prompt (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png
  prompt: nope
  parameters:
    "6.text": hi`,
			wantErr: "prompt is only valid on type: text",
		},
		{
			name: "comfyui with prompt_file (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png
  prompt_file: nope.tmpl
  parameters:
    "6.text": hi`,
			wantErr: "prompt_file is only valid on type: text",
		},
		{
			name: "comfyui with params (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png
  parameters:
    "6.text": hi
  params:
    temperature: 0.8`,
			wantErr: "params is only valid on type: text",
		},
		{
			name: "text stage with workflow (comfyui-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  workflow: nope.json`,
			wantErr: "workflow is only valid on type: comfyui",
		},
		{
			name: "text stage with parameters (comfyui-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  parameters:
    "6.text": hi`,
			wantErr: "parameters is only valid on type: comfyui",
		},
		{
			name: "comfyui bad parameter key shape",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png
  parameters:
    "CLIPTextEncode.text": hi`,
			wantErr: "node_id",
		},
		{
			name: "unknown stage type",
			yaml: `name: x
stages:
- id: a
  type: nope
  capability: r
  output: a.md`,
			wantErr: "type \"nope\"",
		},
		{
			name: "audio without voice",
			yaml: `name: x
stages:
- id: a
  type: audio
  text: "hi"
  output: a.wav`,
			wantErr: "voice is required",
		},
		{
			name: "audio without text",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  output: a.wav`,
			wantErr: "text is required",
		},
		{
			name: "audio output not .wav",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  text: "hi"
  output: a.mp3`,
			wantErr: "must end in .wav",
		},
		{
			name: "audio with prompt (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  text: "hi"
  prompt: "nope"
  output: a.wav`,
			wantErr: "prompt is only valid on type: text",
		},
		{
			name: "audio with prompt_file (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  text: "hi"
  prompt_file: nope.tmpl
  output: a.wav`,
			wantErr: "prompt_file is only valid on type: text",
		},
		{
			name: "audio with params (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  text: "hi"
  output: a.wav
  params:
    temperature: 0.8`,
			wantErr: "params is only valid on type: text",
		},
		{
			name: "audio with workflow (comfyui-only field)",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  text: "hi"
  output: a.wav
  workflow: wf.json`,
			wantErr: "workflow is only valid on type: comfyui",
		},
		{
			name: "text stage with voice (audio-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  voice: en_US-lessac-medium`,
			wantErr: "voice/text/voices_dir/binary are only valid on type: audio",
		},
		{
			name: "text stage with text field (audio-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  text: "nope"`,
			wantErr: "voice/text/voices_dir/binary are only valid on type: audio",
		},
		{
			name: "comfyui with voice (audio-only field)",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png
  parameters:
    "6.text": hi
  voice: nope`,
			wantErr: "voice/text/voices_dir/binary are only valid on type: audio",
		},
		{
			name: "ffmpeg without ffmpeg_args",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  output: a.mp4`,
			wantErr: "ffmpeg_args is required",
		},
		{
			name: "ffmpeg with empty ffmpeg_args list",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: []
  output: a.mp4`,
			wantErr: "ffmpeg_args is required",
		},
		{
			name: "ffmpeg with prompt (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  prompt: "nope"`,
			wantErr: "prompt is only valid on type: text",
		},
		{
			name: "ffmpeg with prompt_file (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  prompt_file: nope.tmpl`,
			wantErr: "prompt_file is only valid on type: text",
		},
		{
			name: "ffmpeg with params (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  params:
    temperature: 0.8`,
			wantErr: "params is only valid on type: text",
		},
		{
			name: "ffmpeg with output_format (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  output_format: json`,
			wantErr: "output_format is only valid on type: text",
		},
		{
			name: "ffmpeg with workflow (comfyui-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  workflow: wf.json`,
			wantErr: "workflow is only valid on type: comfyui",
		},
		{
			name: "ffmpeg with parameters (comfyui-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  parameters:
    "6.text": hi`,
			wantErr: "parameters is only valid on type: comfyui",
		},
		{
			name: "ffmpeg with voice (audio-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  voice: en_US-lessac-medium`,
			wantErr: "voice/text/voices_dir are only valid on type: audio",
		},
		{
			name: "ffmpeg with text (audio-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  text: "nope"`,
			wantErr: "voice/text/voices_dir are only valid on type: audio",
		},
		{
			name: "ffmpeg with capability (subprocess type rejects profile binding)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  capability: video
  ffmpeg_args: ["-i", "x"]
  output: a.mp4`,
			wantErr: "capability is only valid",
		},
		{
			name: "text stage with ffmpeg_args (ffmpeg-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  ffmpeg_args: ["-i", "x"]`,
			wantErr: "ffmpeg_args is only valid on type: ffmpeg",
		},
		{
			name: "audio stage with ffmpeg_args (ffmpeg-only field)",
			yaml: `name: x
stages:
- id: a
  type: audio
  voice: en_US-lessac-medium
  text: "hi"
  output: a.wav
  ffmpeg_args: ["-i", "x"]`,
			wantErr: "ffmpeg_args is only valid on type: ffmpeg",
		},
		{
			name: "comfyui stage with ffmpeg_args (ffmpeg-only field)",
			yaml: `name: x
stages:
- id: a
  type: comfyui
  capability: image
  workflow: wf.json
  output: a.png
  parameters:
    "6.text": hi
  ffmpeg_args: ["-i", "x"]`,
			wantErr: "ffmpeg_args is only valid on type: ffmpeg",
		},
		{
			name: "youtube without video",
			yaml: `name: x
stages:
- id: a
  type: youtube
  title: T
  description: D
  output: url.txt`,
			wantErr: "video is required",
		},
		{
			name: "youtube without title",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  description: D
  output: url.txt`,
			wantErr: "title is required",
		},
		{
			name: "youtube without description",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  title: T
  output: url.txt`,
			wantErr: "description is required",
		},
		{
			name: "youtube invalid privacy",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  title: T
  description: D
  privacy: secret
  output: url.txt`,
			wantErr: "privacy",
		},
		{
			name: "youtube with capability (network type rejects profile binding)",
			yaml: `name: x
stages:
- id: a
  type: youtube
  capability: upload
  video: final.mp4
  title: T
  description: D
  output: url.txt`,
			wantErr: "capability is only valid",
		},
		{
			name: "youtube with prompt (text-only field)",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  title: T
  description: D
  output: url.txt
  prompt: "nope"`,
			wantErr: "prompt is only valid on type: text",
		},
		{
			name: "youtube with workflow (comfyui-only field)",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  title: T
  description: D
  output: url.txt
  workflow: wf.json`,
			wantErr: "workflow is only valid on type: comfyui",
		},
		{
			name: "youtube with ffmpeg_args (ffmpeg-only field)",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  title: T
  description: D
  output: url.txt
  ffmpeg_args: ["-i", "x"]`,
			wantErr: "ffmpeg_args is only valid on type: ffmpeg",
		},
		{
			name: "youtube with voice (audio-only field)",
			yaml: `name: x
stages:
- id: a
  type: youtube
  video: final.mp4
  title: T
  description: D
  output: url.txt
  voice: en_US-lessac-medium`,
			wantErr: "voice/text/voices_dir are only valid on type: audio",
		},
		{
			name: "text stage with video (youtube-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  video: final.mp4`,
			wantErr: "video is only valid on type: youtube",
		},
		{
			name: "text stage with privacy (youtube-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  privacy: unlisted`,
			wantErr: "privacy is only valid on type: youtube",
		},
		{
			name: "ffmpeg stage with thumbnail (youtube-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  thumbnail: cover.png`,
			wantErr: "thumbnail is only valid on type: youtube",
		},
		{
			name: "webhook missing url",
			yaml: `name: x
stages:
- id: a
  type: webhook
  body:
    text: hi
  output: a.txt`,
			wantErr: "url is required for type: webhook",
		},
		{
			name: "webhook with unsupported method",
			yaml: `name: x
stages:
- id: a
  type: webhook
  url: https://example.com/hook
  method: TRACE
  body:
    text: hi
  output: a.txt`,
			wantErr: "method \"TRACE\" is not supported",
		},
		{
			name: "text stage with webhook url (webhook-only field)",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  url: https://example.com/hook`,
			wantErr: "url is only valid on type: webhook",
		},
		{
			name: "ffmpeg stage with webhook headers (webhook-only field)",
			yaml: `name: x
stages:
- id: a
  type: ffmpeg
  ffmpeg_args: ["-i", "x"]
  output: a.mp4
  headers:
    X-Foo: bar`,
			wantErr: "headers is only valid on type: webhook",
		},
		{
			name: "webhook with capability rejected",
			yaml: `name: x
stages:
- id: a
  type: webhook
  capability: reasoning
  url: https://example.com/hook
  body:
    text: hi
  output: a.txt`,
			wantErr: "capability is only valid on stage types that activate a vibe profile",
		},
		{
			name: "retry with negative max_attempts",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  retry:
    max_attempts: -1`,
			wantErr: "retry.max_attempts must be >= 1",
		},
		{
			name: "retry with multiplier < 1.0",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  retry:
    multiplier: 0.5`,
			wantErr: "retry.multiplier must be >= 1.0",
		},
		{
			name: "retry with unknown retry_on entry",
			yaml: `name: x
stages:
- id: a
  capability: r
  prompt: hi
  output: a.md
  retry:
    retry_on: [bogus]`,
			wantErr: "retry.retry_on entry \"bogus\"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPipeline(writePipeline(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadPipeline_RunWhenDefault verifies that omitting run_when on a
// stage normalises to "success" after parsing, matching the documented
// default and what the executor's run_when scheduler keys on.
func TestLoadPipeline_RunWhenDefault(t *testing.T) {
	yaml := `
name: rw
stages:
  - id: a
    capability: reasoning
    prompt: hi
    output: a.md
`
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[0].RunWhen; got != RunWhenSuccess {
		t.Errorf("RunWhen default = %q, want %q", got, RunWhenSuccess)
	}
}

// TestLoadPipeline_RunWhenValidValues parses every legal run_when value
// and verifies the slice entry's RunWhen lands verbatim.
func TestLoadPipeline_RunWhenValidValues(t *testing.T) {
	for _, value := range []string{RunWhenSuccess, RunWhenFailure, RunWhenAlways} {
		t.Run(value, func(t *testing.T) {
			yaml := `
name: rw
stages:
  - id: a
    capability: reasoning
    prompt: hi
    output: a.md
    run_when: ` + value + `
`
			p, err := LoadPipeline(writePipeline(t, yaml))
			if err != nil {
				t.Fatalf("LoadPipeline: %v", err)
			}
			if got := p.Stages[0].RunWhen; got != value {
				t.Errorf("RunWhen = %q, want %q", got, value)
			}
		})
	}
}

// TestLoadPipeline_RunWhenInvalid rejects a typoed run_when value at
// validate time so users see a clear error pointing at the unsupported
// keyword.
func TestLoadPipeline_RunWhenInvalid(t *testing.T) {
	yaml := `
name: rw
stages:
  - id: a
    capability: reasoning
    prompt: hi
    output: a.md
    run_when: failrue
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for invalid run_when")
	}
	if !strings.Contains(err.Error(), "run_when") {
		t.Errorf("err = %v, want message mentioning run_when", err)
	}
}
