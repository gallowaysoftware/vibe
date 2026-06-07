// Package profile defines the vibe profile YAML schema and its loader.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Profile is the top-level YAML shape on disk.
//
// Backend is a discriminated union: exactly one of its typed sub-blocks
// (LlamaServer, ComfyUI) must be set. The presence of the sub-block is the
// discriminator — no separate `kind` field is needed.
//
// Frontend is optional: ComfyUI ships its own UI, so ComfyUI-backed profiles
// leave it unset. LlamaServer-backed profiles still require a frontend block
// (external or docker-compose).
type Profile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Backend is the inline model-server spec. Mutually exclusive with
	// BackendRef: a profile either inlines its backend here or references a
	// shared one by name. After Load, Backend is always populated (resolved
	// from the ref when BackendRef is set).
	Backend Backend `yaml:"backend,omitempty"`
	// BackendRef names a reusable backend defined under
	// $XDG_CONFIG_HOME/vibe/backends/<ref>.yaml. Lets many frontends share one
	// model spec and lets vamp capabilities target a backend (not a profile).
	BackendRef      string   `yaml:"backend_ref,omitempty"`
	Frontend        Frontend `yaml:"frontend,omitempty"`
	EstimatedVRAMGB float64  `yaml:"estimated_vram_gb,omitempty"`
	// Mode controls whether this profile takes the daemon's single
	// "active" slot or runs as a background service alongside other
	// services and an active profile. Default (empty / "active") is
	// the historical single-active-profile behavior — backward compat
	// for every profile that existed before mode was introduced.
	//
	// "service" mode is for stateless sidecars (searxng, embedding
	// servers, TTS engines, image-gen daemons) that pipelines call by
	// well-known port. Multiple service-mode profiles can run
	// concurrently with each other and with one active-mode profile.
	// They bypass the proxy (callers use the service's published
	// port directly) and the frontend activation path.
	Mode string `yaml:"mode,omitempty"`
}

// Profile modes. Default-empty maps to ModeActive so existing
// profiles keep their behavior without YAML edits.
const (
	ModeActive  = "active"
	ModeService = "service"
)

// ResolvedMode returns Mode with the historical default ("active")
// substituted for empty. Centralised so every caller agrees on the
// default.
func (p *Profile) ResolvedMode() string {
	if p.Mode == "" {
		return ModeActive
	}
	return p.Mode
}

// Backend is a discriminated union of supported local-AI backends. Exactly
// one sub-block must be non-nil; loaders reject zero-or-both.
type Backend struct {
	LlamaServer *LlamaServerBackend `yaml:"llama_server,omitempty"`
	ComfyUI     *ComfyUIBackend     `yaml:"comfyui,omitempty"`
	HTTPServer  *HTTPServerBackend  `yaml:"http_server,omitempty"`
	TabbyAPI    *TabbyAPIBackend    `yaml:"tabby_api,omitempty"`
}

// LlamaServerBackend supervises a llama-server child process exposing an
// OpenAI-compatible API. This was the only backend in Phase 1 and used to
// live under the top-level `model:` key.
type LlamaServerBackend struct {
	Path        string       `yaml:"path"`
	Huggingface *Huggingface `yaml:"huggingface,omitempty"`
	Alias       string       `yaml:"alias"`
	Context     int          `yaml:"context"`
	Parallel    int          `yaml:"parallel,omitempty"`
	GPULayers   int          `yaml:"gpu_layers,omitempty"`
	FlashAttn   bool         `yaml:"flash_attn,omitempty"`
	CacheTypeK  string       `yaml:"cache_type_k,omitempty"`
	CacheTypeV  string       `yaml:"cache_type_v,omitempty"`
	Jinja       bool         `yaml:"jinja,omitempty"`
	ExtraArgs   []string     `yaml:"extra_args,omitempty"`
	// Port pins the host port llama-server publishes on. When set,
	// the daemon uses this exact port instead of PickFreePort. Useful
	// for service-mode profiles that pipelines reach by a stable
	// well-known address — otherwise each daemon restart hands the
	// service a different ephemeral port. Zero / unset falls back to
	// the historical "pick a free port" behavior.
	Port int `yaml:"port,omitempty"`
	// MMProj is the multimodal projector GGUF that llama-server loads
	// via --mmproj to enable image input on vision-capable models
	// (Gemma 3, Qwen2.5-VL, LLaVA, etc.). The accompanying weights file
	// at Path must be a vision-capable model; loading a text-only model
	// with an mmproj is a configuration error llama-server will reject.
	// When Huggingface.MMProjFile is set, this path is the *target* for
	// the pulled file and may not yet exist.
	MMProj string `yaml:"mmproj,omitempty"`
	// DraftModel is a speculative-decoding draft model GGUF that
	// llama-server loads via --model-draft. Used for Gemma 4 MTP, whose
	// multi-token-prediction head ships as a separate ~0.4B "assistant"
	// drafter (unlike Qwen MTP, which is in-weights and needs no draft
	// file). When set, SpecType (default "draft-mtp") and SpecDraftNMax
	// (default 4) are passed too. When Huggingface.DraftFile is set, this
	// path is the *target* for the pulled file and may not yet exist.
	// NOTE: draft-mtp requires an f16 KV cache — a quantized cache_type_k/v
	// silently yields ~0% draft acceptance, so validation rejects that combo.
	DraftModel string `yaml:"draft_model,omitempty"`
	// SpecType selects the speculative-decoding strategy (--spec-type),
	// e.g. "draft-mtp" for Gemma 4 MTP. Defaults to "draft-mtp" when
	// DraftModel is set. Only meaningful alongside DraftModel.
	SpecType string `yaml:"spec_type,omitempty"`
	// SpecDraftNMax caps how many tokens the drafter proposes per step
	// (--spec-draft-n-max). Defaults to 4 when DraftModel is set; 2-4 is
	// the recommended range (higher reduces acceptance). Only meaningful
	// alongside DraftModel.
	SpecDraftNMax int `yaml:"spec_draft_n_max,omitempty"`
	// Binary overrides which llama-server binary to spawn for this
	// profile. Empty (default) means "use the daemon's configured
	// llama-server, which falls back to the first one on $PATH" — the
	// usual recipe (~/.local/bin/llama-server is a symlink at whichever
	// flavor `vibe doctor --install llama-cpp` planted). Set this when
	// you want one profile to pin a specific build (e.g. a feature
	// branch under ~/src/llama.cpp/build/bin/llama-server) without
	// affecting the others. Tilde-expanded at load; validated to exist
	// and be executable.
	Binary string `yaml:"binary,omitempty"`
}

