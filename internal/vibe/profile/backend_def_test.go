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

// chat_template_file is expanded in Backend.normalize() like the other backend
// path fields, so a referenced backend must get the same treatment as an
// inline one — validation stats the path and would fail on a literal "~".
func TestLoadBackend_ChatTemplateFileTildeExpanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmpl := filepath.Join(home, "templates", "qwen3-coder-tools.jinja")
	if err := os.MkdirAll(filepath.Dir(tmpl), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpl, []byte("{{ messages }}"), 0644); err != nil {
		t.Fatal(err)
	}
	writeBackend(t, "qwen", `
name: qwen
backend:
  llama_server:
    path: ~/m.gguf
    huggingface: {repo: a/b, file: m.gguf}
    chat_template_file: ~/templates/qwen3-coder-tools.jinja
    jinja: true
    alias: q
    context: 1024
`)
	p, err := Load(writeProfile(t, `
name: coder
backend_ref: qwen
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := p.Backend.LlamaServer.ChatTemplateFile; got != tmpl {
		t.Errorf("chat_template_file = %q, want %q", got, tmpl)
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

	// http_server is the one kind the router can never serve.
	_, err := Load(writeProfile(t, `
name: h
backend:
  external: true
  http_server: {image: some/image, port: 8000}
`))
	if err == nil || !strings.Contains(err.Error(), "not valid for http_server") {
		t.Fatalf("external http_server: got %v, want rejection", err)
	}

	// comfyui became a valid swap tenant (router-lifecycle §16): it rides
	// llama-swap's /upstream passthrough.
	pc, err := Load(writeProfile(t, `
name: c
backend:
  external: true
  comfyui: {dir: `+comfy+`}
`))
	if err != nil {
		t.Fatalf("external comfyui: %v", err)
	}
	if !pc.Backend.External {
		t.Error("comfyui external flag not retained")
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

func TestLoadBackend_LifecycleAndRouter(t *testing.T) {
	writeBackend(t, "qwen", `
name: qwen
backend:
  external: true
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
lifecycle:
  ttl: 45m
  preload: true
  evict_cost: 3
  refresh: nightly_if_idle
  start_timeout: 15m
router:
  aliases: [coder-max, coder]
  alias_owner: true
  unlisted: true
`)
	def, err := LoadBackend("qwen")
	if err != nil {
		t.Fatalf("LoadBackend: %v", err)
	}
	lc := def.Lifecycle
	if lc == nil {
		t.Fatal("lifecycle block not stored")
	}
	if lc.TTL == nil || lc.TTL.Seconds() != 2700 {
		t.Errorf("ttl = %v, want 45m (2700s)", lc.TTL)
	}
	if !lc.Preload {
		t.Error("preload not stored")
	}
	if lc.EvictCost != 3 {
		t.Errorf("evict_cost = %d, want 3", lc.EvictCost)
	}
	if lc.Refresh != RefreshNightlyIfIdle {
		t.Errorf("refresh = %q", lc.Refresh)
	}
	if lc.StartTimeout == nil || lc.StartTimeout.Seconds() != 900 {
		t.Errorf("start_timeout = %v, want 15m (900s)", lc.StartTimeout)
	}
	r := def.Router
	if r == nil {
		t.Fatal("router block not stored")
	}
	if len(r.Aliases) != 2 || r.Aliases[0] != "coder-max" || r.Aliases[1] != "coder" {
		t.Errorf("aliases = %v", r.Aliases)
	}
	if !r.AliasOwner || !r.Unlisted {
		t.Errorf("alias_owner/unlisted not stored: %+v", r)
	}
}

func TestLoadBackend_LifecycleValidation(t *testing.T) {
	base := `
name: qwen
backend:
  external: true
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
lifecycle:
`
	cases := map[string]struct{ yaml, wantErr string }{
		"unknown refresh policy": {base + "  refresh: hourly\n", "refresh"},
		"negative ttl":           {base + "  ttl: -5m\n", "negative"},
		"negative evict_cost":    {base + "  evict_cost: -1\n", "evict_cost"},
		"non-duration ttl":       {base + "  ttl: soon\n", "duration"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			writeBackend(t, "qwen", tc.yaml)
			if _, err := LoadBackend("qwen"); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadBackend_CloudPeer(t *testing.T) {
	writeBackend(t, "anthropic", `
name: anthropic
backend:
  cloud_peer:
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
    models: [claude-opus-4-8, claude-sonnet-5]
    formats: [anthropic, openai]
`)
	def, err := LoadBackend("anthropic")
	if err != nil {
		t.Fatalf("LoadBackend: %v", err)
	}
	if !def.Backend.External {
		t.Error("cloud_peer must imply backend.external")
	}
	cp := def.Backend.CloudPeer
	if cp == nil || cp.BaseURL != "https://api.anthropic.com" || cp.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("cloud_peer = %+v", cp)
	}
	if len(cp.Models) != 2 {
		t.Errorf("models = %v", cp.Models)
	}
}

func TestLoadBackend_CloudPeerValidation(t *testing.T) {
	cases := map[string]struct{ yaml, wantErr string }{
		"missing base_url": {`
name: p
backend:
  cloud_peer: {models: [m]}
`, "base_url"},
		"non-http base_url": {`
name: p
backend:
  cloud_peer: {base_url: api.anthropic.com, models: [m]}
`, "http"},
		"empty models": {`
name: p
backend:
  cloud_peer: {base_url: https://x.example}
`, "models"},
		"bad api_key_env": {`
name: p
backend:
  cloud_peer: {base_url: https://x.example, models: [m], api_key_env: "1BAD KEY"}
`, "api_key_env"},
		"unknown format": {`
name: p
backend:
  cloud_peer: {base_url: https://x.example, models: [m], formats: [grpc]}
`, "formats"},
		// cloud_peer implies external; a service sidecar contradiction must
		// trip the same guard as an explicit external: true.
		"service mode": {`
name: p
mode: service
backend:
  cloud_peer: {base_url: https://x.example, models: [m]}
`, "service"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			writeBackend(t, "p", tc.yaml)
			if _, err := LoadBackend("p"); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoad_CloudPeerProfile(t *testing.T) {
	writeBackend(t, "anthropic", `
name: anthropic
backend:
  cloud_peer: {base_url: https://api.anthropic.com, models: [claude-sonnet-5]}
`)
	p, err := Load(writeProfile(t, "name: claude\nbackend_ref: anthropic\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !p.Backend.External || p.Backend.CloudPeer == nil {
		t.Errorf("cloud_peer profile: external=%v cloud_peer=%v", p.Backend.External, p.Backend.CloudPeer)
	}

	// A frontend against a peer is the point of a peer: the router already
	// serves the model, and the profile is what aims a harness at it.
	fp, err := Load(writeProfile(t, `
name: claude
backend:
  cloud_peer: {base_url: https://api.anthropic.com, models: [claude-sonnet-5], context: 1000000}
frontend: {kind: external, write_file: /tmp/x, template: {a: 1}}
`))
	if err != nil {
		t.Fatalf("frontend on cloud_peer: %v", err)
	}
	if got := fp.Backend.CloudPeer.Context; got != 1000000 {
		t.Errorf("cloud_peer.context = %d, want 1000000", got)
	}

	// The frontend still has to be a valid frontend — accepting the pairing
	// must not skip the per-kind shape rules.
	_, err = Load(writeProfile(t, `
name: claude-bad
backend:
  cloud_peer: {base_url: https://api.anthropic.com, models: [claude-sonnet-5]}
frontend: {kind: external}
`))
	if err == nil || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("external frontend without write_file: got %v, want a write_file error", err)
	}
}

// cell: names the fleet cell a def renders onto (fleet-control C2 §6). The
// loader accepts any name — even one hosts.yaml doesn't know — because defs
// also load standalone for daemon activation; only the router render
// validates the name against fleet membership.
// cell: set ⇒ os.Stat validation is gated OFF (fleet.md §4.2's host:
// rule): a fleet-scoped def's paths exist on its cell, not necessarily
// on the box loading it. Without cell:, a missing path still fails.
func TestLoadBackend_CellScopedSkipsPathStat(t *testing.T) {
	t.Run("missing path passes with cell", func(t *testing.T) {
		writeBackend(t, "qwen", `
name: qwen
cell: gpu-cellar
backend:
  llama_server: {path: /nonexistent/on/this/box/m.gguf, alias: q, context: 1024}
`)
		if _, err := LoadBackend("qwen"); err != nil {
			t.Fatalf("cell-scoped def with absent path must load (validated on its cell): %v", err)
		}
	})
	t.Run("missing path fails without cell", func(t *testing.T) {
		writeBackend(t, "qwen2", `
name: qwen2
backend:
  llama_server: {path: /nonexistent/on/this/box/m.gguf, alias: q, context: 1024}
`)
		if _, err := LoadBackend("qwen2"); err == nil {
			t.Fatal("unscoped def with absent path must fail validation")
		}
	})
	t.Run("non-path rules still apply with cell", func(t *testing.T) {
		writeBackend(t, "qwen3", `
name: qwen3
cell: gpu-cellar
backend:
  llama_server: {path: /nonexistent/m.gguf, alias: "", context: 1024}
`)
		if _, err := LoadBackend("qwen3"); err == nil {
			t.Fatal("cell-scoped def still validates non-path rules (alias required)")
		}
	})
}

func TestLoadBackend_CellAcceptedWithoutValidation(t *testing.T) {
	writeBackend(t, "qwen", `
name: qwen
cell: gpu-cellar
backend:
  llama_server: {path: ~/m.gguf, huggingface: {repo: a/b, file: m.gguf}, alias: q, context: 1024}
`)
	def, err := LoadBackend("qwen")
	if err != nil {
		t.Fatalf("LoadBackend: %v", err)
	}
	if def.Cell != "gpu-cellar" {
		t.Errorf("cell = %q, want gpu-cellar", def.Cell)
	}
}
