package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	// Lifecycle carries router-lifecycle knobs consumed by `vibe router
	// render` for external backends (docs/design/router-lifecycle.md §4).
	// Accepted on any def so a backend can flip external on/off without
	// having to delete the block, but only external defs are rendered.
	Lifecycle *Lifecycle `yaml:"lifecycle,omitempty"`
	// Router carries client-facing naming knobs for the rendered llama-swap
	// entry (extra aliases, alias-collision ownership, catalog visibility).
	Router *RouterOpts `yaml:"router,omitempty"`
	// Fingerprint selects the flags_sha256 failure mode at announce (C3):
	// "strict" EXCLUDES the model from the front render on mismatch
	// (embed-class models, where flag drift is silent retrieval damage);
	// "advisory" (default) raises the loud event and keeps serving — a
	// hash-normalization bug must not yank a working chat model.
	Fingerprint string `yaml:"fingerprint,omitempty"`
	// Cell names the fleet cell that owns this backend (fleet-control C2
	// §6): `vibe router render --cell <name>` places the def on that cell's
	// llama-swap, and the front render turns every cell-assigned def into a
	// peers: stanza. Empty means unassigned — rendered wherever the render
	// runs. The NAME is validated at render time against hosts.yaml, not
	// here: defs also load standalone for daemon activation, where an
	// unknown cell is meaningless.
	Cell string `yaml:"cell,omitempty"`
	// Trial marks a def written by `vibe model try` (fleet-control C18):
	// a candidate under evaluation on ONE cell, not part of the fleet's
	// model set. It is excluded from the FRONT render, which is the
	// structural reason a trial can never become fleet catalog no matter
	// whose def checkout it lands in — see router.Render.
	//
	// Promotion is deleting this one line and committing the def. That is
	// deliberate: nothing in vibe can promote a trial, because entering
	// the fleet catalog is a change to the shared def set and the shared
	// def set is a git repo with a human on it.
	Trial bool `yaml:"trial,omitempty"`
}

// validateTrial holds the two things a trial def must be for its
// exclusion to mean anything. Both are structural, not stylistic: an
// unassigned trial would render onto whatever box ran a render (its
// weights are on ONE box), and a non-external trial is not served by the
// router at all, so nothing about the front-render exclusion applies to
// it and the flag would be decoration.
func (d *BackendDef) validateTrial() error {
	if !d.Trial {
		return nil
	}
	if d.Cell == "" {
		return errors.New("trial: true requires cell: — a trial's weights live on one box, and an unassigned def renders wherever a render runs")
	}
	if !d.Backend.External {
		return errors.New("trial: true requires backend.external: true — a trial is evaluated through the router, and the front-render exclusion that keeps it out of the fleet catalog only applies to rendered defs")
	}
	if d.Backend.CloudPeer != nil {
		return errors.New("trial: true is not valid for cloud_peer (there are no weights to try and no cell to try them on)")
	}
	return nil
}

// Lifecycle is the router-lifecycle block on a backend def. All fields feed
// the llama-swap config renderer; none affect vibe-supervised backends.
type Lifecycle struct {
	// TTL is the idle-unload timeout, rendered to llama-swap's per-model
	// `ttl` (seconds). Explicit 0 means "never unload"; absent defaults to
	// 2h for external LLM backends at render time.
	TTL *Duration `yaml:"ttl,omitempty"`
	// Preload asks the router to load this model at startup
	// (hooks.on_startup.preload).
	Preload bool `yaml:"preload,omitempty"`
	// EvictCost is reserved for llama-swap matrix mode (higher = evicted
	// last). Accepted and stored now; the renderer emits it only once a
	// matrix section exists (multi-model co-residency, phase A6).
	EvictCost int `yaml:"evict_cost,omitempty"`
	// Refresh names a periodic restart policy. Only "nightly_if_idle" is
	// recognised: a systemd timer that unloads the model at 04:00 when idle
	// (an unloaded model makes the unload a no-op, so "if idle" is free).
	// TODO(router-lifecycle A2+): rendering the systemd timer unit is out of
	// scope for the config renderer pass; the field is accepted and stored
	// so defs can declare intent ahead of the timer renderer.
	Refresh string `yaml:"refresh,omitempty"`
	// StartTimeout is this model's cold-start budget. The renderer's global
	// healthCheckTimeout is max(300s, max start_timeout over rendered defs),
	// so one slow-loading model raises the shared ceiling.
	StartTimeout *Duration `yaml:"start_timeout,omitempty"`
}