// HTTPServerBackend supervises any HTTP-serving inference engine — typically
// shipped as a docker container, but the same backend can wrap a bare binary.
// The daemon spawns the configured process (or `docker run` invocation),
// polls HealthURL until ready, and proxies vamp's API traffic through the
// vibe proxy like any other backend. Designed for things vibe doesn't have
// a first-class backend type for: TTS engines (Kokoro-FastAPI), embedding
// servers, third-party inference daemons, etc.
//
// Two modes, mutually exclusive:
//
//   - **docker mode** (Image set): daemon shells out to `docker run --rm
//     --name <slug> -p <Port>:<ContainerPort> [...env, volumes, gpu...]
//     <Image>`. The supervisor inherits the docker client's lifecycle (SIGINT
//     to docker → graceful container stop).
//
//   - **binary mode** (Binary set): daemon execs Binary with Args directly,
//     identical lifecycle to llama_server/comfyui.
//
// Port is the host port the daemon publishes / the binary listens on; the
// proxy points at it.
type HTTPServerBackend struct {
	// Docker-mode fields. Image is the container reference (e.g.
	// "ghcr.io/remsky/kokoro-fastapi-gpu:latest"). When non-empty the
	// daemon launches via docker run.
	Image string `yaml:"image,omitempty"`
	// ContainerPort is the port exposed inside the container. Defaults
	// to Port when unset, which fits the common case where the container
	// listens on the same port number it's published as.
	ContainerPort int `yaml:"container_port,omitempty"`
	// Volumes are "host:container[:ro]" mappings passed to `docker run -v`.
	Volumes []string `yaml:"volumes,omitempty"`
	// GPU, when true, adds `--gpus all` to the docker invocation (NVIDIA
	// container toolkit must be installed). Required for VRAM-resident
	// engines like Kokoro-FastAPI's GPU build.
	GPU bool `yaml:"gpu,omitempty"`

	// Binary-mode field. Set instead of Image to wrap a bare process.
	Binary string `yaml:"binary,omitempty"`

	// Common fields.
	// Port is the host TCP port the daemon points its proxy at. Required.
	// Set to 0 to ask the daemon to pick a free port — only viable in
	// binary mode where Args can reference the port via a template
	// placeholder (NOT supported in this MVP; pin Port explicitly).
	Port int `yaml:"port"`
	// Args is the argv passed to Binary (binary mode) or appended after
	// the image name (docker mode — runs as the container's command
	// override). Empty in the common case where the image's default
	// CMD is what you want.
	Args []string `yaml:"args,omitempty"`
	// Env is forwarded as KEY=VALUE either to the binary's environment
	// (binary mode) or as `docker run -e KEY=VALUE` flags (docker mode).
	Env map[string]string `yaml:"env,omitempty"`
	// HealthPath is appended to http://127.0.0.1:Port to form the URL
	// the supervisor polls until the backend reports ready. Defaults
	// to "/health" when unset — common convention; override for
	// servers that don't expose /health (e.g. "/v1/audio/speech" for
	// Kokoro-FastAPI, which doesn't have a dedicated health endpoint
	// — a non-2xx response without a body is fine, it just proves the
	// process is up).
	HealthPath string `yaml:"health_path,omitempty"`
}

