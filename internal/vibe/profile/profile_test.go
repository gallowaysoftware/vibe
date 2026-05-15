package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubModelFile creates a placeholder file so model.path existence checks pass.
func stubModelFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: code
description: test
model:
  path: ` + model + `
  alias: qwen3-coder-30b
  context: 190000
  parallel: 1
  gpu_layers: 999
  flash_attn: true
  cache_type_k: q8_0
  jinja: true
  extra_args:
    - --no-mmap
frontend:
  kind: external
  app: opencode
  restart_required: true
  write_file: /tmp/opencode.json
  template:
    provider:
      vibe-local:
        npm: "@ai-sdk/openai-compatible"
        options:
          baseURL: ${VIBE_API}
        models:
          ${MODEL_ALIAS}:
            limit:
              context: ${MODEL_CONTEXT}
    model: vibe-local/${MODEL_ALIAS}
estimated_vram_gb: 26
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "code" {
		t.Errorf("name = %q", p.Name)
	}
	if p.Model.Context != 190000 {
		t.Errorf("context = %d", p.Model.Context)
	}
	if !p.Model.FlashAttn {
		t.Errorf("flash_attn = false")
	}
	if len(p.Model.ExtraArgs) != 1 || p.Model.ExtraArgs[0] != "--no-mmap" {
		t.Errorf("extra_args = %v", p.Model.ExtraArgs)
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: code
model:
  path: ` + model + `
  alias: x
  context: 1024
  weird_field: nope
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x.json
  template:
    a: 1
`
	_, err := Load(writeProfile(t, yaml))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "weird_field") {
		t.Errorf("err = %v, want mention of weird_field", err)
	}
}

func TestLoad_TildeExpansion(t *testing.T) {
	home, _ := os.UserHomeDir()
	// Create a real file under home so validation passes.
	dir, err := os.MkdirTemp(home, "vibe-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	modelPath := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(modelPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, modelPath)
	if err != nil {
		t.Fatal(err)
	}

	yaml := `
name: code
model:
  path: ~/` + rel + `
  alias: x
  context: 1024
frontend:
  kind: external
  app: opencode
  write_file: ~/oc.json
  template: {a: 1}
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Model.Path != modelPath {
		t.Errorf("model.path = %q, want %q", p.Model.Path, modelPath)
	}
	if !strings.HasPrefix(p.Frontend.WriteFile, home) {
		t.Errorf("write_file = %q, want home prefix", p.Frontend.WriteFile)
	}
}

func TestValidate(t *testing.T) {
	model := stubModelFile(t)
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing name",
			yaml: `
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "name is required",
		},
		{
			name: "bad name",
			yaml: `
name: "has spaces"
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "must match",
		},
		{
			name: "missing alias",
			yaml: `
name: x
model: {path: ` + model + `, context: 1024}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "model.alias is required",
		},
		{
			name: "zero context",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 0}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "model.context must be > 0",
		},
		{
			name: "model file missing",
			yaml: `
name: x
model: {path: /nonexistent/model.gguf, alias: x, context: 1024}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "model.path",
		},
		{
			name: "unknown kind",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {kind: weird, app: x, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "is unknown",
		},
		{
			name: "missing kind",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {app: x, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "frontend.kind is required",
		},
		{
			name: "external missing write_file",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {kind: external, app: opencode, template: {a: 1}}
`,
			wantErr: "write_file is required",
		},
		{
			name: "external missing template",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {kind: external, app: opencode, write_file: /tmp/x}
`,
			wantErr: "template is required",
		},
		{
			name: "docker-compose not yet supported",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend: {kind: docker-compose, app: x}
`,
			wantErr: "not supported yet",
		},
		{
			name: "duplicate mcp names",
			yaml: `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template: {a: 1}
  mcps: [datadog, jira, datadog]
`,
			wantErr: `duplicate "datadog"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeProfile(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestExpandTemplate_PreservesIntType(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: x
model: {path: ` + model + `, alias: my-model, context: 8192}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template:
    provider:
      vibe-local:
        options:
          baseURL: ${VIBE_API}
        models:
          ${MODEL_ALIAS}:
            limit:
              context: ${MODEL_CONTEXT}
    model: vibe-local/${MODEL_ALIAS}
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.ExpandTemplate(ExpandContext{
		VibeAPI:      "http://127.0.0.1:9000/v1",
		ModelAlias:   "my-model",
		ModelContext: 8192,
	})
	if err != nil {
		t.Fatal(err)
	}

	if out["model"] != "vibe-local/my-model" {
		t.Errorf("model = %v", out["model"])
	}
	prov := out["provider"].(map[string]any)["vibe-local"].(map[string]any)
	if prov["options"].(map[string]any)["baseURL"] != "http://127.0.0.1:9000/v1" {
		t.Errorf("baseURL = %v", prov["options"].(map[string]any)["baseURL"])
	}
	models := prov["models"].(map[string]any)
	mm, ok := models["my-model"].(map[string]any)
	if !ok {
		t.Fatalf("expected key 'my-model' in models, got %v", models)
	}
	ctxVal := mm["limit"].(map[string]any)["context"]
	if ctxVal != 8192 {
		t.Errorf("limit.context = %v (type %T), want int 8192", ctxVal, ctxVal)
	}
}

func TestExpandTemplate_UnknownVar(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: x
model: {path: ` + model + `, alias: x, context: 1024}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template:
    weird: ${NONEXISTENT}
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ExpandTemplate(ExpandContext{})
	if err == nil {
		t.Fatal("expected error for unknown var")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT") {
		t.Errorf("err = %v, want mention of NONEXISTENT", err)
	}
}

func TestExpandTemplate_SubstringStaysString(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: x
model: {path: ` + model + `, alias: m, context: 1024}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template:
    model_id: "vibe-local/${MODEL_ALIAS}"
    context_note: "context=${MODEL_CONTEXT} tokens"
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.ExpandTemplate(ExpandContext{ModelAlias: "m", ModelContext: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if out["model_id"] != "vibe-local/m" {
		t.Errorf("model_id = %v", out["model_id"])
	}
	if out["context_note"] != "context=1024 tokens" {
		t.Errorf("context_note = %v", out["context_note"])
	}
}
