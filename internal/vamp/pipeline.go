package vamp

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// Pipeline is the parsed YAML description of a vamp pipeline.
type Pipeline struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description,omitempty"`
	Inputs      map[string]InputSpec `yaml:"inputs,omitempty"`
	Stages      []Stage              `yaml:"stages"`
	// Cache, when explicitly false, disables the content-addressed cache
	// for every stage in the pipeline. A nil pointer means "default"
	// (caching is on for cacheable stage types); the explicit-bool /
	// nil-vs-false discrimination is the reason the field is a *bool
	// rather than a plain bool. Individual stages can override the
	// pipeline default via Stage.Cache.
	Cache *bool `yaml:"cache,omitempty"`
}

// InputSpec declares a pipeline-level input passed on the CLI as --input.
type InputSpec struct {
	Type     string `yaml:"type,omitempty"` // informational; only "string" supported in Phase 1
	Required bool   `yaml:"required,omitempty"`
	Default  string `yaml:"default,omitempty"`
}

// Stage is a single step in the pipeline.
type Stage struct {
	ID           string         `yaml:"id"`
	Type         StageType      `yaml:"type,omitempty"` // "" | "text" | "comfyui" | "audio" | "ffmpeg" | "youtube" | "webhook"; empty defaults to "text"
	Capability   string         `yaml:"capability,omitempty"`
	Prompt       string         `yaml:"prompt,omitempty"`
	PromptFile   string         `yaml:"prompt_file,omitempty"`
	Inputs       []string       `yaml:"inputs,omitempty"` // ids of prior stages to depend on
	Output       string         `yaml:"output"`
	OutputFormat string         `yaml:"output_format,omitempty"` // "" | "json"
	Params       map[string]any `yaml:"params,omitempty"`        // merged into the chat-completion body (text stages only)
	// ImageDir, when non-empty on a text stage, instructs the executor to
	// read all image files from the directory (expanded relative to
	// PipelineDir), base64-encode them, and attach them as multimodal
	// content alongside the prompt. Requires a vision-capable backend
	// (Gemma 3 + mmproj) to be meaningful; a non-vision backend will
	// ignore the images. The directory is scanned for common raster
	// extensions (.png, .jpg, .jpeg, .gif, .webp, .bmp); SVGs are
	// rasterized through rsvg-convert into a content-addressed PNG cache
	// before being attached, since vision encoders consume pixels, not
	// vector markup.
	ImageDir string `yaml:"image_dir,omitempty"`

	// ImageFiles is the explicit-list alternative to ImageDir: each entry
	// is a templated path to a single image file (rendered against the
	// foreach binding so per-iteration single-image stages are
	// expressible). Use this when a multimodal pipeline needs to fan out
	// over images one-at-a-time — e.g. lessons whose 6+ diagrams blow
	// past the model's context when sent together. Mutually exclusive
	// with ImageDir.
	ImageFiles []string `yaml:"image_files,omitempty"`

	// Audio-stage fields. Audio stages shell out to a Piper TTS binary to
	// synthesize speech from a rendered text template. Voice names the voice
	// model (resolved to <VoicesDir>/<Voice>.onnx). Text is the template
	// rendered and fed to piper on stdin. VoicesDir defaults to
	// ~/.local/share/piper-voices and supports tilde expansion. Binary
	// defaults to "piper" on $PATH.
	Voice     string `yaml:"voice,omitempty"`
	Text      string `yaml:"text,omitempty"`
	VoicesDir string `yaml:"voices_dir,omitempty"`
	Binary    string `yaml:"binary,omitempty"`
	// Engine selects the TTS backend for a `type: audio` stage. Default is
	// "piper" (subprocess + .onnx voices on disk). "kokoro" routes through
	// an OpenAI-compatible HTTP endpoint (Kokoro-FastAPI) — markedly more
	// natural narration for English long-form, with deterministic output
	// (same voice + text → same WAV).
	Engine string `yaml:"engine,omitempty"`
	// EngineURL is the OpenAI-compatible TTS endpoint base, used when
	// engine: kokoro (and any future HTTP-backed engines). Defaults to
	// http://127.0.0.1:8880 (Kokoro-FastAPI's docker default). The
	// executor appends "/v1/audio/speech". Kept distinct from the URL
	// field on webhook stages so each stage type's validator can reject
	// fields that don't apply.
	EngineURL string `yaml:"engine_url,omitempty"`
	// Foreach, when non-nil, makes this a fan-out stage. The upstream stage
	// referenced by From must produce output_format: json and its output must
	// parse as a JSON array (or {"items":[...]} convenience wrap). The stage
	// then runs once per array element, in parallel, sharing the same profile
	// activation. The per-item value is bound under Foreach.Var in the
	// template namespace (defaults to "item").
	Foreach *ForeachSpec `yaml:"foreach,omitempty"`

	// ComfyUI-stage fields. Workflow is a path to a ComfyUI workflow JSON file
	// (relative to the pipeline YAML's directory). Parameters maps
	// "<node_id>.<input_name>" -> template string; each rendered value is
	// type-coerced (int/float/bool/string) and substituted into the workflow's
	// node inputs prior to submission. Capability is still required and maps
	// to the vibe profile that supervises the ComfyUI backend.
	Workflow   string            `yaml:"workflow,omitempty"`
	Parameters map[string]string `yaml:"parameters,omitempty"`

	// FFmpeg-stage fields. FFmpegArgs is the literal argv passed to ffmpeg
	// (in order) after the binary; each entry is rendered as a Go template
	// against the standard binding (inputs/stages/runDir + foreach item)
	// before invocation. The executor appends `-y <rendered output>` after
	// the user-supplied args so users don't manage the destination path
	// themselves. Binary (shared with audio) defaults to "ffmpeg" on $PATH.
	FFmpegArgs []string `yaml:"ffmpeg_args,omitempty"`
	// ConcatWavs, when true on an ffmpeg stage, auto-globs all "*.wav" files
	// in the run dir, creates a concat file list, and runs ffmpeg to merge
	// them into the output MP3. The stage's ffmpeg_args are ignored when this
	// is true; the executor constructs the argv internally. Designed for
	// concatenating foreach audio stage outputs into a single podcast file.
	ConcatWavs bool `yaml:"concat_wavs,omitempty"`
	// ConcatWavsDir, when non-empty on a concat_wavs ffmpeg stage, scopes
	// the WAV walk to a specific subdirectory of the run dir instead of
	// the whole run dir. Template-rendered so a foreach mp3 stage can
	// concat per-item subdirectories (e.g. `audio/{{.unit.unit_id}}` —
	// one MP3 per unit when generate_script emits unit-tagged segments
	// to per-unit audio subdirs). Path must resolve to a path relative
	// to the run dir; absolute paths are rejected to keep cleanup +
	// resume sane.
	ConcatWavsDir string `yaml:"concat_wavs_dir,omitempty"`
	// Pandoc-stage fields. SourceFile is the template rendering the input
	// file path (relative to run dir or absolute). PandocFrom / PandocTo
	// are the --from / --to format flags. PandocMetadata is a map of
	// pandoc --metadata key=value pairs (title/author/date). PandocArgs
	// is appended raw after the generated args.
	SourceFile     string            `yaml:"source_file,omitempty"`
	PandocFrom     string            `yaml:"pandoc_from,omitempty"`
	PandocTo       string            `yaml:"pandoc_to,omitempty"`
	PandocMetadata map[string]string `yaml:"pandoc_metadata,omitempty"`
	PandocArgs     []string          `yaml:"pandoc_args,omitempty"`
	// CoverImage is an optional template rendering a path to a PNG/JPEG
	// cover image. Embedded as audiobook art when set on an M4B ffmpeg
	// stage, as --epub-cover-image when set on a pandoc EPUB stage.
	CoverImage string `yaml:"cover_image,omitempty"`

	// M4B* fields, when set together on a `type: ffmpeg` stage, switch the
	// executor into "build a chapterized M4B audiobook" mode. Designed to
	// take an upstream JSON-array stage (typically the dedup'd parent list
	// from a unique_parents render stage), look up the corresponding MP3
	// file per item, ffprobe each for duration, and concat them into one
	// AAC-encoded .m4b with embedded chapter metadata. Apple Books, every
	// audiobook player, and Audiobookshelf all read the resulting file
	// directly — proper chapter navigation, position sync, speed control.
	//
	// All four fields are required as a set; setting one without the
	// others is a validation error. Mutually exclusive with concat_wavs
	// + ffmpeg_args.
	//
	// M4BFrom names the upstream stage whose JSON-array output drives
	// the chapter list (and chapter ORDER — the executor preserves the
	// upstream's emit order, NOT lexical filename sort, so a unit
	// list like [cereals, cereal_wort_production, ...] stays in that
	// natural order rather than going alphabetical).
	M4BFrom string `yaml:"m4b_from,omitempty"`
	// M4BVar is the template variable name bound to each upstream item
	// while rendering M4BFile and M4BChapter. Defaults to "item".
	M4BVar string `yaml:"m4b_var,omitempty"`
	// M4BFile is a template rendered per upstream item to the absolute
	// or run-dir-relative path of the MP3 file for that chapter. Each
	// rendered file must exist when the stage runs.
	M4BFile string `yaml:"m4b_file,omitempty"`
	// M4BChapter is a template rendered per upstream item to the human-
	// readable chapter title embedded in the M4B metadata. Shows up as
	// the chapter name in Apple Books / Audiobookshelf / VLC.
	M4BChapter string `yaml:"m4b_chapter,omitempty"`

	// YouTube-stage fields. Video is a template path to the MP4 to upload
	// (typically the output of an upstream ffmpeg stage). Title/Description
	// are template-rendered metadata. Tags is a fixed string list (optional).
	// Privacy is one of "private" (default), "unlisted", "public".
	// CategoryID is YouTube's numeric category id (default "22", People &
	// Blogs). Thumbnail is an optional template path to an image uploaded
	// after the video. CredentialsFile is an OAuth refresh-token JSON;
	// defaults to ~/.config/vamp/youtube-credentials.json. The executor
	// records the resulting watch URL as the stage output.
	Video           string   `yaml:"video,omitempty"`
	Title           string   `yaml:"title,omitempty"`
	Description     string   `yaml:"description,omitempty"`
	Tags            []string `yaml:"tags,omitempty"`
	Privacy         string   `yaml:"privacy,omitempty"`
	CategoryID      string   `yaml:"category_id,omitempty"`
	Thumbnail       string   `yaml:"thumbnail,omitempty"`
	CredentialsFile string   `yaml:"credentials_file,omitempty"`

	// Webhook-stage fields. URL is a template-rendered destination URL the
	// executor POSTs to (Slack/Discord/Mattermost-compatible incoming webhook
	// shape). Method overrides the HTTP method (default POST). Body, when
	// non-nil, is a template-rendered map that the executor marshals to JSON;
	// each leaf string value is rendered as a Go template against the
	// standard binding. BodyTemplateFile, when non-empty, names a file
	// (resolved relative to PipelineDir) whose contents are rendered as a
	// single template and sent verbatim as the request body — mutually
	// exclusive with Body. Headers is a flat map of header name -> templated
	// value.
	//
	// RetryOnTransient defaults to true and is the modern, honestly-named
	// switch for synthesising a transient-error retry policy. Transient now
	// covers BOTH HTTP 5xx AND HTTP 429 (rate limited) — the latter being the
	// shape real-world search/LLM APIs return under burst load. When set
	// together with a stage-level retry: block of its own the stage's explicit
	// policy wins.
	//
	// RetryOn5xx is the deprecated alias kept for back-compat: pipelines that
	// already set retry_on_5xx continue to work unchanged (the loader emits a
	// one-shot deprecation warning when it sees the field). Setting both
	// names at once is rejected at validate time so users don't end up with
	// one true and one false silently.
	URL              string            `yaml:"url,omitempty"`
	Method           string            `yaml:"method,omitempty"`
	Body             map[string]any    `yaml:"body,omitempty"`
	BodyTemplateFile string            `yaml:"body_template_file,omitempty"`
	Headers          map[string]string `yaml:"headers,omitempty"`
	RetryOnTransient *bool             `yaml:"retry_on_transient,omitempty"`
	RetryOn5xx       *bool             `yaml:"retry_on_5xx,omitempty"`
	// Assert, when non-nil, turns a webhook stage into a smoke-test
	// probe: the executor runs the request, then validates the response
	// against the declared expectations and fails the stage with a
	// descriptive error when any check fails. Designed for end-to-end
	// regression pipelines that exercise a stack from the outside (HTTP
	// → real services → response shape) without having to write a
	// downstream `text:` stage just to read the body and judge it.
	Assert *AssertSpec `yaml:"assert,omitempty"`

	// Retry, when non-nil, enables per-stage retry-with-exponential-backoff
	// for transient failures. The runner wraps each executor.Execute call
	// (per-item for foreach stages) in a retry loop governed by this
	// policy. Absent / nil means "no retry" — the executor's first error
	// is returned verbatim, matching pre-retry behaviour.
	Retry *RetryPolicy `yaml:"retry,omitempty"`

	// Cache, when explicitly false, disables the content-addressed cache
	// for this stage even when the pipeline-level default is on. A nil
	// pointer falls through to the pipeline-level setting, which in turn
	// defaults to "on" for cacheable stage types when both layers are
	// nil. Non-cacheable stage types (webhook/youtube) are never cached
	// regardless of this field's value.
	Cache *bool `yaml:"cache,omitempty"`

	// RunWhen controls whether the stage runs based on the status of its
	// declared Inputs (and, when Inputs is empty, the pipeline as a whole).
	// Values are "success" (the default; runs only when every dep succeeded),
	// "failure" (runs only when at least one dep — or any prior stage when
	// Inputs is empty — errored), or "always" (runs regardless of prior
	// status, e.g. cleanup hooks). For "failure" and "always" stages the
	// template namespace gains {{ .pipeline_status }} and
	// {{ .failure_summary }} bindings.
	//
	// Anything other than the three keywords is treated as a Go text/template
	// expression that renders against the same namespace as a stage's prompt
	// template (inputs/stages/runDir + pipeline_status/failure_summary). The
	// rendered output is trimmed and lowercased; "true"/"yes"/"1" → run,
	// "false"/"no"/"0"/"" → skip, anything else → pipeline error at runtime.
	// A template-form run_when is treated as an implicit "success" keyword
	// for upstream-status gating: the template fires only after the deps
	// have all succeeded.
	RunWhen string `yaml:"run_when,omitempty"`

	// Compact-stage fields. A `type: compact` stage replaces the brittle
	// `| truncate N` template pattern: when an upstream's text would
	// overflow a downstream LLM's context window, route it through a
	// compact stage instead so an LLM call produces a summary that fits,
	// rather than silently dropping the tail of the content.
	//
	// Source is the template that renders the text to compress (typically
	// `{{ .stages.<upstream>.output }}`).
	//
	// TargetChars is the approximate ceiling on the compacted output's
	// length in characters. The executor recursively re-compacts when a
	// first pass still overshoots.
	//
	// ChunkChars caps how much of Source is sent to the model per LLM
	// call. Default is TargetChars * 4 (lets a single chunk fit a
	// 131K-context model comfortably with room for output). When Source
	// is shorter than ChunkChars, exactly one LLM call runs.
	//
	// Preserve is a free-form directive describing what MUST be retained
	// across the compaction (e.g. "every numerical value, temperature,
	// pH, equipment name"). It is interpolated into the built-in
	// compaction prompt template.
	Source      string `yaml:"source,omitempty"`
	TargetChars int    `yaml:"target_chars,omitempty"`
	ChunkChars  int    `yaml:"chunk_chars,omitempty"`
	Preserve    string `yaml:"preserve,omitempty"`

	// Message is the rendered prompt a `type: confirm` stage prints to the
	// operator (and writes into the marker file) when asking for approval.
	// Required for confirm stages; rejected on every other stage type.
	Message string `yaml:"message,omitempty"`

	// Timeout, when non-zero, bounds how long a `type: confirm` stage will
	// wait for the operator to respond before auto-rejecting. Zero (the
	// default) means "wait forever". Rejected on every other stage type.
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// Cleanup is an optional list of glob patterns (relative to the run
	// dir) that the runner removes from disk after the stage's success
	// path completes. Designed for stages that consume large intermediate
	// files (e.g. an ffmpeg stage that concatenates a directory of wavs
	// into a single mp3) and want to free that disk after producing their
	// own output. Patterns must NOT escape the run dir: `..` segments and
	// absolute paths are rejected at load time. Cleanup runs ONLY on
	// success (not on error, not on cancel, not on cache-hit/resume
	// skip), and is best-effort — failures log a warning and continue.
	// Only valid on stage types that produce a stable on-disk output
	// (text/comfyui/audio/ffmpeg); rejected on webhook/youtube/confirm.
	Cleanup []string `yaml:"cleanup,omitempty"`

	// AssetFS, when non-nil, replaces PipelineDir as the root for resolving
	// PromptFile / BodyTemplateFile / Workflow / similar pipeline-shipped
	// asset references on this stage. YAML pipelines leave it nil and
	// continue resolving against PipelineDir on disk; Go-DSL pipelines set
	// it via the *Stage builder's PromptFS / BodyTemplateFS / WorkflowFS
	// methods so the user can ship a pipeline's templates inside an
	// embed.FS (or any other fs.FS) alongside the binary. The field is
	// tagged yaml:"-" so it never round-trips through pipeline.yaml.snapshot
	// or any other YAML serialization path.
	AssetFS fs.FS `yaml:"-"`
}

