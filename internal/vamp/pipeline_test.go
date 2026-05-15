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
			name: "forward input reference",
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
  output: b.md`,
			wantErr: "earlier stage",
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