// TabbyAPIBackend supervises a tabbyAPI process serving an ExLlamaV3
// (EXL3-format) model on NVIDIA hardware. tabbyAPI exposes the same
// OpenAI-compatible /v1 surface as llama-server, so vamp text stages
// switch profiles without any pipeline code changes — only the
// profile YAML differs.
//
// **Why have this alongside llama_server**: EXL3 single-stream is
// 1.5-2x faster than equivalent GGUF on Ampere / Ada / Blackwell, and
// batch / parallel work (RAG enrichment, multi-segment TTS prompting)
// is noticeably more. The trade-off is VRAM-only — no CPU offload,
// no AMD/Apple — so this backend is for NVIDIA + "model fits in
// VRAM" setups. For text-only stages on those rigs, prefer this
// over llama_server.
//
// **Multimodal**: not supported on this backend. Vision-capable
// models stay on llama_server.
//
// Launch model: tabbyAPI is a Python project; we exec its start.py
// from a checkout (Repo) using a venv (Venv) that has the right
// exllamav3 + tabbyAPI installed. Daemon writes a temporary
// config.yml derived from this block before each launch so the
// per-profile knobs (model dir, port, cache mode) come through
// without the user maintaining a parallel config file.
type TabbyAPIBackend struct {
	// ModelDir is the path to the EXL3 model directory. Must contain
	// the safetensors shards + config.json + tokenizer files. Required.
	// Tilde-expanded at load.
	ModelDir string `yaml:"model_dir"`
	// Huggingface, when set, downloads the EXL3 model snapshot from
	// HF into ModelDir on first launch / `vibe pull`. Mirrors
	// LlamaServerBackend.Huggingface but pulls a directory not a
	// single file.
	Huggingface *HuggingfaceRepo `yaml:"huggingface,omitempty"`
	// Alias is the model id reported on /v1/models and what vamp's
	// OpenAI client uses as `model:` in completion requests. Required.
	Alias string `yaml:"alias"`
	// Context is the max sequence length tabbyAPI loads the model
	// with. Required.
	Context int `yaml:"context"`
	// Port pins the host port tabbyAPI publishes on. Required —
	// daemon-picked ports aren't supported here, matching the
	// http_server backend's discipline (keeps service-mode profiles
	// reachable at a stable address across daemon restarts).
	Port int `yaml:"port"`
	// CacheMode selects the KV cache quantisation tabbyAPI uses.
	// One of FP16 (default — highest quality, most VRAM), Q8, Q6, Q4.
	// Q4 roughly halves KV memory for ~negligible quality loss on
	// reasonable models; useful when context > 32k.
	CacheMode string `yaml:"cache_mode,omitempty"`
	// Venv is the python venv that has exllamav3 + tabbyAPI installed.
	// Daemon exec's <Venv>/bin/python directly so PATH / shell state
	// doesn't matter. Required. Tilde-expanded.
	Venv string `yaml:"venv"`
	// Repo is the tabbyAPI checkout used as workdir + entrypoint
	// source (we exec start.py from there). Required for now —
	// running the installed package via `python -m tabbyAPI` could
	// work but the upstream layout still prefers the checkout-with-
	// start.py shape. Tilde-expanded.
	Repo string `yaml:"repo"`
	// DraftModelDir, when set, points at a smaller EXL3 model dir
	// used for speculative decoding. Optional but a major throughput
	// win for long_form profiles when the draft fits alongside the
	// main model.
	DraftModelDir string `yaml:"draft_model_dir,omitempty"`
	// ExtraArgs are appended to the start.py argv after vibe's
	// standard ones. Use for tabbyAPI-specific knobs vibe doesn't
	// yet expose as first-class fields.
	ExtraArgs []string `yaml:"extra_args,omitempty"`
}

// HuggingfaceRepo names an HF model snapshot to download as a
// directory. Distinguishes from Huggingface (above), which names a
// single file inside a repo — EXL3 models are multi-file snapshots
// (safetensors shards + tokenizer + config + measurement.json), so
// the pull is a full snapshot download.
type HuggingfaceRepo struct {
	Repo     string `yaml:"repo"`
	Revision string `yaml:"revision,omitempty"`
}

// ComfyUIBackend supervises a ComfyUI python entrypoint. ComfyUI manages its
// own model assets (we don't pull weights for it) and serves its own UI, so
// no frontend block is allowed.
type ComfyUIBackend struct {
	Dir       string   `yaml:"dir"`              // ComfyUI checkout path; supports ~/  expansion
	Python    string   `yaml:"python,omitempty"` // python binary; default "python3"
	Listen    string   `yaml:"listen,omitempty"` // default "127.0.0.1"
	Port      int      `yaml:"port,omitempty"`   // default 8188; 0 picks random
	ExtraArgs []string `yaml:"extra_args,omitempty"`
}

// Huggingface points at a model file on huggingface.co. When set, vibe
// downloads the file to LlamaServerBackend.Path on demand (via `vibe pull` or
// implicitly at the start of `vibe start`).
//
// MMProjFile, when non-empty, names a second file from the same Repo/Revision
// (the multimodal projector for vision-capable models). It is downloaded to
// LlamaServerBackend.MMProj. Setting MMProjFile without MMProj — or vice
// versa, when MMProj points at a non-existent path with no HF spec — is a
// validation error.
type Huggingface struct {
	Repo       string `yaml:"repo"`
	File       string `yaml:"file"`
	Revision   string `yaml:"revision,omitempty"` // default "main"
	MMProjFile string `yaml:"mmproj_file,omitempty"`
	// DraftFile, when non-empty, names a speculative draft model file from
	// the same Repo/Revision (e.g. a Gemma 4 MTP assistant GGUF). It is
	// downloaded to LlamaServerBackend.DraftModel, mirroring MMProjFile.
	DraftFile string `yaml:"draft_file,omitempty"`
}