// RetryPolicy controls per-stage retry behaviour for transient executor
// failures. All fields have defaults; the zero value of *RetryPolicy
// (nil) means "no retry" (max_attempts effectively 1).
type RetryPolicy struct {
	MaxAttempts    int           `yaml:"max_attempts,omitempty"`
	InitialBackoff time.Duration `yaml:"initial_backoff,omitempty"`
	MaxBackoff     time.Duration `yaml:"max_backoff,omitempty"`
	Multiplier     float64       `yaml:"multiplier,omitempty"`
	RetryOn        []string      `yaml:"retry_on,omitempty"`
}

const (
	defaultRetryMaxAttempts    = 1
	defaultRetryInitialBackoff = time.Second
	defaultRetryMaxBackoff     = 30 * time.Second
	defaultRetryMultiplier     = 2.0
)

const (
	retryOnTransient = "transient"
	retryOnTimeout   = "timeout"
	// retryOnInvalidOutput retries when the stage returned a non-error
	// response that didn't satisfy the stage's output_format contract —
	// e.g. an LLM emitted text that fails validateJSON. A re-roll with a
	// different sample usually succeeds; not transient in the network
	// sense, but observationally identical from the pipeline's POV.
	retryOnInvalidOutput = "invalid_output"
)

// Stage.RunWhen string values. RunWhenSuccess is the default applied when
// the field is empty; the executor inspects RunWhen to decide whether a
// stage is eligible to run after its declared deps finish (regardless of
// whether they succeeded). Centralised here so pipeline.go's validation
// and exec.go's scheduler stay in sync.
const (
	RunWhenSuccess = "success"
	RunWhenFailure = "failure"
	RunWhenAlways  = "always"
)