// RefreshNightlyIfIdle is the only recognised Lifecycle.Refresh policy.
const RefreshNightlyIfIdle = "nightly_if_idle"

func (l *Lifecycle) validate() error {
	if l == nil {
		return nil
	}
	if l.Refresh != "" && l.Refresh != RefreshNightlyIfIdle {
		return fmt.Errorf("lifecycle.refresh %q is not recognised (expected %q)", l.Refresh, RefreshNightlyIfIdle)
	}
	if l.EvictCost < 0 {
		return errors.New("lifecycle.evict_cost must be >= 0")
	}
	return nil
}

// RouterOpts controls how a backend def surfaces in the rendered llama-swap
// config. The def NAME is always the canonical model id; these knobs shape
// the extra client-facing names around it.
type RouterOpts struct {
	// Aliases are extra client-facing model names. When absent, the
	// llama_server alias field is the default alias (kept for stale-state
	// compat with configs written against the pre-router alias).
	Aliases []string `yaml:"aliases,omitempty"`
	// AliasOwner resolves alias collisions: when several defs claim the same
	// alias (e.g. a base model and its --reasoning-off variant sharing one
	// llama_server alias), the def with alias_owner: true keeps the alias
	// and the others render without it. Zero or multiple owners for a
	// contested alias is a render error.
	AliasOwner bool `yaml:"alias_owner,omitempty"`
	// Unlisted hides the model from the router's /v1/models catalog.
	Unlisted bool `yaml:"unlisted,omitempty"`
}

func (r *RouterOpts) validate() error {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, a := range r.Aliases {
		if a == "" {
			return errors.New("router.aliases entries must be non-empty")
		}
		if seen[a] {
			return fmt.Errorf("router.aliases lists %q twice", a)
		}
		seen[a] = true
	}
	return nil
}

// Duration is a time.Duration that unmarshals from the Go duration string
// form ("2h", "45m") used across vibe YAML. Negative values are rejected —
// llama-swap's ttl -1 ("use globalTTL") is deliberately not exposed; a def
// that wants the default simply omits the field.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dd, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	if dd < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	*d = Duration(dd)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Seconds returns the duration in whole seconds, the unit llama-swap's ttl
// and healthCheckTimeout fields speak.
func (d Duration) Seconds() int {
	return int(time.Duration(d) / time.Second)
}

// isEmpty reports whether the backend block is the zero union. External is
// included so `backend: {external: true}` next to a backend_ref trips the
// mutual-exclusion error instead of being silently discarded when the ref
// resolves over it.
func (b Backend) isEmpty() bool {
	return !b.External && b.LlamaServer == nil && b.ComfyUI == nil && b.HTTPServer == nil && b.TabbyAPI == nil && b.CloudPeer == nil && b.MLXServer == nil
}