const (
	FrontendExternal      = "external"
	FrontendDockerCompose = "docker-compose"
	FrontendManaged       = "managed"
)

// BrowserURL returns the URL to suggest to a user once the frontend is
// up. Precedence:
//
//  1. The explicit Frontend.URL field, when set.
//  2. Otherwise, the wait_for entry whose URL ends in a root path
//     ("/" or no path) — the conventional "UI is on this URL"
//     marker that distinguishes the user-visible endpoint from
//     health-check URLs (`/health`, `/readyz`, etc.).
//
// Returns "" when neither produces an answer, signalling the CLI not
// to print a browser hint.
func (f Frontend) BrowserURL() string {
	if f.URL != "" {
		return f.URL
	}
	for _, w := range f.WaitFor {
		if isUIRootURL(w.URL) {
			return w.URL
		}
	}
	return ""
}

// isUIRootURL returns true when u has no path component or a path of
// just "/". Probe URLs for health endpoints (".../health", "/readyz",
// etc.) carry a non-trivial path; this lets us separate them from the
// "open this in a browser" URL without a schema field.
func isUIRootURL(u string) bool {
	// Find the path after the host: look for a "://" then the next "/".
	// We deliberately avoid url.Parse here to stay tolerant of slightly
	// malformed inputs (the daemon validates wait_for URLs separately).
	i := strings.Index(u, "://")
	if i < 0 {
		return false
	}
	rest := u[i+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return true // no path at all
	}
	return rest[slash:] == "/"
}

type Frontend struct {
	Kind            string            `yaml:"kind"`
	RestartRequired bool              `yaml:"restart_required,omitempty"`
	WriteFile       string            `yaml:"write_file,omitempty"`
	Template        map[string]any    `yaml:"template,omitempty"`
	Env             map[string]string `yaml:"env,omitempty"`
	MCPs            []string          `yaml:"mcps,omitempty"`
	// URL is the address the user should point their browser at once
	// the frontend is up. Optional: when unset, the daemon falls back
	// to the wait_for entry whose URL path is "/" (the conventional
	// "root of the UI") and uses that. Surfaced in the `vibe start`
	// summary as "browser: <url>" so the user doesn't have to
	// remember whether they're on :8080, :3000, or whatever.
	URL string `yaml:"url,omitempty"`

	// docker-compose kind fields.
	ComposeFile string       `yaml:"compose_file,omitempty"`
	ProjectName string       `yaml:"project_name,omitempty"`
	Services    []string     `yaml:"services,omitempty"`
	WaitFor     []WaitForURL `yaml:"wait_for,omitempty"`

	// managed kind fields. Binary is the executable to launch; Args/Workdir
	// are optional. Env, WaitFor, RestartRequired, and App are reused from
	// the existing shape above.
	Binary  string   `yaml:"binary,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	Workdir string   `yaml:"workdir,omitempty"`

	// Legacy collects YAML keys we no longer model but used to. The inline
	// map lets the KnownFields(true) decoder accept these legacy fields
	// without erroring — they're recognized by the parent decoder as
	// "claimed" by this map. Currently only `app:` (a purely cosmetic
	// display string the daemon would echo back) lands here; on Load we
	// log a one-line deprecation hint when the key is present so users
	// know it can be removed at their leisure. Genuine typos under
	// `frontend:` will still be caught at the daemon's first attempt to
	// use the value, so the strictness loss is bounded.
	Legacy map[string]any `yaml:",inline"`
}

// WaitForURL describes a health-check endpoint the docker-compose driver
// polls after `docker compose up -d`.
type WaitForURL struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"-"`
}

// waitForURLYAML is the on-disk shape; Timeout arrives as a duration string
// (e.g. "60s") which we parse in UnmarshalYAML.
type waitForURLYAML struct {
	URL     string `yaml:"url"`
	Timeout string `yaml:"timeout"`
}

// UnmarshalYAML parses the wait_for entry, including its duration-shaped
// Timeout field.
func (w *WaitForURL) UnmarshalYAML(value *yaml.Node) error {
	var raw waitForURLYAML
	raw.URL = ""
	if err := value.Decode(&raw); err != nil {
		return err
	}
	w.URL = raw.URL
	if raw.Timeout == "" {
		w.Timeout = 0
		return nil
	}
	d, err := time.ParseDuration(raw.Timeout)
	if err != nil {
		return fmt.Errorf("wait_for.timeout %q: %w", raw.Timeout, err)
	}
	w.Timeout = d
	return nil
}