// defaultWebhookRetryAttempts is the synthesised retry count we inject onto
// webhook stages with retry_on_5xx=true (the default) and no explicit
// retry: block. Three attempts (one initial + two retries) is the
// pragmatic ceiling: it survives a single transient blip on the receiving
// webhook ingestion endpoint without dragging out a doomed pipeline by an
// order of magnitude.
const defaultWebhookRetryAttempts = 3

// defaultAudioRetryAttempts is the synthesised retry count for audio
// stages with no explicit retry: block. Kokoro-FastAPI occasionally
// returns an empty body (model warmup races, internal scheduling) and
// piper subprocesses can flake on a tight binary path; without retry
// one bad item in a 1500+-chunk foreach kills hours of upstream work.
// Four attempts mirrors what the existing cibd-distilling pipelines
// configure by hand.
const defaultAudioRetryAttempts = 4

func (r *RetryPolicy) Normalize() {
	if r == nil {
		return
	}
	if r.MaxAttempts == 0 {
		r.MaxAttempts = defaultRetryMaxAttempts
	}
	if r.InitialBackoff == 0 {
		r.InitialBackoff = defaultRetryInitialBackoff
	}
	if r.MaxBackoff == 0 {
		r.MaxBackoff = defaultRetryMaxBackoff
	}
	if r.Multiplier == 0 {
		r.Multiplier = defaultRetryMultiplier
	}
	if len(r.RetryOn) == 0 {
		r.RetryOn = []string{retryOnTransient}
	}
}

func (r *RetryPolicy) Validate(ctx string) error {
	if r == nil {
		return nil
	}
	if r.MaxAttempts < 0 {
		return fmt.Errorf("%s: retry.max_attempts must be >= 1 (got %d)", ctx, r.MaxAttempts)
	}
	if r.InitialBackoff < 0 {
		return fmt.Errorf("%s: retry.initial_backoff must be > 0 (got %s)", ctx, r.InitialBackoff)
	}
	if r.MaxBackoff < 0 {
		return fmt.Errorf("%s: retry.max_backoff must be > 0 (got %s)", ctx, r.MaxBackoff)
	}
	if r.Multiplier != 0 && r.Multiplier < 1.0 {
		return fmt.Errorf("%s: retry.multiplier must be >= 1.0 (got %g)", ctx, r.Multiplier)
	}
	for _, mode := range r.RetryOn {
		switch mode {
		case retryOnTransient, retryOnTimeout, retryOnInvalidOutput:
		default:
			return fmt.Errorf("%s: retry.retry_on entry %q is not supported (allowed: %q, %q, %q)", ctx, mode, retryOnTransient, retryOnTimeout, retryOnInvalidOutput)
		}
	}
	return nil
}

// AssertSpec turns a webhook stage into an assertion: after the request
// completes, the executor checks the response against these expectations
// and fails the stage with a descriptive error when any check fails.
//
// The presence of any check changes the executor's default behaviour:
// when StatusCode is non-zero, only that exact status is accepted (so
// assert.status_code: 401 lets a test confirm that auth IS required).
// Body checks run after the status check on success; an empty StatusCode
// keeps the default "2xx required" behaviour.
//
// All non-zero/non-empty checks are evaluated. The error message lists
// every failed check, so a single run gives the user a full picture
// rather than failing fast on the first mismatch.
type AssertSpec struct {
	// StatusCode, when non-zero, is the exact HTTP status code that
	// must be returned. Overrides the executor's default 2xx check.
	StatusCode int `yaml:"status_code,omitempty"`
	// BodyContains is a list of substrings that must ALL appear in the
	// response body. Comparison is case-sensitive.
	BodyContains []string `yaml:"body_contains,omitempty"`
	// BodyNotContains is a list of substrings that must NOT appear in
	// the response body. Useful for negative assertions (e.g. that an
	// error string is absent).
	BodyNotContains []string `yaml:"body_not_contains,omitempty"`
	// MinBodyLength, when non-zero, requires the body to be at least
	// this many bytes — catches "endpoint returned 200 + empty body"
	// (a common silent-failure shape).
	MinBodyLength int `yaml:"min_body_length,omitempty"`
}

