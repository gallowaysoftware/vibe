package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePipeline(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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