// migrationHint is appended to parse errors whenever the YAML appears to be
// in the old `model:` shape. We detect this by looking for the keyword in the
// raw error message produced by yaml.v3's KnownFields(true) decoder.
const migrationHint = `: profile schema changed — the top-level 'model:' block moved under 'backend.llama_server:'. See profiles/code.example.yaml`

// Load reads, parses, and validates a profile YAML file. Unknown fields are
// rejected; ~/-prefixed paths are expanded relative to the user's home.
func Load(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open profile %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var p Profile
	if err := dec.Decode(&p); err != nil {
		// yaml.v3's KnownFields(true) reports unknown top-level keys with a
		// substring like `field model not found in type profile.Profile`.
		// Wrap that with a migration pointer so users who upgrade vibe but
		// not their profiles get a clear path forward.
		msg := err.Error()
		if strings.Contains(msg, "field model not found") || strings.Contains(msg, "\"model\"") {
			return nil, fmt.Errorf("parse profile %s%s: %w", path, migrationHint, err)
		}
		return nil, fmt.Errorf("parse profile %s: %w", path, err)
	}

	// Resolve a backend_ref into the inline Backend before normalization so
	// downstream code (validation, launch, daemon) sees a fully-populated
	// backend regardless of whether it was inlined or referenced.
	if p.BackendRef != "" {
		if !p.Backend.isEmpty() {
			return nil, fmt.Errorf("parse profile %s: backend_ref and an inline backend block are mutually exclusive", path)
		}
		def, err := LoadBackend(p.BackendRef)
		if err != nil {
			return nil, fmt.Errorf("profile %s backend_ref %q: %w", path, p.BackendRef, err)
		}
		p.Backend = def.Backend
		// A referenced backend can carry estimated VRAM + mode; the profile
		// may override either by setting its own.
		if p.EstimatedVRAMGB == 0 {
			p.EstimatedVRAMGB = def.EstimatedVRAMGB
		}
		if p.Mode == "" {
			p.Mode = def.Mode
		}
	}
	p.Backend.normalize()
	p.Frontend.WriteFile = expandTilde(p.Frontend.WriteFile)
	p.Frontend.Binary = expandTilde(p.Frontend.Binary)
	p.Frontend.Workdir = expandTilde(p.Frontend.Workdir)
	p.Frontend.ComposeFile = expandTilde(p.Frontend.ComposeFile)

	// `app:` used to be a cosmetic display field; it's been removed from
	// the schema but still appears in older user profiles on disk. Strip
	// it from Legacy so it doesn't leak into IsZero or any other consumer
	// that walks the map. Other legacy keys (if any future migration adds
	// them) stay in the map so an explicit handler can decide what to do.
	if len(p.Frontend.Legacy) > 0 {
		delete(p.Frontend.Legacy, "app")
		if len(p.Frontend.Legacy) == 0 {
			p.Frontend.Legacy = nil
		}
	}

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("validate profile %s: %w", path, err)
	}
	return &p, nil
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func (p *Profile) Validate() error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("name %q must match [a-zA-Z0-9_-]+", p.Name)
	}
	// Catch a profile that was generated by `vibe profile new` but never
	// edited: the starter templates leave `REPLACE-...` sentinels on the
	// fields the user must fill in (model path, alias, image, port, ...).
	// Re-marshalling drops the explanatory `# REPLACE:` comments, so we
	// only match real values left unedited — and surface one clear message
	// instead of a downstream "file not found" / launch failure.
	if raw, err := yaml.Marshal(p); err == nil && strings.Contains(string(raw), "REPLACE-") {
		return errors.New("profile still has REPLACE- placeholder(s) from the starter template; edit the REPLACE-marked fields before activating")
	}
	if err := p.validateMode(); err != nil {
		return err
	}

	if err := p.validateBackend(); err != nil {
		return err
	}
	return p.validateFrontend()
}

// validateMode checks the Mode field. Empty (= ModeActive) and the
// two named modes are accepted; anything else is a typo.
// Service-mode profiles also reject a frontend block — frontends are
// the active-profile path (proxy wiring + browser URL); a sidecar
// service has no frontend to launch.
func (p *Profile) validateMode() error {
	switch p.ResolvedMode() {
	case ModeActive, ModeService:
		// ok
	default:
		return fmt.Errorf("mode %q is not recognised (expected %q or %q)", p.Mode, ModeActive, ModeService)
	}
	if p.ResolvedMode() == ModeService && !p.Frontend.IsZero() {
		return errors.New("mode: service profiles cannot declare a frontend block (services are sidecars; the frontend path is for active-mode profiles only)")
	}
	return nil
}

func (p *Profile) validateBackend() error {
	// After Load, a backend_ref has been resolved into p.Backend, so the
	// union validation is identical whether the backend was inlined or
	// referenced. (The mutual-exclusion check happens in Load.)
	return p.Backend.validate()
}

