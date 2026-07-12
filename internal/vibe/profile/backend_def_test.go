package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBackend writes a backend def under $XDG_CONFIG_HOME/vibe/backends and
// returns the config home it set, so the test's profiles resolve refs against it.
func writeBackend(t *testing.T, name, yaml string) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dir := filepath.Join(cfg, "vibe", "backends")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLoadBackend_AndProfileRef(t *testing.T) {
	writeBackend(t, "qwen", `
name: qwen
backend:
  llama_server:
    path: ~/models/qwen.gguf
    huggingface: {repo: unsloth/Qwen3.6, file: qwen.gguf}
    alias: qwen3.6
    context: 131072
estimated_vram_gb: 22
`)
	// A profile that references the backend instead of inlining it.
	prof := `
name: coder
backend_ref: qwen
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`
	p, err := Load(writeProfile(t, prof))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Backend.LlamaServer == nil {
		t.Fatal("backend_ref did not resolve into an llama_server backend")
	}
	if got := p.Backend.LlamaServer.Alias; got != "qwen3.6" {
		t.Errorf("alias = %q, want qwen3.6", got)
	}
	if !strings.HasSuffix(p.Backend.LlamaServer.Path, "/models/qwen.gguf") || strings.HasPrefix(p.Backend.LlamaServer.Path, "~") {
		t.Errorf("path not tilde-expanded after ref resolution: %q", p.Backend.LlamaServer.Path)
	}
	if p.EstimatedVRAMGB != 22 {
		t.Errorf("estimated_vram_gb = %v, want 22 (inherited from backend)", p.EstimatedVRAMGB)
	}
}

func TestBackendRef_ProfileOverridesVRAM(t *testing.T) {
	writeBackend(t, "qwen", `
name: qwen
backend:
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
estimated_vram_gb: 22
`)
	p, err := Load(writeProfile(t, `
name: c
backend_ref: qwen
estimated_vram_gb: 30
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.EstimatedVRAMGB != 30 {
		t.Errorf("profile estimated_vram_gb should win: got %v, want 30", p.EstimatedVRAMGB)
	}
}

func TestBackendRef_MutuallyExclusiveWithInline(t *testing.T) {
	writeBackend(t, "qwen", "name: qwen\nbackend:\n  llama_server: {path: ~/m.gguf, alias: q, context: 1024}\n")
	_, err := Load(writeProfile(t, `
name: c
backend_ref: qwen
backend:
  llama_server: {path: ~/m.gguf, alias: q, context: 1024}
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestBackendRef_Missing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := Load(writeProfile(t, `
name: c
backend_ref: nonexistent
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err == nil || !strings.Contains(err.Error(), "backend_ref") {
		t.Fatalf("expected backend_ref resolution error, got %v", err)
	}
}

// TestLoadBackend_External pins the external-backend flag: it must load from
// a backend def and survive backend_ref resolution into the profile, because
// the daemon decides launch-vs-router-check off the resolved p.Backend.
func TestLoadBackend_External(t *testing.T) {
	writeBackend(t, "qwen", `
name: qwen
backend:
  external: true
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
`)
	def, err := LoadBackend("qwen")
	if err != nil {
		t.Fatalf("LoadBackend: %v", err)
	}
	if !def.Backend.External {
		t.Error("Backend.External = false, want true")
	}

	p, err := Load(writeProfile(t, `
name: c
backend_ref: qwen
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err != nil {
		t.Fatalf("Load via backend_ref: %v", err)
	}
	if !p.Backend.External {
		t.Error("profile did not inherit external: true from the referenced backend")
	}
}

// mode: service means "vibe supervises this sidecar process"; external means
// "vibe supervises nothing". The contradiction must be rejected at both load
// surfaces (backend def and profile), since backend defs are also activated
// directly without Profile.Validate.
func TestLoadBackend_External_RejectsServiceMode(t *testing.T) {
	writeBackend(t, "emb", `
name: emb
mode: service
backend:
  external: true
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
`)
	if _, err := LoadBackend("emb"); err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("service+external backend def: got %v, want service/external contradiction error", err)
	}
}

func TestLoad_External_RejectsServiceModeProfile(t *testing.T) {
	_, err := Load(writeProfile(t, `
name: s
mode: service
backend:
  external: true
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
`))
	if err == nil || !strings.Contains(err.Error(), "external backend") {
		t.Fatalf("service-mode profile with external backend: got %v, want rejection", err)
	}
}

// external is only meaningful where the router serves the backend's OpenAI
// surface; comfyui and http_server must reject it.
func TestBackend_External_OnlyLLMServingKinds(t *testing.T) {
	comfy := stubComfyDir(t)
	cases := map[string]string{
		"comfyui": `
name: c
backend:
  external: true
  comfyui: {dir: ` + comfy + `}
`,
		"http_server": `
name: h
backend:
  external: true
  http_server: {image: some/image, port: 8000}
`,
	}
	for kind, yaml := range cases {
		t.Run(kind, func(t *testing.T) {
			_, err := Load(writeProfile(t, yaml))
			if err == nil || !strings.Contains(err.Error(), "backend.external is only valid") {
				t.Fatalf("external %s: got %v, want LLM-serving-kinds rejection", kind, err)
			}
		})
	}

	// tabby_api is LLM-serving and must accept the flag.
	p, err := Load(writeProfile(t, `
name: ok
backend:
  external: true
  tabby_api: {model_dir: /models/q, alias: q, context: 1024, port: 5000, venv: /venv, repo: /repo}
`))
	if err != nil {
		t.Fatalf("external tabby_api: %v", err)
	}
	if !p.Backend.External {
		t.Error("tabby_api external flag not retained")
	}
}

// backend: {external: true} next to backend_ref must trip the
// mutual-exclusion error rather than being silently overwritten when the
// ref resolves.
func TestBackendRef_ExternalOnlyInlineBlockIsExclusive(t *testing.T) {
	writeBackend(t, "qwen", "name: qwen\nbackend:\n  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}\n")
	_, err := Load(writeProfile(t, `
name: c
backend_ref: qwen
backend: {external: true}
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("backend_ref + inline external flag: got %v, want mutual-exclusion error", err)
	}
}

func TestLoadBackend_NameValidation(t *testing.T) {
	writeBackend(t, "qwen", "name: qwen\nbackend:\n  llama_server: {path: ~/m.gguf, alias: q, context: 1024}\n")

	if _, err := LoadBackend("../profiles/qwen"); err == nil || !strings.Contains(err.Error(), "path separators") {
		t.Errorf("path-shaped name: got %v, want bare-file-stem error", err)
	}
	if _, err := LoadBackend("qwen.yaml"); err == nil || !strings.Contains(err.Error(), "extension") {
		t.Errorf("extension-suffixed name: got %v, want drop-the-extension error", err)
	}
	// Dots in the stem are legal — real backends are named like qwen3.6-27b.
	if _, err := LoadBackend("qwen3.6-27b"); err == nil || strings.Contains(err.Error(), "extension") {
		t.Errorf("dotted stem must pass name validation (fail only on open): got %v", err)
	}
}

func TestLoadBackend_NameFieldMustMatchFilename(t *testing.T) {
	writeBackend(t, "qwen", "name: other\nbackend:\n  llama_server: {path: ~/m.gguf, alias: q, context: 1024}\n")
	if _, err := LoadBackend("qwen"); err == nil || !strings.Contains(err.Error(), "does not match filename") {
		t.Fatalf("mismatched name: got %v, want mismatch error", err)
	}
}