// ForeachSpec is the structured fan-out descriptor for a stage. The previous
// Phase 1 form was a free-form template string plus a separate foreach_as
// field; this form ties the iteration directly to a declared upstream stage,
// which keeps Phase 2 (non-LLM stages) honest about its dependencies.
type ForeachSpec struct {
	// From is the id of the upstream stage whose JSON-array output drives the
	// fan-out. Must appear in the consuming stage's Inputs list.
	From string `yaml:"from"`
	// Var is the template variable name bound to each item while rendering
	// Prompt/Output. Defaults to "item" when empty.
	Var string `yaml:"var,omitempty"`
}

var (
	pipelineNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	stageIDRE      = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// comfyParamKeyRE enforces the "<node_id>.<input_name>" shape expected for
	// every entry in a comfyui stage's parameters map. ComfyUI workflow nodes
	// are keyed by all-numeric string ids; input names are Go-identifier-like.
	comfyParamKeyRE = regexp.MustCompile(`^[0-9]+\.[A-Za-z_][A-Za-z0-9_]*$`)
)

// LoadPipeline reads, parses, and validates a pipeline YAML file. Unknown
// fields are rejected so typos surface early.
func LoadPipeline(path string) (*Pipeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var p Pipeline
	if err := dec.Decode(&p); err != nil {
		return nil, migrateForeachError(fmt.Errorf("parse %s: %w", path, err))
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &p, nil
}

// DefaultForeachVar is the template variable name bound to each item of a
// foreach stage's array when ForeachSpec.Var is unset.
const DefaultForeachVar = "item"

// ForeachNonTemplatedMultiItemErrMsg is the format string used at runtime
// when a foreach stage's upstream resolves to >1 items and Stage.Output
// contains no template marker. The runtime check (in executeForeachStage)
// supersedes the old static "{{" presence check in Validate so that
// single-item foreaches with a literal output path are accepted. We expose
// the message as a constant so the runtime call site and any future
// dry-run / linting check stay in lockstep.
const ForeachNonTemplatedMultiItemErrMsg = "%s: foreach stages require a templated output path (contains {{...}}) so per-item writes don't collide"

// migrateForeachError annotates the YAML decode error with a migration hint
// when the failure is most likely caused by a pipeline still using the old
// Phase 1 foreach syntax (string template + separate foreach_as). YAML's
// strict-field mode rejects the legacy syntax with a generic "cannot unmarshal
// !!str into vamp.ForeachSpec" / "field foreach_as not found" message;
// rewriting that to point at the new form saves users the dig.
func migrateForeachError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "foreach_as") ||
		strings.Contains(msg, "into vamp.ForeachSpec") ||
		strings.Contains(msg, "cannot unmarshal !!str into") {
		return fmt.Errorf("%w\n\nhint: the foreach syntax changed. Replace:\n  foreach: \"{{.stages.X.output}}\"\n  foreach_as: var\nwith:\n  foreach:\n    from: X\n    var: var", err)
	}
	return err
}

