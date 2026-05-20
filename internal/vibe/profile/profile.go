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
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description,omitempty"`
	Backend         Backend  `yaml:"backend"`
	Frontend        Frontend `yaml:"frontend,omitempty"`
	EstimatedVRAMGB float64  `yaml:"estimated_vram_gb,omitempty"`
}

// Backend is a discriminated union of supported local-AI backends. Exactly
// one sub-block must be non-nil; loaders reject zero-or-both.
type Backend struct {
	LlamaServer *LlamaServerBackend `yaml:"llama_server,omitempty"`
	ComfyUI     *ComfyUIBackend     `yaml:"comfyui,omitempty"`
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
	// MMProj is the multimodal projector GGUF that llama-server loads
	// via --mmproj to enable image input on vision-capable models
	// (Gemma 3, Qwen2.5-VL, LLaVA, etc.). The accompanying weights file
	// at Path must be a vision-capable model; loading a text-only model
	// with an mmproj is a configuration error llama-server will reject.
	// When Huggingface.MMProjFile is set, this path is the *target* for
	// the pulled file and may not yet exist.
	MMProj string `yaml:"mmproj,omitempty"`
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

	if p.Backend.LlamaServer != nil {
		if p.Backend.LlamaServer.Parallel == 0 {
			p.Backend.LlamaServer.Parallel = 1
		}
		p.Backend.LlamaServer.Path = expandTilde(p.Backend.LlamaServer.Path)
		p.Backend.LlamaServer.Binary = expandTilde(p.Backend.LlamaServer.Binary)
		p.Backend.LlamaServer.MMProj = expandTilde(p.Backend.LlamaServer.MMProj)
	}
	if p.Backend.ComfyUI != nil {
		p.Backend.ComfyUI.Dir = expandTilde(p.Backend.ComfyUI.Dir)
		p.Backend.ComfyUI.Python = expandTilde(p.Backend.ComfyUI.Python)
	}
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

	if err := p.validateBackend(); err != nil {
		return err
	}
	return p.validateFrontend()
}

func (p *Profile) validateBackend() error {
	llama := p.Backend.LlamaServer
	comfy := p.Backend.ComfyUI
	switch {
	case llama == nil && comfy == nil:
		return errors.New("backend is required: set exactly one of backend.llama_server or backend.comfyui")
	case llama != nil && comfy != nil:
		return errors.New("backend: only one of backend.llama_server or backend.comfyui may be set")
	case llama != nil:
		return validateLlamaServer(llama)
	case comfy != nil:
		return validateComfyUI(comfy)
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
	return nil
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
		if p.Frontend.Kind != FrontendExternal {
			return fmt.Errorf("frontend.mcps requires frontend.kind=external (got %q)", p.Frontend.Kind)
		}
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