// validateTabbyAPI enforces the required fields. EXL3 models are
// directories not single files, so ModelDir (not a Path/File pair)
// is the load-bearing pointer. Venv + Repo must both be set —
// tabbyAPI's start.py needs its workdir to find adapter modules,
// and we need the venv to find the right python+exllamav3.
func validateTabbyAPI(t *TabbyAPIBackend) error {
	if t.ModelDir == "" && t.Huggingface == nil {
		return errors.New("backend.tabby_api: model_dir or huggingface is required")
	}
	if t.Huggingface != nil && t.Huggingface.Repo == "" {
		return errors.New("backend.tabby_api.huggingface.repo is required when huggingface is set")
	}
	if t.Alias == "" {
		return errors.New("backend.tabby_api.alias is required")
	}
	if t.Context <= 0 {
		return errors.New("backend.tabby_api.context must be > 0")
	}
	if t.Port <= 0 {
		return errors.New("backend.tabby_api.port must be > 0 (daemon-picked ports not supported for this backend)")
	}
	if t.Venv == "" {
		return errors.New("backend.tabby_api.venv is required (python venv with exllamav3 + tabbyAPI installed)")
	}
	if t.Repo == "" {
		return errors.New("backend.tabby_api.repo is required (path to tabbyAPI checkout)")
	}
	if t.CacheMode != "" {
		switch t.CacheMode {
		case "FP16", "Q8", "Q6", "Q4":
		default:
			return fmt.Errorf("backend.tabby_api.cache_mode %q: must be one of FP16, Q8, Q6, Q4", t.CacheMode)
		}
	}
	// tabbyAPI loads models by name from inside a models dir, so the
	// alias vamp uses MUST match the on-disk dir basename. Enforce
	// consistency at load time rather than letting a confused "no
	// such model" error surface later.
	if t.ModelDir != "" {
		want := filepath.Base(t.ModelDir)
		if t.Alias != want {
			return fmt.Errorf("backend.tabby_api.alias %q must equal basename(model_dir) = %q (tabbyAPI addresses models by name inside their parent dir)", t.Alias, want)
		}
	}
	return nil
}

// validateHTTPServer enforces the docker-mode vs binary-mode XOR plus the
// always-required Port field. Image OR Binary must be set; Port must be > 0
// (this MVP doesn't support daemon-picked ports for http_server backends).
func validateHTTPServer(h *HTTPServerBackend) error {
	if h.Image == "" && h.Binary == "" {
		return errors.New("backend.http_server: exactly one of image (docker mode) or binary (process mode) is required")
	}
	if h.Image != "" && h.Binary != "" {
		return errors.New("backend.http_server: image and binary are mutually exclusive")
	}
	if h.Port <= 0 {
		return errors.New("backend.http_server.port must be > 0 (daemon-picked ports are not supported for this backend type)")
	}
	if h.Image == "" {
		// Binary mode: Volumes / GPU are docker-only.
		if len(h.Volumes) > 0 {
			return errors.New("backend.http_server.volumes is only valid in docker mode (image: set)")
		}
		if h.GPU {
			return errors.New("backend.http_server.gpu is only valid in docker mode (image: set)")
		}
	}
	return nil
}

func validateLlamaServer(m *LlamaServerBackend) error {
	if m.Path == "" {
		return errors.New("backend.llama_server.path is required")
	}
	if m.Huggingface != nil {
		if m.Huggingface.Repo == "" {
			return errors.New("backend.llama_server.huggingface.repo is required when huggingface is set")
		}
		if m.Huggingface.File == "" {
			return errors.New("backend.llama_server.huggingface.file is required when huggingface is set")
		}
		// path doesn't need to exist; `vibe pull` will create it.
	} else if _, err := os.Stat(m.Path); err != nil {
		return fmt.Errorf("backend.llama_server.path %s: %w", m.Path, err)
	}
	if m.Alias == "" {
		return errors.New("backend.llama_server.alias is required (must match /v1/models id)")
	}
	if m.Context <= 0 {
		return errors.New("backend.llama_server.context must be > 0")
	}
	if m.Binary != "" {
		info, err := os.Stat(m.Binary)
		if err != nil {
			return fmt.Errorf("backend.llama_server.binary %s: %w", m.Binary, err)
		}
		if info.IsDir() {
			return fmt.Errorf("backend.llama_server.binary %s: is a directory", m.Binary)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("backend.llama_server.binary %s: not executable (mode %v)", m.Binary, info.Mode().Perm())
		}
	}
	if m.Huggingface != nil && m.Huggingface.MMProjFile != "" && m.MMProj == "" {
		return errors.New("backend.llama_server.mmproj is required when huggingface.mmproj_file is set (it's the target path for the pulled file)")
	}
	if m.MMProj != "" {
		// If no HF spec covers the mmproj, the file must already exist.
		// Mirrors the Path/Huggingface relationship above.
		hasHFPull := m.Huggingface != nil && m.Huggingface.MMProjFile != ""
		if !hasHFPull {
			if _, err := os.Stat(m.MMProj); err != nil {
				return fmt.Errorf("backend.llama_server.mmproj %s: %w", m.MMProj, err)
			}
		}
	}
	if m.Huggingface != nil && m.Huggingface.DraftFile != "" && m.DraftModel == "" {
		return errors.New("backend.llama_server.draft_model is required when huggingface.draft_file is set (it's the target path for the pulled file)")
	}
	if (m.SpecType != "" || m.SpecDraftNMax != 0) && m.DraftModel == "" {
		return errors.New("backend.llama_server.spec_type / spec_draft_n_max require draft_model to be set")
	}
	if m.DraftModel != "" {
		hasHFPull := m.Huggingface != nil && m.Huggingface.DraftFile != ""
		if !hasHFPull {
			if _, err := os.Stat(m.DraftModel); err != nil {
				return fmt.Errorf("backend.llama_server.draft_model %s: %w", m.DraftModel, err)
			}
		}
		// draft-mtp gives ~0% draft acceptance with a quantized KV cache —
		// the speedup silently evaporates. Reject the footgun rather than
		// let it look enabled but do nothing. (f16, the llama-server default
		// when cache_type is unset, is correct.)
		specType := m.SpecType
		if specType == "" {
			specType = "draft-mtp"
		}
		if specType == "draft-mtp" && (isQuantizedKV(m.CacheTypeK) || isQuantizedKV(m.CacheTypeV)) {
			return errors.New("draft-mtp requires an f16 KV cache: remove cache_type_k/cache_type_v (or set them to f16); a quantized KV cache yields ~0% draft acceptance")
		}
	}
	return nil
}

