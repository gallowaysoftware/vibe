package profile

import (
	"encoding/json"
	"fmt"
)

// JSONSchemaDraft is the $schema URL we emit on the root of the generated
// document. Draft-07 matches the dialect yaml-language-server speaks
// fluently and what most schema-aware editor extensions (VS Code's RedHat
// YAML, IntelliJ, Helix) target by default. We deliberately stay on the
// same draft as `vamp schema` so users running both tools get consistent
// editor behavior across the two YAML shapes.
const JSONSchemaDraft = "http://json-schema.org/draft-07/schema#"

// schemaProperty is the minimal subset of JSON Schema we model by hand for
// profile.yaml. The keyword set is intentionally the same as vamp's
// internal schemaProperty (kept duplicate rather than shared to keep the
// vibe -> vamp dependency direction clean — `vibe` should not import
// internal/vamp/). New keywords get a new field; we avoid the
// gojsonschema / kin-openapi libraries because the schema's shape is
// fixed by the Profile / Backend / Frontend Go types and pinning it to a
// hand-built struct makes the emitted JSON diff-stable across Go
// upgrades.
type schemaProperty struct {
	Schema               string                     `json:"$schema,omitempty"`
	Ref                  string                     `json:"$ref,omitempty"`
	ID                   string                     `json:"$id,omitempty"`
	Title                string                     `json:"title,omitempty"`
	Type                 any                        `json:"type,omitempty"` // string or []string for nullable shapes
	Description          string                     `json:"description,omitempty"`
	Enum                 []any                      `json:"enum,omitempty"`
	Pattern              string                     `json:"pattern,omitempty"`
	Default              any                        `json:"default,omitempty"`
	Examples             []any                      `json:"examples,omitempty"`
	Minimum              *float64                   `json:"minimum,omitempty"`
	Items                *schemaProperty            `json:"items,omitempty"`
	Properties           map[string]*schemaProperty `json:"properties,omitempty"`
	AdditionalProperties any                        `json:"additionalProperties,omitempty"` // bool or *schemaProperty
	Required             []string                   `json:"required,omitempty"`
	OneOf                []*schemaProperty          `json:"oneOf,omitempty"`
	AnyOf                []*schemaProperty          `json:"anyOf,omitempty"`
	Definitions          map[string]*schemaProperty `json:"definitions,omitempty"`
}

