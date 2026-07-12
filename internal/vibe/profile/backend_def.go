package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"gopkg.in/yaml.v3"
)

// BackendDef is a named, reusable model-server spec stored under
// $XDG_CONFIG_HOME/vibe/backends/<name>.yaml. A profile references one via
// `backend_ref` instead of inlining a `backend:` block, so the same model
// (e.g. qwen3.6-27b) is defined once and shared across many frontends, and
// vamp capabilities can target a backend by name without depending on a
// specific frontend-bearing profile.
type BackendDef struct {
	Name            string  `yaml:"name"`
	Backend         Backend `yaml:"backend"`
	EstimatedVRAMGB float64 `yaml:"estimated_vram_gb,omitempty"`
	// Mode mirrors Profile.Mode ("" / "active" / "service"); a service
	// backend (embedding server, TTS daemon) runs as a sidecar.
	Mode string `yaml:"mode,omitempty"`
}

// isEmpty reports whether no backend sub-block is set (the zero union).
func (b Backend) isEmpty() bool {
	return b.LlamaServer == nil && b.ComfyUI == nil && b.HTTPServer == nil && b.TabbyAPI == nil
}

// normalize applies in-place defaults (llama_server.parallel) and tilde-expands
// every path field. Shared by profile Load and LoadBackend so an inline
// backend and a referenced one are treated identically.
func (b *Backend) normalize() {
	if b.LlamaServer != nil {
		if b.LlamaServer.Parallel == 0 {
			b.LlamaServer.Parallel = 1
		}
		b.LlamaServer.Path = expandTilde(b.LlamaServer.Path)
		b.LlamaServer.Binary = expandTilde(b.LlamaServer.Binary)
		b.LlamaServer.MMProj = expandTilde(b.LlamaServer.MMProj)
		b.LlamaServer.DraftModel = expandTilde(b.LlamaServer.DraftModel)
	}
	if b.HTTPServer != nil {
		for i, v := range b.HTTPServer.Volumes {
			parts := strings.SplitN(v, ":", 2)
			if len(parts) == 2 {
				parts[0] = expandTilde(parts[0])
				b.HTTPServer.Volumes[i] = strings.Join(parts, ":")
			}
		}
		b.HTTPServer.Binary = expandTilde(b.HTTPServer.Binary)
	}
	if b.ComfyUI != nil {
		b.ComfyUI.Dir = expandTilde(b.ComfyUI.Dir)
		b.ComfyUI.Python = expandTilde(b.ComfyUI.Python)
	}
	if b.TabbyAPI != nil {
		b.TabbyAPI.ModelDir = expandTilde(b.TabbyAPI.ModelDir)
		b.TabbyAPI.Venv = expandTilde(b.TabbyAPI.Venv)
		b.TabbyAPI.Repo = expandTilde(b.TabbyAPI.Repo)
		b.TabbyAPI.DraftModelDir = expandTilde(b.TabbyAPI.DraftModelDir)
	}
}

// validate enforces the discriminated-union rule (exactly one sub-block set)
// and runs the per-backend validation.
func (b Backend) validate() error {
	set := 0
	for _, on := range []bool{b.LlamaServer != nil, b.ComfyUI != nil, b.HTTPServer != nil, b.TabbyAPI != nil} {
		if on {
			set++
		}
	}
	switch {
	case set == 0:
		return errors.New("backend is required: set exactly one of backend.llama_server, backend.comfyui, backend.http_server, or backend.tabby_api")
	case set > 1:
		return errors.New("backend: only one of backend.llama_server, backend.comfyui, backend.http_server, or backend.tabby_api may be set")
	case b.LlamaServer != nil:
		return validateLlamaServer(b.LlamaServer)
	case b.ComfyUI != nil:
		return validateComfyUI(b.ComfyUI)
	case b.HTTPServer != nil:
		return validateHTTPServer(b.HTTPServer)
	default:
		return validateTabbyAPI(b.TabbyAPI)
	}
}

// LoadBackend reads, parses, normalizes, and validates a named backend
// definition from $XDG_CONFIG_HOME/vibe/backends/<name>.yaml. Unknown fields
// are rejected; ~/-prefixed paths are expanded.
func LoadBackend(name string) (*BackendDef, error) {
	if name == "" {
		return nil, errors.New("backend name is required")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("backend name %q must be a bare file stem under backends/ (no path separators)", name)
	}
	// A trailing extension would silently double up ("<name>.yaml.yaml") and
	// produce a confusing not-found error instead of telling the author to
	// drop it.
	if ext := filepath.Ext(name); ext == ".yaml" || ext == ".yml" {
		return nil, fmt.Errorf("backend name %q should not include the %s extension (use %q)",
			name, ext, strings.TrimSuffix(name, ext))
	}
	path := filepath.Join(paths.BackendsDir(), name+".yaml")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open backend %s: %w", name, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var def BackendDef
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("parse backend %s: %w", name, err)
	}
	// The filename is the backend's identity everywhere (backend_ref,
	// StartRequest.backend, the synthetic profile's Name); a stale `name:`
	// after a rename/copy would silently mislead, so reject the mismatch.
	if def.Name != "" && def.Name != name {
		return nil, fmt.Errorf("backend %s: name %q does not match filename (rename the file or fix name:)", name, def.Name)
	}
	def.Backend.normalize()
	if err := def.Backend.validate(); err != nil {
		return nil, fmt.Errorf("backend %s: %w", name, err)
	}
	return &def, nil
}

// ListBackends returns the names of backend definitions on disk (filenames
// without the .yaml suffix), sorted. A missing backends dir yields an empty
// list, not an error.
func ListBackends() ([]string, error) {
	entries, err := os.ReadDir(paths.BackendsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if ext := filepath.Ext(n); ext == ".yaml" || ext == ".yml" {
			names = append(names, strings.TrimSuffix(n, ext))
		}
	}
	return names, nil
}
