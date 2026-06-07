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
