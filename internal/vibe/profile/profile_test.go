package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubModelFile creates a placeholder file so backend.llama_server.path
// existence checks pass.
func stubModelFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubComfyDir creates a directory containing a main.py so backend.comfyui
// validation passes.
func stubComfyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("# fake\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// stubManagedBinary writes an executable shell script so
// frontend.binary validation passes. Returns its absolute path.
func stubManagedBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run-server.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubManagedBinaryNotExec writes a regular file *without* the executable
// bit set. Used to assert validation rejects non-executable binaries.
func stubManagedBinaryNotExec(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run-server.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: code
description: test
backend:
  llama_server:
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
	if p.Backend.LlamaServer == nil {
		t.Fatalf("backend.llama_server is nil")
	}
	m := p.Backend.LlamaServer
	if m.Context != 190000 {
		t.Errorf("context = %d", m.Context)
	}
	if !m.FlashAttn {
		t.Errorf("flash_attn = false")
	}
	if len(m.ExtraArgs) != 1 || m.ExtraArgs[0] != "--no-mmap" {
		t.Errorf("extra_args = %v", m.ExtraArgs)
	}
}

func TestLoad_ComfyUI_Valid(t *testing.T) {
	dir := stubComfyDir(t)
	yaml := `
name: imagegen
description: ComfyUI for image generation
backend:
  comfyui:
    dir: ` + dir + `
    port: 8188
estimated_vram_gb: 12
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend.ComfyUI == nil {
		t.Fatalf("backend.comfyui is nil")
	}
	if p.Backend.ComfyUI.Dir != dir {
		t.Errorf("dir = %q, want %q", p.Backend.ComfyUI.Dir, dir)
	}
	if p.Backend.ComfyUI.Port != 8188 {
		t.Errorf("port = %d, want 8188", p.Backend.ComfyUI.Port)
	}
	if p.Backend.LlamaServer != nil {
		t.Errorf("llama_server should be nil")
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: code
backend:
  llama_server:
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

func TestLoad_RejectsOldModelShape(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: code
model:
  path: ` + model + `
  alias: x
  context: 1024
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x.json
  template: {a: 1}
`
	_, err := Load(writeProfile(t, yaml))
	if err == nil {
		t.Fatal("expected error for old `model:` shape")
	}
	if !strings.Contains(err.Error(), "schema changed") {
		t.Errorf("err = %v, want schema-changed migration hint", err)
	}
	if !strings.Contains(err.Error(), "backend.llama_server") {
		t.Errorf("err = %v, want pointer to backend.llama_server", err)
	}
	if !strings.Contains(err.Error(), "profiles/code.example.yaml") {
		t.Errorf("err = %v, want pointer to example file", err)
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
backend:
  llama_server:
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
	if p.Backend.LlamaServer.Path != modelPath {
		t.Errorf("backend.llama_server.path = %q, want %q", p.Backend.LlamaServer.Path, modelPath)
	}
	if !strings.HasPrefix(p.Frontend.WriteFile, home) {
		t.Errorf("write_file = %q, want home prefix", p.Frontend.WriteFile)
	}
}

// TestLoad_ComposeFileTildeExpansion guards the docker-compose path field.
// Skipped this once and the daemon's stat() failed at activation time with
// a literal "~" in the path; if it regresses, this test catches it before
// the user sees it.
func TestLoad_ComposeFileTildeExpansion(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir, err := os.MkdirTemp(home, "vibe-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	modelPath := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(modelPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(composePath, []byte("services: {x: {image: nginx}}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(home, modelPath)
	composeRel, _ := filepath.Rel(home, composePath)

	yaml := `
name: x
backend:
  llama_server:
    path: ~/` + rel + `
    alias: x
    context: 1024
frontend:
  kind: docker-compose
  app: open-webui
  compose_file: ~/` + composeRel + `
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Frontend.ComposeFile != composePath {
		t.Errorf("compose_file = %q, want %q", p.Frontend.ComposeFile, composePath)
	}
}

func TestValidate(t *testing.T) {
	model := stubModelFile(t)
	comfyDir := stubComfyDir(t)
	emptyDir := t.TempDir() // no main.py
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing name",
			yaml: `
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "name is required",
		},
		{
			name: "bad name",
			yaml: `
name: "has spaces"
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "must match",
		},
		{
			name: "missing backend entirely",
			yaml: `
name: x
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "backend is required",
		},
		{
			name: "both backends set",
			yaml: `
name: x
backend:
  llama_server: {path: ` + model + `, alias: x, context: 1024}
  comfyui: {dir: ` + comfyDir + `}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "only one of",
		},
		{
			name: "missing alias",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, context: 1024}}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "backend.llama_server.alias is required",
		},
		{
			name: "zero context",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 0}}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "backend.llama_server.context must be > 0",
		},
		{
			name: "model file missing",
			yaml: `
name: x
backend: {llama_server: {path: /nonexistent/model.gguf, alias: x, context: 1024}}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "backend.llama_server.path",
		},
		{
			name: "comfyui dir missing main.py",
			yaml: `
name: x
backend: {comfyui: {dir: ` + emptyDir + `}}
`,
			wantErr: "missing main.py",
		},
		{
			name: "comfyui dir does not exist",
			yaml: `
name: x
backend: {comfyui: {dir: /this/path/does/not/exist/anywhere}}
`,
			wantErr: "backend.comfyui.dir",
		},
		{
			name: "comfyui with frontend block",
			yaml: `
name: x
backend: {comfyui: {dir: ` + comfyDir + `}}
frontend: {kind: external, app: opencode, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "frontend is not supported for comfyui",
		},
		{
			name: "unknown kind",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: weird, app: x, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "is unknown",
		},
		{
			name: "missing kind",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {app: x, write_file: /tmp/x, template: {a: 1}}
`,
			wantErr: "frontend.kind is required",
		},
		{
			name: "external missing write_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: external, app: opencode, template: {a: 1}}
`,
			wantErr: "write_file is required",
		},
		{
			name: "external missing template",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: external, app: opencode, write_file: /tmp/x}
`,
			wantErr: "template is required",
		},
		{
			name: "managed missing binary",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: managed, app: x}
`,
			wantErr: "binary is required",
		},
		{
			name: "managed binary missing on disk",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: managed, app: x, binary: /nonexistent/run-server.sh}
`,
			wantErr: "frontend.binary",
		},
		{
			name: "managed binary not executable",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: managed, app: x, binary: ` + stubManagedBinaryNotExec(t) + `}
`,
			wantErr: "not executable",
		},
		{
			name: "managed rejects template without write_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: managed
  app: x
  binary: ` + stubManagedBinary(t) + `
  template: {a: 1}
`,
			wantErr: "template requires frontend.write_file",
		},
		{
			name: "managed rejects mcps without write_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: managed
  app: x
  binary: ` + stubManagedBinary(t) + `
  mcps: [datadog]
`,
			wantErr: "mcps requires frontend.write_file",
		},
		{
			name: "managed rejects compose_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: managed
  app: x
  binary: ` + stubManagedBinary(t) + `
  compose_file: /tmp/dc.yaml
`,
			wantErr: "compose_file is only valid for kind=docker-compose",
		},
		{
			name: "managed rejects project_name",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: managed
  app: x
  binary: ` + stubManagedBinary(t) + `
  project_name: vibe-x
`,
			wantErr: "project_name is only valid for kind=docker-compose",
		},
		{
			name: "managed rejects services",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: managed
  app: x
  binary: ` + stubManagedBinary(t) + `
  services: [web]
`,
			wantErr: "services is only valid for kind=docker-compose",
		},
		{
			name: "managed wait_for missing url",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: managed
  app: x
  binary: ` + stubManagedBinary(t) + `
  wait_for:
    - timeout: 5s
`,
			wantErr: "wait_for[0].url is required",
		},
		{
			name: "external rejects binary",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template: {a: 1}
  binary: /usr/local/bin/foo
`,
			wantErr: "binary is only valid for kind=managed",
		},
		{
			name: "external rejects args",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template: {a: 1}
  args: [--foo]
`,
			wantErr: "args is only valid for kind=managed",
		},
		{
			name: "external rejects workdir",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template: {a: 1}
  workdir: /tmp
`,
			wantErr: "workdir is only valid for kind=managed",
		},
		{
			name: "docker-compose rejects binary",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  binary: /usr/local/bin/foo
`,
			wantErr: "binary is only valid for kind=managed",
		},
		{
			name: "docker-compose rejects args",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  args: [--foo]
`,
			wantErr: "args is only valid for kind=managed",
		},
		{
			name: "docker-compose rejects workdir",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  workdir: /tmp
`,
			wantErr: "workdir is only valid for kind=managed",
		},
		{
			name: "docker-compose missing compose_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend: {kind: docker-compose, app: x}
`,
			wantErr: "compose_file is required",
		},
		{
			name: "docker-compose rejects write_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  write_file: /tmp/x.json
`,
			wantErr: "write_file is only valid for kind=external",
		},
		{
			name: "docker-compose rejects template",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  template: {a: 1}
`,
			wantErr: "template is only valid for kind=external",
		},
		{
			name: "docker-compose rejects mcps",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  mcps: [datadog]
`,
			wantErr: "mcps is only valid for kind=external",
		},
		{
			name: "docker-compose duplicate service",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  services: [a, b, a]
`,
			wantErr: `duplicate "a"`,
		},
		{
			name: "docker-compose wait_for missing url",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  wait_for:
    - timeout: 5s
`,
			wantErr: "wait_for[0].url is required",
		},
		{
			name: "docker-compose bad timeout",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: docker-compose
  app: x
  compose_file: /tmp/dc.yaml
  wait_for:
    - {url: "http://x/", timeout: nope}
`,
			wantErr: "wait_for.timeout",
		},
		{
			name: "external rejects compose_file",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
frontend:
  kind: external
  app: opencode
  write_file: /tmp/x
  template: {a: 1}
  compose_file: /tmp/dc.yaml
`,
			wantErr: "compose_file is only valid for kind=docker-compose",
		},
		{
			name: "duplicate mcp names",
			yaml: `
name: x
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
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

func TestLoad_DockerCompose_Valid(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: perplexica
description: research stack
backend:
  llama_server:
    path: ` + model + `
    alias: m
    context: 1024
frontend:
  kind: docker-compose
  app: perplexica
  compose_file: /tmp/dc.yaml
  project_name: vibe-perplexica
  services: [searxng, perplexica]
  wait_for:
    - url: http://127.0.0.1:3001/
      timeout: 60s
    - url: http://127.0.0.1:8080/
  env:
    OLLAMA_API_URL: ${VIBE_API}
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Frontend.Kind != FrontendDockerCompose {
		t.Errorf("kind = %q", p.Frontend.Kind)
	}
	if p.Frontend.ComposeFile != "/tmp/dc.yaml" {
		t.Errorf("compose_file = %q", p.Frontend.ComposeFile)
	}
	if p.Frontend.ProjectName != "vibe-perplexica" {
		t.Errorf("project_name = %q", p.Frontend.ProjectName)
	}
	if len(p.Frontend.Services) != 2 || p.Frontend.Services[0] != "searxng" {
		t.Errorf("services = %v", p.Frontend.Services)
	}
	if len(p.Frontend.WaitFor) != 2 {
		t.Fatalf("wait_for = %v", p.Frontend.WaitFor)
	}
	if p.Frontend.WaitFor[0].Timeout != 60*time.Second {
		t.Errorf("wait_for[0].timeout = %v, want 60s", p.Frontend.WaitFor[0].Timeout)
	}
	if p.Frontend.WaitFor[1].Timeout != 0 {
		t.Errorf("wait_for[1].timeout = %v, want 0 (unset)", p.Frontend.WaitFor[1].Timeout)
	}
	if p.Frontend.Env["OLLAMA_API_URL"] != "${VIBE_API}" {
		t.Errorf("env OLLAMA_API_URL = %q (should be the literal placeholder pre-expansion)", p.Frontend.Env["OLLAMA_API_URL"])
	}
}

func TestLoad_Managed_Valid(t *testing.T) {
	model := stubModelFile(t)
	bin := stubManagedBinary(t)
	workdir := filepath.Dir(bin)
	yaml := `
name: openwebui
description: open-webui under vibe supervision
backend:
  llama_server:
    path: ` + model + `
    alias: m
    context: 1024
frontend:
  kind: managed
  app: open-webui
  binary: ` + bin + `
  args:
    - "--listen"
    - "127.0.0.1"
    - "--port"
    - "8080"
  workdir: ` + workdir + `
  env:
    OPEN_WEBUI_DATA: /var/lib/open-webui
  wait_for:
    - url: http://127.0.0.1:8080/
      timeout: 30s
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Frontend.Kind != FrontendManaged {
		t.Errorf("kind = %q", p.Frontend.Kind)
	}
	if p.Frontend.Binary != bin {
		t.Errorf("binary = %q, want %q", p.Frontend.Binary, bin)
	}
	wantArgs := []string{"--listen", "127.0.0.1", "--port", "8080"}
	if len(p.Frontend.Args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", p.Frontend.Args, wantArgs)
	}
	for i, a := range wantArgs {
		if p.Frontend.Args[i] != a {
			t.Errorf("args[%d] = %q, want %q", i, p.Frontend.Args[i], a)
		}
	}
	if p.Frontend.Workdir != workdir {
		t.Errorf("workdir = %q, want %q", p.Frontend.Workdir, workdir)
	}
	if p.Frontend.Env["OPEN_WEBUI_DATA"] != "/var/lib/open-webui" {
		t.Errorf("env OPEN_WEBUI_DATA = %q", p.Frontend.Env["OPEN_WEBUI_DATA"])
	}
	if len(p.Frontend.WaitFor) != 1 {
		t.Fatalf("wait_for = %v", p.Frontend.WaitFor)
	}
	if p.Frontend.WaitFor[0].URL != "http://127.0.0.1:8080/" {
		t.Errorf("wait_for[0].url = %q", p.Frontend.WaitFor[0].URL)
	}
	if p.Frontend.WaitFor[0].Timeout != 30*time.Second {
		t.Errorf("wait_for[0].timeout = %v, want 30s", p.Frontend.WaitFor[0].Timeout)
	}
}

// TestLoad_Managed_WithWriteFile covers the opencode-style profile: managed
// kind that also renders a config file the binary then reads via env. The
// validator must accept write_file/template/mcps when paired with a binary.
func TestLoad_Managed_WithWriteFile(t *testing.T) {
	model := stubModelFile(t)
	bin := stubManagedBinary(t)
	yaml := `
name: code
backend:
  llama_server:
    path: ` + model + `
    alias: m
    context: 1024
frontend:
  kind: managed
  app: opencode
  binary: ` + bin + `
  write_file: /tmp/opencode.json
  template:
    model: llama-local/m
  env:
    OPENCODE_CONFIG: ${WRITE_FILE}
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if p.Frontend.Kind != FrontendManaged {
		t.Errorf("kind = %q", p.Frontend.Kind)
	}
	if p.Frontend.WriteFile != "/tmp/opencode.json" {
		t.Errorf("write_file = %q", p.Frontend.WriteFile)
	}
	if _, ok := p.Frontend.Template["model"]; !ok {
		t.Errorf("template missing model key: %v", p.Frontend.Template)
	}
}

func TestExpandTemplate_PreservesIntType(t *testing.T) {
	model := stubModelFile(t)
	yaml := `
name: x
backend: {llama_server: {path: ` + model + `, alias: my-model, context: 8192}}
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
backend: {llama_server: {path: ` + model + `, alias: x, context: 1024}}
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
backend: {llama_server: {path: ` + model + `, alias: m, context: 1024}}
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