// normalize applies in-place defaults (llama_server.parallel) and tilde-expands
// every path field. Shared by profile Load and LoadBackend so an inline
// backend and a referenced one are treated identically.
func (b *Backend) normalize() {
	// cloud_peer implies external semantics: there is never a process for
	// vibe to launch — the router proxies to the cloud API and readiness is
	// "the peer's model ids appear in the router catalog". Forcing the flag
	// here lets every downstream external check stay a single boolean.
	if b.CloudPeer != nil {
		b.External = true
	}
	if b.LlamaServer != nil {
		if b.LlamaServer.Parallel == 0 {
			b.LlamaServer.Parallel = 1
		}
		b.LlamaServer.Path = expandTilde(b.LlamaServer.Path)
		b.LlamaServer.Binary = expandTilde(b.LlamaServer.Binary)
		b.LlamaServer.MMProj = expandTilde(b.LlamaServer.MMProj)
		b.LlamaServer.DraftModel = expandTilde(b.LlamaServer.DraftModel)
		b.LlamaServer.ChatTemplateFile = expandTilde(b.LlamaServer.ChatTemplateFile)
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
	if b.MLXServer != nil {
		if b.MLXServer.Host == "" {
			b.MLXServer.Host = "127.0.0.1"
		}
		// The expanded ModelDir is what reaches --model, /v1/models, and
		// the proxy's rewrite target, so it has to be absolute here or the
		// three would disagree about the model's identity.
		b.MLXServer.ModelDir = expandTilde(b.MLXServer.ModelDir)
		b.MLXServer.Venv = expandTilde(b.MLXServer.Venv)
		b.MLXServer.DraftModel = expandTilde(b.MLXServer.DraftModel)
	}
}

// markCellScoped flags path-stat-bearing sub-structs when the def is
// fleet-scoped (cell: set): their paths exist on the cell, so os.Stat
// validation is gated off at load (fleet.md §4.2's host: rule applied to
// cell:). Everything non-path still validates.
func (b *Backend) markCellScoped(scoped bool) {
	if b.LlamaServer != nil {
		b.LlamaServer.cellScoped = scoped
	}
	if b.ComfyUI != nil {
		b.ComfyUI.cellScoped = scoped
	}
}

// validate enforces the discriminated-union rule (exactly one sub-block set)
// and runs the per-backend validation.
func (b Backend) validate() error {
	set := 0
	for _, on := range []bool{b.LlamaServer != nil, b.ComfyUI != nil, b.HTTPServer != nil, b.TabbyAPI != nil, b.CloudPeer != nil, b.MLXServer != nil} {
		if on {
			set++
		}
	}
	switch {
	case set == 0:
		return errors.New("backend is required: set exactly one of backend.llama_server, backend.comfyui, backend.http_server, backend.tabby_api, backend.cloud_peer, or backend.mlx_server")
	case set > 1:
		return errors.New("backend: only one of backend.llama_server, backend.comfyui, backend.http_server, backend.tabby_api, backend.cloud_peer, or backend.mlx_server may be set")
	}
	// external makes sense where the router serves the backend: LLM kinds
	// appear on /v1/models, and comfyui rides llama-swap's /upstream/<id>
	// raw passthrough as a swap tenant (design §16 experiment). http_server
	// wraps arbitrary vibe-supervised services — external there could only
	// be a mistake. (cloud_peer is external by definition; normalize forces
	// the flag.)
	if b.External && b.HTTPServer != nil {
		return errors.New("backend.external is not valid for http_server backends (they stay vibe-supervised); llama_server, tabby_api, cloud_peer, and comfyui may be external")
	}
	switch {
	case b.LlamaServer != nil:
		return validateLlamaServer(b.LlamaServer)
	case b.ComfyUI != nil:
		return validateComfyUI(b.ComfyUI)
	case b.HTTPServer != nil:
		return validateHTTPServer(b.HTTPServer)
	case b.CloudPeer != nil:
		return validateCloudPeer(b.CloudPeer)
	case b.MLXServer != nil:
		return validateMLXServer(b.MLXServer)
	default:
		return validateTabbyAPI(b.TabbyAPI)
	}
}

// LoadBackend reads, parses, normalizes, and validates a named backend
// definition from $XDG_CONFIG_HOME/vibe/backends/<name>.yaml. Unknown fields
// are rejected; ~/-prefixed paths are expanded.
func LoadBackend(name string) (*BackendDef, error) {
	return LoadBackendFrom(paths.BackendsDir(), name)
}

// LoadBackendFrom is LoadBackend against an explicit backends directory.
// Exists so the router config renderer (and its tests) can load defs from a
// fixture dir instead of the user's config home.
func LoadBackendFrom(dir, name string) (*BackendDef, error) {
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
	path := filepath.Join(dir, name+".yaml")
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
	// cell: set ⇒ os.Stat validation is gated OFF, mirroring fleet.md
	// §4.2's host: rule: a fleet-scoped def's paths exist on its cell,
	// not necessarily on the box loading it (the canonical def checkout
	// puts every def on every box). Everything else — shape, aliases,
	// hf rules — still validates; the cell's own render/activation
	// surfaces a bad path where the path is real.
	def.Backend.markCellScoped(def.Cell != "")
	if err := def.Backend.validate(); err != nil {
		return nil, fmt.Errorf("backend %s: %w", name, err)
	}
	if err := def.Lifecycle.validate(); err != nil {
		return nil, fmt.Errorf("backend %s: %w", name, err)
	}
	if err := def.Router.validate(); err != nil {
		return nil, fmt.Errorf("backend %s: %w", name, err)
	}
	if err := def.validateTrial(); err != nil {
		return nil, fmt.Errorf("backend %s: %w", name, err)
	}
	switch def.Fingerprint {
	case "", "strict", "advisory":
	default:
		return nil, fmt.Errorf("backend %s: fingerprint %q must be \"strict\" or \"advisory\"", name, def.Fingerprint)
	}
	// Mirrors Profile.validateMode: backend defs are also activated directly
	// (StartRequest.backend synthesizes a profile without running Validate),
	// so the service/external contradiction must be caught here too.
	if def.Mode == ModeService && def.Backend.External {
		return nil, fmt.Errorf("backend %s: mode: service cannot be combined with backend.external (the router owns external lifecycles; services are vibe-supervised sidecars)", name)
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