func (p *Pipeline) Validate() error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if !pipelineNameRE.MatchString(p.Name) {
		return fmt.Errorf("name %q must match [a-zA-Z0-9_-]+", p.Name)
	}
	if len(p.Stages) == 0 {
		return errors.New("pipeline must have at least one stage")
	}
	for name, spec := range p.Inputs {
		if !pipelineNameRE.MatchString(name) {
			return fmt.Errorf("inputs[%s]: name must match [a-zA-Z0-9_-]+", name)
		}
		if spec.Type != "" && spec.Type != "string" {
			return fmt.Errorf("inputs[%s]: type %q is not supported (only \"string\" in Phase 1)", name, spec.Type)
		}
	}
	// First pass: per-stage shape validation + duplicate-id detection. We
	// intentionally do NOT enforce the "input must reference an earlier
	// stage" rule here anymore: the DAG executor only requires the
	// dependency graph to be acyclic, so forward references are fine. A
	// dedicated cycle check below produces a clearer error than the old
	// rule, and lets pipelines declare stages in any order.
	seenStages := make(map[string]bool)
	for i, s := range p.Stages {
		loc := fmt.Sprintf("stages[%d]", i)
		if s.ID == "" {
			return fmt.Errorf("%s: id is required", loc)
		}
		if !stageIDRE.MatchString(s.ID) {
			return fmt.Errorf("%s: id %q must match [a-zA-Z_][a-zA-Z0-9_]* (template-syntax-safe)", loc, s.ID)
		}
		if seenStages[s.ID] {
			return fmt.Errorf("%s: duplicate stage id %q", loc, s.ID)
		}
		seenStages[s.ID] = true
		ctx := fmt.Sprintf("stage %s", s.ID)
		if s.Output == "" {
			return fmt.Errorf("%s: output is required", ctx)
		}
		// Type discrimination: empty defaults to text. Per-type shape rules
		// below reject fields that belong to the other type.
		stageType := s.Type
		if stageType == "" {
			stageType = StageTypeText
		}
		// Capability is required for every stage type except the
		// subprocess-only ones (audio, ffmpeg) and the network-only types
		// (youtube, webhook), which talk to a third-party API and never
		// activate a vibe profile. Confirm stages are pure local I/O —
		// stdin or a marker file — and also don't activate a profile.
		if stageType != StageTypeAudio && stageType != StageTypeFFmpeg && stageType != StageTypeYouTube && stageType != StageTypeWebhook && stageType != StageTypeConfirm && stageType != StageTypeRender && stageType != StageTypePandoc && s.Capability == "" {
			return fmt.Errorf("%s: capability is required", ctx)
		}
		switch stageType {
		case StageTypeText:
			if (s.Prompt == "") == (s.PromptFile == "") {
				return fmt.Errorf("%s: exactly one of prompt or prompt_file is required", ctx)
			}
			if s.ImageDir != "" && len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_dir and image_files are mutually exclusive", ctx)
			}
			for idx, tmpl := range s.ImageFiles {
				if tmpl == "" {
					return fmt.Errorf("%s: image_files[%d] is empty", ctx, idx)
				}
				if _, err := template.New(fmt.Sprintf("%s:image_files[%d]", s.ID, idx)).Funcs(templateFuncs()).Parse(tmpl); err != nil {
					return fmt.Errorf("%s: parse image_files[%d] template: %w", ctx, idx, err)
				}
			}
			if err := validateParamsKeys(ctx, s.Params); err != nil {
				return err
			}
			if s.Prompt != "" {
				if _, err := template.New(s.ID + ":prompt").Funcs(templateFuncs()).Parse(s.Prompt); err != nil {
					return fmt.Errorf("%s: parse prompt template: %w", ctx, err)
				}
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages (text stages use params)", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" || s.Binary != "" {
				return fmt.Errorf("%s: voice/text/voices_dir/binary are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
		case StageTypeAudio:
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if s.Voice == "" {
				return fmt.Errorf("%s: voice is required for type: audio stages", ctx)
			}
			if s.Text == "" {
				return fmt.Errorf("%s: text is required for type: audio stages", ctx)
			}
			if !hasAudioExtension(s.Output) {
				return fmt.Errorf("%s: output %q must end in a supported audio extension for type: audio stages (allowed: %s)", ctx, s.Output, strings.Join(audioOutputExtensions, ", "))
			}
			engine := s.Engine
			if engine == "" {
				engine = AudioEnginePiper
			}
			switch engine {
			case AudioEnginePiper:
				if s.EngineURL != "" {
					return fmt.Errorf("%s: engine_url is only valid on engine: kokoro audio stages", ctx)
				}
			case AudioEngineKokoro:
				if s.VoicesDir != "" {
					return fmt.Errorf("%s: voices_dir is only valid on engine: piper audio stages", ctx)
				}
				if s.Binary != "" {
					return fmt.Errorf("%s: binary is only valid on engine: piper audio stages", ctx)
				}
			default:
				return fmt.Errorf("%s: engine %q is not supported (allowed: %q, %q)", ctx, engine, AudioEnginePiper, AudioEngineKokoro)
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages", ctx)
			}
			if s.OutputFormat != "" {
				return fmt.Errorf("%s: output_format is only valid on type: text stages", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
			// Synthesise a retry policy when none is set. Audio
			// backends (kokoro especially) flake on a small fraction
			// of items in long foreach runs; one empty response in
			// 1500+ chunks shouldn't kill the stage.
			if p.Stages[i].Retry == nil {
				p.Stages[i].Retry = &RetryPolicy{
					MaxAttempts: defaultAudioRetryAttempts,
					RetryOn:     []string{retryOnTransient, retryOnInvalidOutput},
				}
			}
		case StageTypeFFmpeg:
			// An ffmpeg stage selects exactly one mode: explicit
			// ffmpeg_args, auto concat_wavs (with optional dir scope),
			// or auto M4B chapterized concat (with explicit per-chapter
			// inputs). The three modes are mutually exclusive.
			isM4B := s.M4BFrom != "" || s.M4BVar != "" || s.M4BFile != "" || s.M4BChapter != ""
			if s.ConcatWavs && len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: concat_wavs and ffmpeg_args are mutually exclusive (concat_wavs constructs argv internally)", ctx)
			}
			if s.ConcatWavs && isM4B {
				return fmt.Errorf("%s: concat_wavs and m4b_* are mutually exclusive (pick one concat mode)", ctx)
			}
			if isM4B && len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: m4b_* and ffmpeg_args are mutually exclusive (m4b mode constructs argv internally)", ctx)
			}
			if !s.ConcatWavs && !isM4B && len(s.FFmpegArgs) == 0 {
				return fmt.Errorf("%s: ffmpeg_args is required for type: ffmpeg stages (unless concat_wavs or m4b_from is set)", ctx)
			}
			if s.ConcatWavsDir != "" && !s.ConcatWavs {
				return fmt.Errorf("%s: concat_wavs_dir is only valid when concat_wavs is true", ctx)
			}
			if s.ConcatWavsDir != "" {
				cleaned := filepath.Clean(s.ConcatWavsDir)
				if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
					return fmt.Errorf("%s: concat_wavs_dir %q must be a relative path inside the run dir", ctx, s.ConcatWavsDir)
				}
			}
			if isM4B {
				if s.M4BFrom == "" {
					return fmt.Errorf("%s: m4b_from is required when any m4b_* field is set", ctx)
				}
				if s.M4BFile == "" {
					return fmt.Errorf("%s: m4b_file is required when m4b_from is set", ctx)
				}
				if s.M4BChapter == "" {
					return fmt.Errorf("%s: m4b_chapter is required when m4b_from is set", ctx)
				}
				if _, err := template.New(s.ID + ":m4b_file").Funcs(templateFuncs()).Parse(s.M4BFile); err != nil {
					return fmt.Errorf("%s: parse m4b_file template: %w", ctx, err)
				}
				if _, err := template.New(s.ID + ":m4b_chapter").Funcs(templateFuncs()).Parse(s.M4BChapter); err != nil {
					return fmt.Errorf("%s: parse m4b_chapter template: %w", ctx, err)
				}
				// M4BFrom must be declared in Inputs so the cross-cutting
				// dependency is honest, matching foreach.from's rule.
				declared := false
				for _, dep := range s.Inputs {
					if dep == s.M4BFrom {
						declared = true
						break
					}
				}
				if !declared {
					return fmt.Errorf("%s: m4b_from %q must also appear in inputs", ctx, s.M4BFrom)
				}
				// Default M4BVar to "item" so the executor can rely on it
				// always being set.
				if s.M4BVar == "" {
					p.Stages[i].M4BVar = DefaultForeachVar
				}
				if !strings.HasSuffix(s.Output, ".m4b") {
					return fmt.Errorf("%s: output %q must end in .m4b when m4b_from is set", ctx, s.Output)
				}
			}
			if s.Capability != "" {
				return fmt.Errorf("%s: capability is only valid on stage types that activate a vibe profile (ffmpeg runs as a local subprocess)", ctx)
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages", ctx)
			}
			if s.OutputFormat != "" {
				return fmt.Errorf("%s: output_format is only valid on type: text stages", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" {
				return fmt.Errorf("%s: voice/text/voices_dir are only valid on type: audio stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
		case StageTypeComfyUI:
			if s.Workflow == "" {
				return fmt.Errorf("%s: workflow is required for type: comfyui stages", ctx)
			}
			if len(s.Parameters) == 0 {
				return fmt.Errorf("%s: parameters must have at least one entry for type: comfyui stages", ctx)
			}
			for key := range s.Parameters {
				if !comfyParamKeyRE.MatchString(key) {
					return fmt.Errorf("%s: parameters key %q must match \"<node_id>.<input_name>\" (e.g. \"6.text\")", ctx, key)
				}
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages (comfyui uses parameters)", ctx)
			}
			if s.OutputFormat != "" {
				return fmt.Errorf("%s: output_format is only valid on type: text stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" || s.Binary != "" {
				return fmt.Errorf("%s: voice/text/voices_dir/binary are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
		case StageTypeYouTube:
			if s.Video == "" {
				return fmt.Errorf("%s: video is required for type: youtube stages", ctx)
			}
			if s.Title == "" {
				return fmt.Errorf("%s: title is required for type: youtube stages", ctx)
			}
			if s.Description == "" {
				return fmt.Errorf("%s: description is required for type: youtube stages", ctx)
			}
			if s.Privacy != "" && s.Privacy != "private" && s.Privacy != "unlisted" && s.Privacy != "public" {
				return fmt.Errorf("%s: privacy %q is not supported (allowed: private, unlisted, public)", ctx, s.Privacy)
			}
			if s.Capability != "" {
				return fmt.Errorf("%s: capability is only valid on stage types that activate a vibe profile (youtube talks to the YouTube Data API as a network client)", ctx)
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages", ctx)
			}
			if s.OutputFormat != "" {
				return fmt.Errorf("%s: output_format is only valid on type: text stages", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" {
				return fmt.Errorf("%s: voice/text/voices_dir are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
			// (youtube fields are valid here — already checked above)
		case StageTypeWebhook:
			// retry_on_transient defaults to true (per the documented schema).
			// The transient-error classifier picks up the literal HTTP 429 /
			// 5xx status forms the webhook executor returns; we just need to
			// ensure a retry policy exists so the classifier is consulted at
			// all. If the user supplied their own retry: block we respect it
			// (their explicit choice wins). When retry_on_transient (or its
			// deprecated alias retry_on_5xx) is explicitly false we leave
			// Retry alone.
			if s.URL == "" {
				return fmt.Errorf("%s: url is required for type: webhook stages", ctx)
			}
			if s.RetryOnTransient != nil && s.RetryOn5xx != nil {
				return fmt.Errorf("%s: retry_on_transient and retry_on_5xx are mutually exclusive (retry_on_5xx is deprecated; prefer retry_on_transient)", ctx)
			}
			if s.RetryOn5xx != nil {
				// One-shot deprecation log on Validate so users see it once
				// per LoadPipeline call rather than once per stage execution.
				slog.Warn("retry_on_5xx is deprecated, use retry_on_transient (transient now covers HTTP 429 and 5xx)", "stage", s.ID)
			}
			wantsRetry := true
			switch {
			case s.RetryOnTransient != nil:
				wantsRetry = *s.RetryOnTransient
			case s.RetryOn5xx != nil:
				wantsRetry = *s.RetryOn5xx
			}
			if wantsRetry && p.Stages[i].Retry == nil {
				p.Stages[i].Retry = &RetryPolicy{
					MaxAttempts: defaultWebhookRetryAttempts,
					RetryOn:     []string{retryOnTransient},
				}
			}
			// Body and BodyTemplateFile are mutually exclusive. For
			// POST/PUT/PATCH (the notification shape) at least one is
			// required so we don't silently send an empty body. GET/DELETE
			// commonly carry no body — relaxed because the smoke-test
			// pattern (probe a URL) is awkward when you have to invent
			// a body just to satisfy the validator.
			hasInlineBody := len(s.Body) > 0
			hasBodyFile := s.BodyTemplateFile != ""
			if hasInlineBody && hasBodyFile {
				return fmt.Errorf("%s: exactly one of body or body_template_file is required (both set)", ctx)
			}
			method := strings.ToUpper(s.Method)
			if method == "" {
				method = http.MethodPost
			}
			bodyOptional := method == http.MethodGet || method == http.MethodDelete
			if !hasInlineBody && !hasBodyFile && !bodyOptional {
				return fmt.Errorf("%s: exactly one of body or body_template_file is required (neither set)", ctx)
			}
			if s.Method != "" {
				switch method {
				case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				default:
					return fmt.Errorf("%s: method %q is not supported (allowed: GET, POST, PUT, PATCH, DELETE)", ctx, s.Method)
				}
			}
			if s.Assert != nil {
				if sc := s.Assert.StatusCode; sc != 0 && (sc < 100 || sc > 599) {
					return fmt.Errorf("%s: assert.status_code %d is not a valid HTTP status (100-599)", ctx, sc)
				}
				if s.Assert.MinBodyLength < 0 {
					return fmt.Errorf("%s: assert.min_body_length must be >= 0", ctx)
				}
			}
			if s.Capability != "" {
				return fmt.Errorf("%s: capability is only valid on stage types that activate a vibe profile (webhook is a plain HTTP client)", ctx)
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" {
				return fmt.Errorf("%s: voice/text/voices_dir are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
		case StageTypeConfirm:
			// Confirm stages are pure local I/O: render Message, prompt the
			// operator on stdin (TTY mode) or via a marker file (detach
			// mode), and write "accepted"/"rejected" into Output. Nothing
			// from the other stage types is meaningful here.
			if s.Message == "" {
				return fmt.Errorf("%s: message is required for type: confirm stages", ctx)
			}
			if s.Timeout < 0 {
				return fmt.Errorf("%s: timeout must be >= 0 (got %s)", ctx, s.Timeout)
			}
			if s.Capability != "" {
				return fmt.Errorf("%s: capability is only valid on stage types that activate a vibe profile (confirm is a local prompt)", ctx)
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages", ctx)
			}
			if s.OutputFormat != "" {
				return fmt.Errorf("%s: output_format is only valid on type: text stages", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" || s.Binary != "" {
				return fmt.Errorf("%s: voice/text/voices_dir/binary are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
		case StageTypeRender:
			// Render stages are pure template → text: render a prompt or
			// prompt_file against the current binding and write the result.
			// No LLM call, no profile activation, no params.
			if (s.Prompt == "") == (s.PromptFile == "") {
				return fmt.Errorf("%s: exactly one of prompt or prompt_file is required", ctx)
			}
			if s.Prompt != "" {
				if _, err := template.New(s.ID + ":prompt").Funcs(templateFuncs()).Parse(s.Prompt); err != nil {
					return fmt.Errorf("%s: parse prompt template: %w", ctx, err)
				}
			}
			if s.Capability != "" {
				return fmt.Errorf("%s: capability is only valid on stage types that activate a vibe profile (render is pure template execution)", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages (render is template-only)", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" || s.Binary != "" {
				return fmt.Errorf("%s: voice/text/voices_dir/binary are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
		case StageTypeCompact:
			// Compact stages summarize an oversized upstream via LLM calls so
			// downstream prompts fit a model's context window. Required:
			// source (template), target_chars (> 0), capability (LLM to call).
			// preserve and chunk_chars are optional.
			if s.Source == "" {
				return fmt.Errorf("%s: source template is required for type: compact", ctx)
			}
			if _, err := template.New(s.ID + ":source").Funcs(templateFuncs()).Parse(s.Source); err != nil {
				return fmt.Errorf("%s: parse source template: %w", ctx, err)
			}
			if s.TargetChars <= 0 {
				return fmt.Errorf("%s: target_chars must be > 0 for type: compact", ctx)
			}
			if s.ChunkChars < 0 {
				return fmt.Errorf("%s: chunk_chars must be >= 0 (0 = pick automatically) for type: compact", ctx)
			}
			if s.Prompt != "" || s.PromptFile != "" {
				return fmt.Errorf("%s: prompt/prompt_file are not valid on type: compact (use source + preserve instead)", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages", ctx)
			}
			if s.Voice != "" || s.Text != "" || s.VoicesDir != "" {
				return fmt.Errorf("%s: voice/text/voices_dir are only valid on type: audio stages", ctx)
			}
			if len(s.FFmpegArgs) > 0 {
				return fmt.Errorf("%s: ffmpeg_args is only valid on type: ffmpeg stages", ctx)
			}
			if s.ImageDir != "" {
				return fmt.Errorf("%s: image_dir is only valid on type: text stages", ctx)
			}
			if len(s.ImageFiles) > 0 {
				return fmt.Errorf("%s: image_files is only valid on type: text stages", ctx)
			}
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
		case StageTypePandoc:
			if s.SourceFile == "" {
				return fmt.Errorf("%s: source_file is required for type: pandoc", ctx)
			}
			if _, err := template.New(s.ID + ":source_file").Funcs(templateFuncs()).Parse(s.SourceFile); err != nil {
				return fmt.Errorf("%s: parse source_file template: %w", ctx, err)
			}
			if s.PandocTo == "" {
				return fmt.Errorf("%s: pandoc_to is required for type: pandoc (e.g. \"epub\")", ctx)
			}
			if ext := pandocOutputExtension(s.PandocTo); ext != "" && !strings.HasSuffix(strings.ToLower(s.Output), ext) {
				return fmt.Errorf("%s: output %q should end in %s for pandoc_to: %s", ctx, s.Output, ext, s.PandocTo)
			}
			if s.CoverImage != "" {
				if _, err := template.New(s.ID + ":cover_image").Funcs(templateFuncs()).Parse(s.CoverImage); err != nil {
					return fmt.Errorf("%s: parse cover_image template: %w", ctx, err)
				}
			}
			if s.Capability != "" {
				return fmt.Errorf("%s: capability is only valid on stage types that activate a vibe profile (pandoc shells out to a binary)", ctx)
			}
			if s.Prompt != "" || s.PromptFile != "" {
				return fmt.Errorf("%s: prompt/prompt_file are not valid on type: pandoc", ctx)
			}
		default:
			return fmt.Errorf("%s: type %q is not supported (allowed: \"\", text, comfyui, audio, ffmpeg, youtube, webhook, confirm, render, compact, pandoc)", ctx, s.Type)
		}
		if s.OutputFormat != "" && s.OutputFormat != "json" {
			return fmt.Errorf("%s: output_format %q is not supported (allowed: \"\", json)", ctx, s.OutputFormat)
		}
		// Cleanup: optional list of glob patterns scrubbed after the stage's
		// success path completes. Restricted to stage types that produce a
		// stable on-disk output and own a meaningful chunk of intermediate
		// work (text/comfyui/audio/ffmpeg). webhook/youtube/confirm stages
		// talk to remote services or are pure markers — letting them clean
		// up files is most likely a misuse so we reject the field outright.
		if len(s.Cleanup) > 0 {
			switch stageType {
			case StageTypeText, StageTypeComfyUI, StageTypeAudio, StageTypeFFmpeg:
				// ok
			default:
				return fmt.Errorf("%s: cleanup is only valid on type: text/comfyui/audio/ffmpeg stages (got %q)", ctx, stageType)
			}
			if err := validateCleanupPatterns(ctx, s.Cleanup); err != nil {
				return err
			}
		}
		// Retry policy: validate user-supplied fields, then normalize so
		// the executor sees defaults filled in. Validation runs against
		// the pristine values (so negative ones are rejected); Normalize
		// mutates the policy in place via the slice element pointer so
		// the per-stage executor.Execute wrapper can read filled-in
		// defaults without recomputing them.
		if err := s.Retry.Validate(ctx); err != nil {
			return err
		}
		p.Stages[i].Retry.Normalize()
		// RunWhen: default to "success" when unset. The three reserved
		// keywords (success/failure/always) are gating qualifiers that
		// inspect upstream status; anything else is interpreted as a Go
		// text/template expression evaluated at dispatch time. We try-parse
		// template-form values up-front so syntax errors fail loud here
		// rather than surfacing only when the stage runs. Mutates the slice
		// entry in place so the scheduler can read the canonical value
		// without re-applying the default.
		switch s.RunWhen {
		case "":
			p.Stages[i].RunWhen = RunWhenSuccess
		case RunWhenSuccess, RunWhenFailure, RunWhenAlways:
			// ok
		default:
			if !looksLikeTemplate(s.RunWhen) {
				return fmt.Errorf("%s: run_when %q is not supported (allowed: %q, %q, %q, or a Go text/template expression containing %q)", ctx, s.RunWhen, RunWhenSuccess, RunWhenFailure, RunWhenAlways, "{{")
			}
			// Try-parse the template with the same funcs renderTemplate
			// will use at runtime; otherwise `{{ contains ... }}` and
			// other user-visible helpers would parse here but fail with a
			// "function X not defined" mid-run. Sharing the FuncMap keeps
			// the two paths in lock-step automatically.
			if _, err := template.New(s.ID + ":run_when").Funcs(templateFuncs()).Parse(s.RunWhen); err != nil {
				return fmt.Errorf("%s: parse run_when template: %w", ctx, err)
			}
		}
		if s.Foreach != nil {
			if s.Foreach.From == "" {
				return fmt.Errorf("%s: foreach.from is required", ctx)
			}
			// Output-path collision check is DEFERRED to runtime
			// (executeForeachStage's pre-render loop already enforces a
			// stronger check: it walks the actual rendered paths and errors
			// on any duplicate, using ForeachNonTemplatedMultiItemErrMsg
			// below when the rendered set collapses to identical paths from a
			// non-templated Output). Static "{{" presence rejects legitimate
			// single-item foreach uses where a non-templated output cannot
			// collide with itself; runtime knows the actual array length.
			//
			// Default Var to "item" so the executor can rely on the field
			// being set whenever Foreach is. We mutate the stage in place to
			// keep downstream code simple.
			if s.Foreach.Var == "" {
				p.Stages[i].Foreach.Var = DefaultForeachVar
			}
		}
	}
	// Second pass: every input must reference a declared stage, and no stage
	// may depend on itself. For foreach stages we additionally require the
	// upstream named in foreach.from to be declared as an input and to emit
	// output_format: json.
	for _, s := range p.Stages {
		ctx := fmt.Sprintf("stage %s", s.ID)
		for _, dep := range s.Inputs {
			if dep == s.ID {
				return fmt.Errorf("%s: input %q depends on itself", ctx, dep)
			}
			if !seenStages[dep] {
				return fmt.Errorf("%s: input %q does not reference any declared stage", ctx, dep)
			}
		}
		if s.Foreach != nil {
			from := s.Foreach.From
			if !seenStages[from] {
				return fmt.Errorf("%s: foreach.from %q does not reference any declared stage", ctx, from)
			}
			// Require an explicit declaration in Inputs. Auto-adding would
			// hide a real misconfiguration (e.g. typos in the inputs list,
			// or a user forgetting that the upstream needs to complete before
			// this stage runs); an explicit list keeps the DAG honest and the
			// error easier to diagnose.
			declared := false
			for _, dep := range s.Inputs {
				if dep == from {
					declared = true
					break
				}
			}
			if !declared {
				return fmt.Errorf("%s: foreach.from %q must also appear in inputs", ctx, from)
			}
			// The named upstream must produce JSON; otherwise the rendered
			// foreach source can't be parsed as an array.
			hasJSONSource := false
			for _, other := range p.Stages {
				if other.ID == from && other.OutputFormat == "json" {
					hasJSONSource = true
					break
				}
			}
			if !hasJSONSource {
				return fmt.Errorf("%s: foreach.from %q must reference a stage with output_format: json", ctx, from)
			}
		}
	}
	// Third pass: reject dependency cycles. A cycle is detected if a
	// topological sort cannot consume every stage.
	if cycle := findCycle(p.Stages); cycle != nil {
		return fmt.Errorf("dependency cycle detected: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// rejectYouTubeFields returns an error if any youtube-only field is set on a
// non-youtube stage. Kept as a small helper so each non-youtube case in
// Validate's type switch can cheaply enforce the same rejection without
// repeating six checks. Tags is intentionally NOT a youtube-only field — its
// name is generic enough that future stage types may reasonably reuse it; the
// other fields (video, privacy, category_id, thumbnail, credentials_file) are
// specific to the youtube upload path.
func rejectYouTubeFields(ctx string, s Stage) error {
	if s.Video != "" {
		return fmt.Errorf("%s: video is only valid on type: youtube stages", ctx)
	}
	if s.Privacy != "" {
		return fmt.Errorf("%s: privacy is only valid on type: youtube stages", ctx)
	}
	if s.CategoryID != "" {
		return fmt.Errorf("%s: category_id is only valid on type: youtube stages", ctx)
	}
	if s.Thumbnail != "" {
		return fmt.Errorf("%s: thumbnail is only valid on type: youtube stages", ctx)
	}
	if s.CredentialsFile != "" {
		return fmt.Errorf("%s: credentials_file is only valid on type: youtube stages", ctx)
	}
	return nil
}

// rejectConfirmFields rejects the confirm-only fields on non-confirm stages.
// Mirrors rejectWebhookFields / rejectYouTubeFields. message is the human-
// readable prompt, timeout bounds how long the stage waits for approval;
// both only make sense on a confirm stage.
func rejectConfirmFields(ctx string, s Stage) error {
	if s.Message != "" {
		return fmt.Errorf("%s: message is only valid on type: confirm stages", ctx)
	}
	if s.Timeout != 0 {
		return fmt.Errorf("%s: timeout is only valid on type: confirm stages", ctx)
	}
	return nil
}

// rejectWebhookFields rejects the webhook-only fields on non-webhook stages.
// Mirrors rejectYouTubeFields. URL is the canonical marker — body /
// body_template_file / headers / retry_on_5xx / method only make sense in
// concert with a destination, so the URL check would already catch most
// misuse, but enumerating each field gives users a clearer error pointing at
// the field they actually set in YAML.
func rejectWebhookFields(ctx string, s Stage) error {
	if s.URL != "" {
		return fmt.Errorf("%s: url is only valid on type: webhook stages", ctx)
	}
	if s.Method != "" {
		return fmt.Errorf("%s: method is only valid on type: webhook stages", ctx)
	}
	if len(s.Body) > 0 {
		return fmt.Errorf("%s: body is only valid on type: webhook stages", ctx)
	}
	if s.BodyTemplateFile != "" {
		return fmt.Errorf("%s: body_template_file is only valid on type: webhook stages", ctx)
	}
	if len(s.Headers) > 0 {
		return fmt.Errorf("%s: headers is only valid on type: webhook stages", ctx)
	}
	if s.RetryOn5xx != nil {
		return fmt.Errorf("%s: retry_on_5xx is only valid on type: webhook stages", ctx)
	}
	if s.RetryOnTransient != nil {
		return fmt.Errorf("%s: retry_on_transient is only valid on type: webhook stages", ctx)
	}
	if s.Assert != nil {
		return fmt.Errorf("%s: assert is only valid on type: webhook stages", ctx)
	}
	return nil
}

// audioOutputExtensions is the canonical allowlist of file extensions a
// type: audio stage may write to. Piper itself only emits WAV, but the
// validator accepts every common lossless / lossy container so a future
// TTS swap (or a wrapper that transcodes Piper's WAV in place) isn't
// blocked at load time. The runtime executor still produces whatever bytes
// the underlying TTS binary writes — the extension here is a declarative
// contract for downstream stages (ffmpeg/youtube/etc.) that read the file.
var audioOutputExtensions = []string{".wav", ".mp3", ".opus", ".flac", ".ogg"}

// hasAudioExtension reports whether path ends with any extension in
// audioOutputExtensions. Comparison is case-insensitive on the suffix so a
// macOS user typing `.WAV` still validates.
func hasAudioExtension(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range audioOutputExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// looksLikeTemplate reports whether s is plausibly a Go text/template
// expression (contains a `{{` action marker). Used to decide whether a
// run_when string that isn't one of the three reserved keywords should be
// passed to template.Parse for validation. We deliberately keep the check
// cheap (substring rather than a full lexer pass): a string that *looks*
// templated but parses as garbage produces a clear "parse run_when
// template" error in Validate's template.Parse call; a string that doesn't
// look templated is rejected as "not supported" so users get a precise
// error pointing at the typo'd keyword rather than a confusing template
// parse error from a flat string.
func looksLikeTemplate(s string) bool {
	return strings.Contains(s, "{{")
}

// knownTextParamKeys is the canonical allowlist of chat-completion params we
// recognise on a text stage's params: map. The list mirrors the OpenAI-shape
// fields every backend vamp speaks to today actually understands plus the
// llama.cpp grammar/sampling extensions (top_k, min_p, repetition_penalty)
// that the inference proxy forwards. Any other key is most likely a typo
// (e.g. "temperture") which historically slipped through verbatim and was
// silently ignored by the backend; Validate now rejects unknowns up-front
// with a "did you mean" hint computed against this list.
var knownTextParamKeys = []string{
	"temperature",
	"top_p",
	"top_k",
	"min_p",
	"max_tokens",
	"presence_penalty",
	"frequency_penalty",
	"repetition_penalty",
	"seed",
	"stop",
	"stream",
	"response_format",
	"n",
	"logprobs",
	"top_logprobs",
	"tools",
	"tool_choice",
	"parallel_tool_calls",
	"user",
}

// validateParamsKeys rejects any key in params that is not a member of the
// canonical allowlist. The first unknown key seen wins and the error
// includes a "did you mean: X, Y" hint when a near-match exists. Validation
// is intentionally strict-mode-only: silently ignoring unknown keys is what
// historically let `temperture: 0.7` reach the wire as a no-op.
func validateParamsKeys(ctx string, params map[string]any) error {
	if len(params) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(knownTextParamKeys))
	for _, k := range knownTextParamKeys {
		allowed[k] = true
	}
	// Iterate in sorted key order so the error we return for a pipeline
	// with multiple typos is deterministic across map-iteration runs.
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if allowed[k] {
			continue
		}
		if suggestions := suggestFrom(k, knownTextParamKeys); len(suggestions) > 0 {
			return fmt.Errorf("%s: params key %q is not a known chat-completion parameter (did you mean: %s?)", ctx, k, strings.Join(suggestions, ", "))
		}
		return fmt.Errorf("%s: params key %q is not a known chat-completion parameter", ctx, k)
	}
	return nil
}

// suggestFrom returns up to three candidates from `candidates` that are
// within Levenshtein edit distance <= 2 of `want`, sorted by distance
// ascending (ties broken by lexicographic order). Designed for the
// "did you mean:" hint in error messages; an empty return means no close
// match exists. Generalised so callers (params validation today, future
// run_when keyword typos) can reuse a single fuzzy-match helper.
func suggestFrom(want string, candidates []string) []string {
	type scored struct {
		s string
		d int
	}
	const maxDist = 2
	var hits []scored
	for _, c := range candidates {
		d := editDistance(want, c)
		if d <= maxDist {
			hits = append(hits, scored{s: c, d: d})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].d != hits[j].d {
			return hits[i].d < hits[j].d
		}
		return hits[i].s < hits[j].s
	})
	if len(hits) > 3 {
		hits = hits[:3]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.s
	}
	return out
}

// validateCleanupPatterns rejects glob patterns that escape the run dir.
// Each pattern is filepath.Clean'd and the cleaned form must:
//   - not be absolute (no leading /),
//   - not be the parent-traversal token "..",
//   - not start with "../" (or backslash equivalent on Windows),
//   - not equal "" or "." (a pattern that cleans to "." would match the
//     run dir itself; not useful and most likely a mistake).
//
// We do NOT expand globs here — that happens at runtime against the actual
// run dir. The point is to refuse patterns that COULD resolve outside the
// run dir under any expansion. The check runs at load time so a typoed
// cleanup pattern fails `vamp validate` instead of silently nuking files
// outside the run dir on first success.
func validateCleanupPatterns(ctx string, patterns []string) error {
	for _, p := range patterns {
		if p == "" {
			return fmt.Errorf("%s: cleanup pattern must not be empty", ctx)
		}
		if filepath.IsAbs(p) {
			return fmt.Errorf("%s: cleanup pattern %q must be relative to the run dir (absolute paths are rejected)", ctx, p)
		}
		cleaned := filepath.Clean(p)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s: cleanup pattern %q escapes the run dir (parent-traversal segments are rejected)", ctx, p)
		}
		if cleaned == "." || cleaned == "" {
			return fmt.Errorf("%s: cleanup pattern %q resolves to the run dir itself (refusing to wipe the run dir)", ctx, p)
		}
	}
	return nil
}

// findCycle returns the participating stage ids if the dependency graph
// contains a cycle, in the order they appear in the cycle, with the first id
// repeated at the end (e.g. ["a","b","a"]). Returns nil when the graph is
// acyclic.
func findCycle(stages []Stage) []string {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	deps := make(map[string][]string, len(stages))
	for _, s := range stages {
		deps[s.ID] = s.Inputs
	}
	color := make(map[string]int, len(stages))
	var stack []string
	var dfs func(id string) []string
	dfs = func(id string) []string {
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range deps[id] {
			switch color[dep] {
			case gray:
				// Found a back-edge to `dep`; build cycle slice from the
				// first occurrence on the stack.
				for i, n := range stack {
					if n == dep {
						out := append([]string{}, stack[i:]...)
						return append(out, dep)
					}
				}
				return []string{dep, dep}
			case white:
				if c := dfs(dep); c != nil {
					return c
				}
			}
		}
		color[id] = black
		stack = stack[:len(stack)-1]
		return nil
	}
	for _, s := range stages {
		if color[s.ID] == white {
			if c := dfs(s.ID); c != nil {
				return c
			}
		}
	}
	return nil
}
