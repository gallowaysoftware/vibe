package vamp

import (
	"encoding/json"
	"fmt"
)

// JSONSchemaDraft is the $schema URL we emit on the root of the generated
// document. Draft-07 is what yaml-language-server speaks fluently today and
// what most schema-aware editor extensions (VS Code's RedHat YAML, IntelliJ,
// Helix, etc.) target by default; bumping to a later draft buys nothing
// useful for the validation shapes the pipeline schema actually expresses.
const JSONSchemaDraft = "http://json-schema.org/draft-07/schema#"

// schemaProperty is the minimal subset of JSON Schema we model by hand for
// pipeline.yaml. It carries every keyword the generated schema needs —
// `type`, `enum`, `description`, `pattern`, `items`, `properties`,
// `required`, `additionalProperties`, `oneOf`, `anyOf`, and `$ref` — and
// nothing else; new keywords get a new field. We deliberately avoid the
// gojsonschema / kin-openapi libraries: the schema's shape is fixed by the
// Stage/Pipeline Go types and pinning it to a hand-built struct makes the
// emitted output diff-stable across Go upgrades.
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

// Schema returns the JSON Schema (draft-07) describing pipeline.yaml as a
// hand-rolled Go struct ready for JSON marshaling. The shape mirrors the
// Pipeline / Stage types in pipeline.go; the test in schema_test.go round-
// trips the generated schema and re-validates the example pipelines against
// it so the two stay in sync.
func Schema() *schemaProperty {
	stageTypeEnum := []any{"", "text", "comfyui", "audio", "ffmpeg", "youtube", "webhook", "confirm", "render", "compact"}
	// runWhenEnum captures the keyword forms only; template-form run_when
	// values can be any Go text/template expression and are validated at
	// LoadPipeline time. Leaving the field as `type: string` (no enum
	// constraint) on the actual schema property accommodates both shapes
	// while keeping the keyword set discoverable for editor autocomplete.
	runWhenEnum := []any{"", RunWhenSuccess, RunWhenFailure, RunWhenAlways}
	_ = runWhenEnum
	httpMethodEnum := []any{"", "GET", "POST", "PUT", "PATCH", "DELETE"}
	privacyEnum := []any{"", "private", "unlisted", "public"}
	retryOnEnum := []any{retryOnTransient, retryOnTimeout}
	outputFormatEnum := []any{"", "json"}

	inputSpec := &schemaProperty{
		Type:                 "object",
		Description:          "Pipeline-level input passed on the CLI via --input.",
		AdditionalProperties: false,
		Properties: map[string]*schemaProperty{
			"type": {
				Type:        "string",
				Description: "Input value type. Phase 1 only supports \"string\".",
				Enum:        []any{"", "string"},
			},
			"required": {
				Type:        "boolean",
				Description: "When true, the runner errors if the user doesn't pass --input <name>=...",
			},
			"default": {
				Type:        "string",
				Description: "Default value applied when --input <name> is omitted.",
			},
		},
	}

	foreachSpec := &schemaProperty{
		Type:                 "object",
		Description:          "Fan-out descriptor: run this stage once per item of an upstream JSON-array output.",
		AdditionalProperties: false,
		Required:             []string{"from"},
		Properties: map[string]*schemaProperty{
			"from": {
				Type:        "string",
				Description: "ID of the upstream stage whose JSON-array output drives the fan-out.",
			},
			"var": {
				Type:        "string",
				Description: "Template variable name bound to each item (defaults to \"item\").",
			},
		},
	}

	durationSchema := &schemaProperty{
		Type:        "string",
		Description: "Go time.ParseDuration form, e.g. \"500ms\", \"5s\", \"1m\".",
		Pattern:     `^[0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h)$`,
	}

	retryPolicy := &schemaProperty{
		Type:                 "object",
		Description:          "Per-stage retry-with-exponential-backoff policy for transient executor failures.",
		AdditionalProperties: false,
		Properties: map[string]*schemaProperty{
			"max_attempts": {
				Type:        "integer",
				Description: "Maximum number of attempts before giving up (>= 1).",
				Minimum:     float64Ptr(1),
			},
			"initial_backoff": durationSchema,
			"max_backoff":     durationSchema,
			"multiplier": {
				Type:        "number",
				Description: "Backoff growth factor per attempt (>= 1.0).",
				Minimum:     float64Ptr(1),
			},
			"retry_on": {
				Type:        "array",
				Description: "Error classes to retry on.",
				Items: &schemaProperty{
					Type: "string",
					Enum: anySliceFromStrings([]string{retryOnTransient, retryOnTimeout}),
				},
			},
		},
	}
	// retryOnEnum is referenced via items.enum above; keep the symbol used to
	// avoid an unused-variable warning if a future refactor inlines it.
	_ = retryOnEnum

	stage := &schemaProperty{
		Type:                 "object",
		Description:          "A single stage in the pipeline DAG.",
		AdditionalProperties: false,
		Required:             []string{"id", "output"},
		Properties: map[string]*schemaProperty{
			"id": {
				Type:        "string",
				Pattern:     `^[a-zA-Z_][a-zA-Z0-9_]*$`,
				Description: "Stage id; template-syntax-safe identifier.",
			},
			"type": {
				Type:        "string",
				Enum:        stageTypeEnum,
				Description: "Stage executor type (empty defaults to \"text\").",
			},
			"capability": {
				Type:        "string",
				Description: "Capability name resolved to a vibe profile via capabilities.yaml.",
			},
			"prompt": {
				Type:        "string",
				Description: "Inline prompt template (text stages only). Mutually exclusive with prompt_file.",
			},
			"prompt_file": {
				Type:        "string",
				Description: "Path to a prompt template file, relative to the pipeline YAML's directory.",
			},
			"inputs": {
				Type:        "array",
				Description: "IDs of prior stages this stage depends on.",
				Items:       &schemaProperty{Type: "string"},
			},
			"output": {
				Type:        "string",
				Description: "Templated output path, relative to the run dir.",
			},
			"output_format": {
				Type:        "string",
				Enum:        outputFormatEnum,
				Description: "When \"json\", the runner validates the stage output parses as JSON.",
			},
			"params": {
				Type:                 "object",
				Description:          "Extra fields merged into the chat-completion body (text stages only).",
				AdditionalProperties: true,
			},
			"voice": {
				Type:        "string",
				Description: "Piper voice model name (audio stages).",
			},
			"text": {
				Type:        "string",
				Description: "Templated text fed to Piper on stdin (audio stages).",
			},
			"voices_dir": {
				Type:        "string",
				Description: "Override directory containing <voice>.onnx model files (audio stages).",
			},
			"binary": {
				Type:        "string",
				Description: "Override the path to the piper or ffmpeg binary on $PATH.",
			},
			"foreach": foreachSpec,
			"workflow": {
				Type:        "string",
				Description: "Path to a ComfyUI workflow JSON, relative to the pipeline YAML (comfyui stages).",
			},
			"parameters": {
				Type:        "object",
				Description: "Map of \"<node_id>.<input_name>\" -> templated value for comfyui workflows.",
				AdditionalProperties: &schemaProperty{
					Type: "string",
				},
			},
			"ffmpeg_args": {
				Type:        "array",
				Description: "Literal argv passed to ffmpeg (each entry template-rendered).",
				Items:       &schemaProperty{Type: "string"},
			},
			"concat_wavs_dir": {
				Type:        "string",
				Description: "Subdirectory (template-rendered, relative to the run dir) to scope the *.wav walk when concat_wavs is true. Defaults to the whole run dir.",
			},
			"source": {
				Type:        "string",
				Description: "Template rendering the text to compact (compact stages). Typically `{{ .stages.<upstream>.output }}`.",
			},
			"target_chars": {
				Type:        "integer",
				Description: "Approximate ceiling on the compacted output's length in characters (compact stages). Must be > 0.",
				Minimum:     float64Ptr(1),
			},
			"chunk_chars": {
				Type:        "integer",
				Description: "Max characters of source sent per LLM call (compact stages). 0 = pick automatically (target_chars × 4).",
				Minimum:     float64Ptr(0),
			},
			"preserve": {
				Type:        "string",
				Description: "Free-form directive describing what MUST be preserved in the compaction (compact stages). E.g. `every numerical value, equipment name, process step`.",
			},
			"video": {
				Type:        "string",
				Description: "Templated path to the MP4 to upload (youtube stages).",
			},
			"title": {
				Type:        "string",
				Description: "Templated video title (youtube stages).",
			},
			"description": {
				Type:        "string",
				Description: "Templated video description (youtube stages).",
			},
			"tags": {
				Type:        "array",
				Description: "Fixed string list of YouTube tags.",
				Items:       &schemaProperty{Type: "string"},
			},
			"privacy": {
				Type:        "string",
				Enum:        privacyEnum,
				Description: "YouTube visibility: private, unlisted, or public.",
			},
			"category_id": {
				Type:        "string",
				Description: "YouTube numeric category id (default \"22\").",
			},
			"thumbnail": {
				Type:        "string",
				Description: "Optional template path to a thumbnail image (youtube stages).",
			},
			"credentials_file": {
				Type:        "string",
				Description: "OAuth refresh-token JSON path (youtube stages).",
			},
			"url": {
				Type:        "string",
				Description: "Templated webhook destination URL.",
			},
			"method": {
				Type:        "string",
				Enum:        httpMethodEnum,
				Description: "HTTP method (default POST).",
			},
			"body": {
				Type:                 "object",
				Description:          "Templated JSON body (webhook stages). Mutually exclusive with body_template_file.",
				AdditionalProperties: true,
			},
			"body_template_file": {
				Type:        "string",
				Description: "Path to a body template file; rendered verbatim and sent as the request body.",
			},
			"headers": {
				Type:        "object",
				Description: "Flat map of header name -> templated value (webhook stages).",
				AdditionalProperties: &schemaProperty{
					Type: "string",
				},
			},
			"retry_on_transient": {
				Type:        "boolean",
				Description: "When true (the default) HTTP 429 (rate-limited) and HTTP 5xx responses trigger the transient-error retry path with exponential backoff. Replaces retry_on_5xx; mutually exclusive with that deprecated alias.",
			},
			"retry_on_5xx": {
				Type:        "boolean",
				Description: "Deprecated alias for retry_on_transient. When true (the default) a 5xx response triggers the transient-error retry path. New pipelines should use retry_on_transient — it covers HTTP 429 too.",
			},
			"assert": {
				Type:                 "object",
				Description:          "Optional response checks (webhook stages). Turns a webhook into a smoke-test probe: status_code, body_contains, body_not_contains, min_body_length are all validated against the response. Setting status_code overrides the default 2xx-required behaviour so tests can confirm a 401/4xx.",
				AdditionalProperties: false,
				Properties: map[string]*schemaProperty{
					"status_code": {
						Type:        "integer",
						Description: "Exact HTTP status code required. Overrides the default 2xx check.",
					},
					"body_contains": {
						Type:        "array",
						Description: "Substrings that ALL must appear in the response body.",
						Items:       &schemaProperty{Type: "string"},
					},
					"body_not_contains": {
						Type:        "array",
						Description: "Substrings that must NOT appear in the response body.",
						Items:       &schemaProperty{Type: "string"},
					},
					"min_body_length": {
						Type:        "integer",
						Description: "Minimum response body length in bytes. Catches 200 + empty body.",
					},
				},
			},
			"retry": retryPolicy,
			"run_when": {
				Type:        "string",
				Description: "Gate the stage. Reserved keywords success (default) / failure / always inspect upstream status; any other value is a Go text/template expression that must render to one of true/yes/1 (run) or false/no/0/\"\" (skip).",
				Default:     RunWhenSuccess,
			},
			"cache": {
				Type:        "boolean",
				Description: "Per-stage opt-out: explicit false disables the content-addressed cache for this stage; absent inherits the pipeline default.",
			},
			"message": {
				Type:        "string",
				Description: "Approval prompt rendered to the operator (confirm stages).",
			},
			"timeout": durationSchema,
			"cleanup": {
				Type:        "array",
				Description: "Glob patterns (relative to the run dir) removed from disk after the stage's success path completes. Best-effort: cleanup failures log a warning and never fail the stage. Patterns must NOT escape the run dir (no absolute paths, no \"..\" segments). Only valid on stage types that produce a stable on-disk output (text/comfyui/audio/ffmpeg).",
				Items:       &schemaProperty{Type: "string"},
			},
		},
	}

	pipeline := &schemaProperty{
		Schema:               JSONSchemaDraft,
		ID:                   "https://github.com/gallowaysoftware/vibe/schemas/vamp.pipeline.schema.json",
		Title:                "vamp pipeline",
		Type:                 "object",
		Description:          "A vamp pipeline definition: name, optional inputs, and a list of stages forming a DAG.",
		Required:             []string{"name", "stages"},
		AdditionalProperties: false,
		Properties: map[string]*schemaProperty{
			"name": {
				Type:        "string",
				Pattern:     `^[a-zA-Z0-9_-]+$`,
				Description: "Pipeline name; identifier-like, used in run dir names and pipeline.json.",
			},
			"description": {
				Type:        "string",
				Description: "Free-form human description.",
			},
			"inputs": {
				Type:                 "object",
				Description:          "Map of pipeline-level input name -> spec, passed on the CLI as --input.",
				AdditionalProperties: inputSpec,
			},
			"stages": {
				Type:        "array",
				Description: "Ordered list of stages forming the pipeline DAG.",
				Items:       stage,
			},
			"cache": {
				Type:        "boolean",
				Description: "Pipeline-level opt-out: explicit false disables the content-addressed cache for every stage in the pipeline.",
			},
		},
		Definitions: map[string]*schemaProperty{
			"InputSpec":   inputSpec,
			"Stage":       stage,
			"ForeachSpec": foreachSpec,
			"RetryPolicy": retryPolicy,
		},
	}
	return pipeline
}

// SchemaJSON marshals Schema() to indented JSON (two-space indent). The
// indent matches the convention of every other JSON artifact vamp writes
// (pipeline.json, pipeline_timing.json) and keeps diffs reviewable.
func SchemaJSON() ([]byte, error) {
	s := Schema()
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	// Trailing newline so the file is POSIX-clean.
	return append(out, '\n'), nil
}

// float64Ptr is a one-liner used by the schema builder when emitting numeric
// "minimum" constraints; JSON Schema requires absent vs zero to be
// distinguishable, so we model the field as a pointer.
func float64Ptr(v float64) *float64 { return &v }

// anySliceFromStrings converts a []string to []any for use in schemaProperty.Enum,
// which has to accept mixed-type enum values per JSON Schema.
func anySliceFromStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