// isQuantizedKV reports whether a cache_type_k/v value is a quantized type
// (anything other than empty or an f16/f32 float cache).
func isQuantizedKV(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "f16", "f32", "bf16":
		return false
	default:
		return true
	}
}

func validateComfyUI(c *ComfyUIBackend) error {
	if c.Dir == "" {
		return errors.New("backend.comfyui.dir is required")
	}
	info, err := os.Stat(c.Dir)
	if err != nil {
		return fmt.Errorf("backend.comfyui.dir %s: %w", c.Dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backend.comfyui.dir %s: not a directory", c.Dir)
	}
	mainPy := filepath.Join(c.Dir, "main.py")
	if _, err := os.Stat(mainPy); err != nil {
		return fmt.Errorf("backend.comfyui.dir %s missing main.py: %w", c.Dir, err)
	}
	if c.Port < 0 {
		return fmt.Errorf("backend.comfyui.port %d must be >= 0", c.Port)
	}
	return nil
}

func (p *Profile) validateFrontend() error {
	// ComfyUI ships its own UI; reject any frontend config on those profiles.
	if p.Backend.ComfyUI != nil {
		if !p.Frontend.IsZero() {
			return errors.New("frontend is not supported for comfyui backends (ComfyUI ships its own UI)")
		}
		return nil
	}

	// http_server backends are wrapped HTTP services (TTS, embedding servers,
	// third-party inference) consumed directly via the vibe proxy. There's
	// no "frontend" to launch separately, so reject any frontend block and
	// short-circuit.
	if p.Backend.HTTPServer != nil {
		if !p.Frontend.IsZero() {
			return errors.New("frontend is not supported for http_server backends (the HTTP server is the deliverable; consume it via the vibe proxy)")
		}
		return nil
	}

	// tabby_api backends are pipeline-driven by default — vamp text stages
	// reach the OpenAI-compatible /v1 surface through the proxy without
	// needing a UI. If a user wants OpenWebUI on top, they can add an
	// explicit frontend block; otherwise an empty frontend is fine here.
	// (Compare to llama_server, which still demands a frontend for Phase-1
	// historical reasons — tabby_api shipped after that requirement was
	// relaxed and follows the modern shape.)
	if p.Backend.TabbyAPI != nil {
		if p.Frontend.IsZero() {
			return nil
		}
		// Fall through to the existing validation if the user did supply
		// a frontend block — same shape rules apply.
	}

	// Service-mode llama_server backends are sidecar embedding /
	// inference servers — they're consumed directly via the published
	// port, never via a launched UI. validateMode already rejected a
	// frontend block on service profiles; reaching here with an empty
	// frontend on a service-mode llama_server is the supported shape.
	if p.ResolvedMode() == ModeService {
		return nil
	}

	// Llama-server profiles still require a frontend block (Phase 1 behavior).
	switch p.Frontend.Kind {
	case FrontendExternal:
		if p.Frontend.WriteFile == "" {
			return errors.New("frontend.write_file is required for kind=external")
		}
		if len(p.Frontend.Template) == 0 {
			return errors.New("frontend.template is required for kind=external")
		}
		if p.Frontend.ComposeFile != "" {
			return errors.New("frontend.compose_file is only valid for kind=docker-compose")
		}
		if p.Frontend.ProjectName != "" {
			return errors.New("frontend.project_name is only valid for kind=docker-compose")
		}
		if len(p.Frontend.Services) > 0 {
			return errors.New("frontend.services is only valid for kind=docker-compose")
		}
		if len(p.Frontend.WaitFor) > 0 {
			return errors.New("frontend.wait_for is only valid for kind=docker-compose")
		}
		if p.Frontend.Binary != "" {
			return errors.New("frontend.binary is only valid for kind=managed")
		}
		if len(p.Frontend.Args) > 0 {
			return errors.New("frontend.args is only valid for kind=managed")
		}
		if p.Frontend.Workdir != "" {
			return errors.New("frontend.workdir is only valid for kind=managed")
		}
	case FrontendDockerCompose:
		if p.Frontend.ComposeFile == "" {
			return errors.New("frontend.compose_file is required for kind=docker-compose")
		}
		if p.Frontend.WriteFile != "" {
			return errors.New("frontend.write_file is only valid for kind=external")
		}
		if len(p.Frontend.Template) > 0 {
			return errors.New("frontend.template is only valid for kind=external")
		}
		if len(p.Frontend.MCPs) > 0 {
			return errors.New("frontend.mcps is only valid for kind=external")
		}
		if p.Frontend.Binary != "" {
			return errors.New("frontend.binary is only valid for kind=managed")
		}
		if len(p.Frontend.Args) > 0 {
			return errors.New("frontend.args is only valid for kind=managed")
		}
		if p.Frontend.Workdir != "" {
			return errors.New("frontend.workdir is only valid for kind=managed")
		}
		seenSvc := make(map[string]struct{}, len(p.Frontend.Services))
		for _, s := range p.Frontend.Services {
			if s == "" {
				return errors.New("frontend.services contains empty entry")
			}
			if _, dup := seenSvc[s]; dup {
				return fmt.Errorf("frontend.services contains duplicate %q", s)
			}
			seenSvc[s] = struct{}{}
		}
		for i, w := range p.Frontend.WaitFor {
			if w.URL == "" {
				return fmt.Errorf("frontend.wait_for[%d].url is required", i)
			}
		}
	case FrontendManaged:
		if p.Frontend.Binary == "" {
			return errors.New("frontend.binary is required for kind=managed")
		}
		// write_file / template / mcps are accepted: tools like opencode
		// need both a config file rendered AND a binary spawned, so the
		// managed driver reuses external's config-write step. If you don't
		// need a config file, just omit those fields.
		if len(p.Frontend.Template) > 0 && p.Frontend.WriteFile == "" {
			return errors.New("frontend.template requires frontend.write_file")
		}
		if len(p.Frontend.MCPs) > 0 && p.Frontend.WriteFile == "" {
			return errors.New("frontend.mcps requires frontend.write_file (the MCPs are merged into the rendered template)")
		}
		if p.Frontend.ComposeFile != "" {
			return errors.New("frontend.compose_file is only valid for kind=docker-compose")
		}
		if p.Frontend.ProjectName != "" {
			return errors.New("frontend.project_name is only valid for kind=docker-compose")
		}
		if len(p.Frontend.Services) > 0 {
			return errors.New("frontend.services is only valid for kind=docker-compose")
		}
		info, err := os.Stat(p.Frontend.Binary)
		if err != nil {
			return fmt.Errorf("frontend.binary %s: %w", p.Frontend.Binary, err)
		}
		if info.IsDir() {
			return fmt.Errorf("frontend.binary %s: is a directory", p.Frontend.Binary)
		}
		// Require any executable bit (owner, group, or other) to be set so we
		// fail validation rather than failing at exec time with a confusing
		// "permission denied".
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("frontend.binary %s: not executable (mode %v)", p.Frontend.Binary, info.Mode().Perm())
		}
		for i, w := range p.Frontend.WaitFor {
			if w.URL == "" {
				return fmt.Errorf("frontend.wait_for[%d].url is required", i)
			}
		}
	case "":
		return errors.New("frontend.kind is required")
	default:
		return fmt.Errorf("frontend.kind %q is unknown (expected: external, docker-compose, managed)", p.Frontend.Kind)
	}

	if len(p.Frontend.MCPs) > 0 {
		// mcps are valid for kind=external and kind=managed (both render a
		// config file the MCPs merge into). docker-compose already rejected
		// them in its own branch above, so only those two kinds reach here.
		seen := make(map[string]struct{}, len(p.Frontend.MCPs))
		for _, name := range p.Frontend.MCPs {
			if _, dup := seen[name]; dup {
				return fmt.Errorf("frontend.mcps contains duplicate %q", name)
			}
			seen[name] = struct{}{}
		}
	}

	return nil
}

// IsZero reports whether the Frontend was left unset. Used to allow ComfyUI
// profiles to omit `frontend:` entirely without tripping the kind validator.
// Legacy is intentionally ignored: a profile carrying only the deprecated
// `app:` field (and nothing else) is still semantically empty.
func (f Frontend) IsZero() bool {
	return f.Kind == "" &&
		!f.RestartRequired &&
		f.WriteFile == "" &&
		len(f.Template) == 0 &&
		len(f.Env) == 0 &&
		len(f.MCPs) == 0 &&
		f.ComposeFile == "" &&
		f.ProjectName == "" &&
		len(f.Services) == 0 &&
		len(f.WaitFor) == 0 &&
		f.Binary == "" &&
		len(f.Args) == 0 &&
		f.Workdir == ""
}

func expandTilde(p string) string {
	if p == "" || !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