// Schema returns the JSON Schema (draft-07) describing profile.yaml as a
// hand-rolled Go struct ready for JSON marshaling. The shape mirrors the
// Profile / Backend / Frontend types in profile.go; the test in
// schema_test.go round-trips the generated schema and re-validates one of
// the bundled profile templates against it so the two stay in sync.
//
// The backend discriminator (llama_server XOR comfyui) is expressed via
// a `oneOf` on the `backend` property: exactly one of the two sub-blocks
// must be present, matching Profile.validateBackend. Per-kind frontend
// constraints (write_file required for external, compose_file for
// docker-compose, binary for managed) are NOT encoded as a oneOf on the
// frontend — yaml-language-server's draft-07 implementation has trouble
// surfacing useful errors through deeply-nested oneOf branches, and the
// runtime Validator already catches kind-mismatch cases with clear text.
// We model the frontend as an open struct keyed by `kind` and let the
// validator enforce inter-field constraints.
func Schema() *schemaProperty {
	frontendKindEnum := []any{
		FrontendExternal,
		FrontendDockerCompose,
		FrontendManaged,
	}

	huggingface := &schemaProperty{
		Type:                 "object",
		Description:          "HuggingFace pull spec. When set, vibe downloads backend.llama_server.path on demand (via `vibe pull` or implicitly at start). Setting mmproj_file additionally pulls a multimodal projector to backend.llama_server.mmproj.",
		AdditionalProperties: false,
		Required:             []string{"repo", "file"},
		Properties: map[string]*schemaProperty{
			"repo": {
				Type:        "string",
				Description: "HuggingFace repository (e.g. \"Qwen/Qwen3-Coder-30B-A3B-Instruct-GGUF\").",
			},
			"file": {
				Type:        "string",
				Description: "Filename to fetch from the repo (e.g. \"Qwen3-Coder-30B-A3B-Instruct-Q6_K.gguf\").",
			},
			"revision": {
				Type:        "string",
				Description: "Git revision (branch / tag / commit). Defaults to \"main\".",
			},
			"mmproj_file": {
				Type:        "string",
				Description: "Multimodal projector filename for vision-capable models. When set, backend.llama_server.mmproj must also be set as the target path.",
			},
		},
	}

	llamaServer := &schemaProperty{
		Type:                 "object",
		Description:          "llama-server child process backend. Supervises a single llama.cpp `llama-server` instance and proxies /v1/* over the vibe proxy port.",
		AdditionalProperties: false,
		Required:             []string{"path", "alias", "context"},
		Properties: map[string]*schemaProperty{
			"path": {
				Type:        "string",
				Description: "Absolute or ~/-prefixed path to the GGUF model file. When huggingface is set, the file does not need to exist at load time (pull creates it); without huggingface, validation requires the file on disk.",
			},
			"huggingface": huggingface,
			"alias": {
				Type:        "string",
				Description: "Model id llama-server advertises at /v1/models. Must match the alias clients send in their requests.",
			},
			"context": {
				Type:        "integer",
				Description: "Context window size in tokens passed to llama-server as --ctx-size.",
				Minimum:     float64Ptr(1),
			},
			"parallel": {
				Type:        "integer",
				Description: "Number of parallel decoding slots (--parallel). Defaults to 1.",
				Minimum:     float64Ptr(1),
			},
			"gpu_layers": {
				Type:        "integer",
				Description: "Layers to offload to the GPU (--n-gpu-layers). Use 999 for \"all\".",
				Minimum:     float64Ptr(0),
			},
			"flash_attn": {
				Type:        "boolean",
				Description: "Enable Flash Attention (--flash-attn).",
			},
			"cache_type_k": {
				Type:        "string",
				Description: "KV cache quantization for K (--cache-type-k). Typical values: q8_0, q4_0.",
			},
			"cache_type_v": {
				Type:        "string",
				Description: "KV cache quantization for V (--cache-type-v). Typical values: q8_0, q4_0.",
			},
			"jinja": {
				Type:        "boolean",
				Description: "Use the model's Jinja chat template (--jinja).",
			},
			"extra_args": {
				Type:        "array",
				Description: "Extra positional flags forwarded to llama-server, after vibe-managed flags. Each entry is one argv token.",
				Items:       &schemaProperty{Type: "string"},
			},
			"mmproj": {
				Type:        "string",
				Description: "Path to the multimodal projector GGUF that llama-server loads via --mmproj to enable image input on vision-capable models (Gemma 3, Qwen2.5-VL, LLaVA, etc.). When huggingface.mmproj_file is set, this path is the target for the pulled file.",
			},
			"binary": {
				Type:        "string",
				Description: "Override the llama-server binary used by this profile. Empty (default) uses the daemon's configured binary (typically ~/.local/bin/llama-server).",
			},
		},
	}

	comfyui := &schemaProperty{
		Type:                 "object",
		Description:          "ComfyUI Python entrypoint backend. Supervises a ComfyUI process and surfaces its address via the proxy; ComfyUI manages its own model assets (vibe does NOT pull weights for it).",
		AdditionalProperties: false,
		Required:             []string{"dir"},
		Properties: map[string]*schemaProperty{
			"dir": {
				Type:        "string",
				Description: "ComfyUI checkout path (must contain main.py). Supports ~/ expansion.",
			},
			"python": {
				Type:        "string",
				Description: "Python interpreter to invoke. Defaults to \"python3\" on $PATH.",
			},
			"listen": {
				Type:        "string",
				Description: "Address ComfyUI binds. Defaults to 127.0.0.1.",
			},
			"port": {
				Type:        "integer",
				Description: "Port ComfyUI listens on. Defaults to 8188; 0 picks a random port.",
				Minimum:     float64Ptr(0),
			},
			"extra_args": {
				Type:        "array",
				Description: "Extra positional flags forwarded to `python main.py`.",
				Items:       &schemaProperty{Type: "string"},
			},
		},
	}

	httpServer := &schemaProperty{
		Type:                 "object",
		Description:          "Generic HTTP-server backend. Supervises a docker container (image set) or bare binary (binary set) exposing an HTTP endpoint, polls health, and proxies traffic through vibe's reverse proxy. Designed for engines vibe doesn't have a first-class backend type for (TTS servers, embedding daemons, third-party inference). Docker mode and binary mode are mutually exclusive.",
		AdditionalProperties: false,
		Required:             []string{"port"},
		Properties: map[string]*schemaProperty{
			"image": {
				Type:        "string",
				Description: "Docker image reference (e.g. \"ghcr.io/remsky/kokoro-fastapi-gpu:latest\"). Mutually exclusive with binary.",
			},
			"container_port": {
				Type:        "integer",
				Description: "Port exposed inside the container. Defaults to port.",
				Minimum:     float64Ptr(0),
			},
			"volumes": {
				Type:        "array",
				Description: "host:container[:ro] mount mappings (docker mode only). Host paths are tilde-expanded.",
				Items:       &schemaProperty{Type: "string"},
			},
			"gpu": {
				Type:        "boolean",
				Description: "Pass --gpus all to docker (docker mode only). Requires NVIDIA container toolkit.",
			},
			"binary": {
				Type:        "string",
				Description: "Path to a binary serving HTTP. Mutually exclusive with image.",
			},
			"port": {
				Type:        "integer",
				Description: "Host TCP port the daemon proxies to. Required (> 0).",
				Minimum:     float64Ptr(1),
			},
			"args": {
				Type:        "array",
				Description: "Argv passed to binary (binary mode) or appended after the image name as the container's command override (docker mode).",
				Items:       &schemaProperty{Type: "string"},
			},
			"env": {
				Type:                 "object",
				Description:          "KEY=VALUE pairs forwarded as the process env (binary mode) or as `docker run -e` flags (docker mode).",
				AdditionalProperties: &schemaProperty{Type: "string"},
			},
			"health_path": {
				Type:        "string",
				Description: "Path appended to http://127.0.0.1:port for the readiness check. Defaults to /health.",
			},
		},
	}

	huggingfaceRepo := &schemaProperty{
		Type:                 "object",
		Description:          "HuggingFace snapshot pull spec. When set, vibe downloads the full EXL3 model directory (safetensors shards + tokenizer + config) into model_dir on demand.",
		AdditionalProperties: false,
		Required:             []string{"repo"},
		Properties: map[string]*schemaProperty{
			"repo": {
				Type:        "string",
				Description: "HuggingFace repository holding the EXL3 snapshot.",
			},
			"revision": {
				Type:        "string",
				Description: "Git revision (branch / tag / commit). Defaults to \"main\".",
			},
		},
	}

	tabbyAPI := &schemaProperty{
		Type:                 "object",
		Description:          "tabbyAPI (ExLlamaV3 / EXL3) backend. Supervises a tabbyAPI process serving an EXL3-format model on NVIDIA hardware and proxies its OpenAI-compatible endpoint through vibe's reverse proxy.",
		AdditionalProperties: false,
		Required:             []string{"alias", "context", "port", "venv", "repo"},
		Properties: map[string]*schemaProperty{
			"model_dir": {
				Type:        "string",
				Description: "Path to the EXL3 model directory (safetensors shards + config.json + tokenizer files). Required unless huggingface is set; tilde-expanded. Its basename must equal alias.",
			},
			"huggingface": huggingfaceRepo,
			"alias": {
				Type:        "string",
				Description: "Model id reported on /v1/models and sent as `model:` in completion requests. Must equal basename(model_dir).",
			},
			"context": {
				Type:        "integer",
				Description: "Max sequence length tabbyAPI loads the model with.",
				Minimum:     float64Ptr(1),
			},
			"port": {
				Type:        "integer",
				Description: "Host TCP port tabbyAPI publishes on. Required (> 0); daemon-picked ports are not supported for this backend.",
				Minimum:     float64Ptr(1),
			},
			"cache_mode": {
				Type:        "string",
				Enum:        []any{"FP16", "Q8", "Q6", "Q4"},
				Description: "KV cache quantisation. One of FP16 (default), Q8, Q6, Q4.",
			},
			"venv": {
				Type:        "string",
				Description: "Python venv with exllamav3 + tabbyAPI installed. The daemon execs <venv>/bin/python directly. Tilde-expanded.",
			},
			"repo": {
				Type:        "string",
				Description: "tabbyAPI checkout used as workdir + entrypoint source (start.py). Tilde-expanded.",
			},
			"draft_model_dir": {
				Type:        "string",
				Description: "Optional smaller EXL3 model dir used for speculative decoding. Tilde-expanded.",
			},
			"extra_args": {
				Type:        "array",
				Description: "Extra flags appended to the start.py argv after vibe-managed flags. Each entry is one argv token.",
				Items:       &schemaProperty{Type: "string"},
			},
		},
	}

	backend := &schemaProperty{
		Type:                 "object",
		Description:          "Backend configuration. Exactly one of llama_server, comfyui, http_server, or tabby_api must be set (discriminated union).",
		AdditionalProperties: false,
		Properties: map[string]*schemaProperty{
			"llama_server": llamaServer,
			"comfyui":      comfyui,
			"http_server":  httpServer,
			"tabby_api":    tabbyAPI,
		},
		// Discriminated-union XOR: exactly one sub-block. Each oneOf
		// branch requires one of the four keys; the loader rejects
		// "more than one set" and "none set" at runtime, matching this
		// constraint.
		OneOf: []*schemaProperty{
			{Required: []string{"llama_server"}},
			{Required: []string{"comfyui"}},
			{Required: []string{"http_server"}},
			{Required: []string{"tabby_api"}},
		},
	}

	waitForURL := &schemaProperty{
		Type:                 "object",
		Description:          "Health-check endpoint polled after `docker compose up -d` until it returns 2xx or times out.",
		AdditionalProperties: false,
		Required:             []string{"url"},
		Properties: map[string]*schemaProperty{
			"url": {
				Type:        "string",
				Description: "HTTP(S) URL to probe.",
			},
			"timeout": {
				Type:        "string",
				Description: "Go time.ParseDuration form (e.g. \"60s\", \"2m\"). Defaults to 60s.",
				Pattern:     `^[0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h)$`,
			},
		},
	}

	frontend := &schemaProperty{
		Type:        "object",
		Description: "Frontend configuration. Required for llama-server backends; rejected for comfyui (which ships its own UI). Per-kind constraints — write_file for external, compose_file for docker-compose, binary for managed — are enforced by the runtime validator with field-level error messages, not by the schema's structure.",
		// Frontend allows unknown fields because the user may carry the
		// legacy `app:` cosmetic field on older profiles — the loader's
		// inline-map catches it and silently drops it. Setting
		// additionalProperties: false here would surface a false-positive
		// editor warning on those YAMLs.
		AdditionalProperties: true,
		Properties: map[string]*schemaProperty{
			"kind": {
				Type:        "string",
				Enum:        frontendKindEnum,
				Description: "Frontend driver: external (config file rendered, user launches their binary), docker-compose (vibe runs `docker compose up`), or managed (vibe spawns and supervises a native binary).",
			},
			"restart_required": {
				Type:        "boolean",
				Description: "When true, vibe tells the user the frontend must be relaunched to pick up the new endpoint. Set for tools that read their config once at startup (opencode, etc.).",
			},
			"write_file": {
				Type:        "string",
				Description: "Path to render the templated config file to. Required for kind=external; optional for kind=managed (when set, the rendered file is exported to the binary via env). ${VIBE_STATE_DIR} expands to $XDG_STATE_HOME/vibe.",
			},
			"template": {
				Type:                 "object",
				Description:          "YAML/JSON template that's rendered to write_file with ${VAR} substitution. Required for kind=external.",
				AdditionalProperties: true,
			},
			"env": {
				Type:                 "object",
				Description:          "Env vars exported to the frontend (managed) or surfaced to the user (external). Values support ${VAR} expansion.",
				AdditionalProperties: &schemaProperty{Type: "string"},
			},
			"mcps": {
				Type:        "array",
				Description: "MCP server names (lookup keys under $XDG_CONFIG_HOME/vibe/mcp/<name>.yaml) merged into the rendered template. Only valid for kind=external.",
				Items:       &schemaProperty{Type: "string"},
			},
			"url": {
				Type:        "string",
				Description: "Browser-pointable URL surfaced in the `vibe start` summary. Optional; defaults to the wait_for entry with a \"/\" path when set.",
			},
			"compose_file": {
				Type:        "string",
				Description: "Path to the docker-compose.yaml that defines the frontend stack. Required for kind=docker-compose.",
			},
			"project_name": {
				Type:        "string",
				Description: "Compose project name (-p flag). Optional; defaults to \"vibe-<profile-name>\".",
			},
			"services": {
				Type:        "array",
				Description: "When set, vibe only `up -d`s these services. When unset, the entire compose project comes up. Only valid for kind=docker-compose.",
				Items:       &schemaProperty{Type: "string"},
			},
			"wait_for": {
				Type:        "array",
				Description: "URLs polled after `up -d` until they return 2xx. Only valid for kind=docker-compose.",
				Items:       waitForURL,
			},
			"binary": {
				Type:        "string",
				Description: "Executable to launch. Required for kind=managed; must exist and be executable. Supports ~/ expansion.",
			},
			"args": {
				Type:        "array",
				Description: "Argv passed to binary (each token a separate entry). Only valid for kind=managed.",
				Items:       &schemaProperty{Type: "string"},
			},
			"workdir": {
				Type:        "string",
				Description: "Working directory for the managed binary. Optional. Supports ~/ expansion.",
			},
		},
	}

	root := &schemaProperty{
		Schema:               JSONSchemaDraft,
		ID:                   "https://github.com/gallowaysoftware/vibe/schemas/vibe.profile.schema.json",
		Title:                "vibe profile",
		Type:                 "object",
		Description:          "A vibe profile definition: a named backend (llama_server, comfyui, http_server, or tabby_api) plus an optional frontend renderer.",
		Required:             []string{"name", "backend"},
		AdditionalProperties: false,
		Properties: map[string]*schemaProperty{
			"name": {
				Type:        "string",
				Pattern:     `^[a-zA-Z0-9_-]+$`,
				Description: "Profile name; identifier-like, used as the on-disk filename stem and the lookup key for `vibe start`.",
			},
			"description": {
				Type:        "string",
				Description: "Free-form human description shown by `vibe list`.",
			},
			"backend":  backend,
			"frontend": frontend,
			"estimated_vram_gb": {
				Type:        "number",
				Description: "Approximate VRAM the loaded model needs, in GiB. Used by the daemon's pre-flight VRAM check.",
				Minimum:     float64Ptr(0),
			},
		},
		Definitions: map[string]*schemaProperty{
			"Backend":            backend,
			"LlamaServerBackend": llamaServer,
			"ComfyUIBackend":     comfyui,
			"HTTPServerBackend":  httpServer,
			"TabbyAPIBackend":    tabbyAPI,
			"Frontend":           frontend,
			"Huggingface":        huggingface,
			"HuggingfaceRepo":    huggingfaceRepo,
			"WaitForURL":         waitForURL,
		},
	}
	return root
}

// SchemaJSON marshals Schema() to indented JSON (two-space indent). The
// indent matches the convention of every other JSON artifact vibe writes
// and keeps diffs reviewable when the schema is checked into a repo.
func SchemaJSON() ([]byte, error) {
	s := Schema()
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	// Trailing newline so the file is POSIX-clean.
	return append(out, '\n'), nil
}

// float64Ptr is a one-liner used by the schema builder when emitting
// numeric "minimum" constraints; JSON Schema requires absent vs zero to
// be distinguishable, so we model the field as a pointer.
func float64Ptr(v float64) *float64 { return &v }
