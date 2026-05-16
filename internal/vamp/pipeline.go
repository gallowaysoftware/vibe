package vamp

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
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
	// value. RetryOn5xx defaults to true; when set together with a stage-
	// level retry: block of its own the stage's explicit policy wins.
	URL              string            `yaml:"url,omitempty"`
	Method           string            `yaml:"method,omitempty"`
	Body             map[string]any    `yaml:"body,omitempty"`
	BodyTemplateFile string            `yaml:"body_template_file,omitempty"`
	Headers          map[string]string `yaml:"headers,omitempty"`
	RetryOn5xx       *bool             `yaml:"retry_on_5xx,omitempty"`

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

	// Message is the rendered prompt a `type: confirm` stage prints to the
	// operator (and writes into the marker file) when asking for approval.
	// Required for confirm stages; rejected on every other stage type.
	Message string `yaml:"message,omitempty"`

	// Timeout, when non-zero, bounds how long a `type: confirm` stage will
	// wait for the operator to respond before auto-rejecting. Zero (the
	// default) means "wait forever". Rejected on every other stage type.
	Timeout time.Duration `yaml:"timeout,omitempty"`
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
		case retryOnTransient, retryOnTimeout:
		default:
			return fmt.Errorf("%s: retry.retry_on entry %q is not supported (allowed: %q, %q)", ctx, mode, retryOnTransient, retryOnTimeout)
		}
	}
	return nil
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
		if stageType != StageTypeAudio && stageType != StageTypeFFmpeg && stageType != StageTypeYouTube && stageType != StageTypeWebhook && stageType != StageTypeConfirm && s.Capability == "" {
			return fmt.Errorf("%s: capability is required", ctx)
		}
		switch stageType {
		case StageTypeText:
			if (s.Prompt == "") == (s.PromptFile == "") {
				return fmt.Errorf("%s: exactly one of prompt or prompt_file is required", ctx)
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
			if s.Voice == "" {
				return fmt.Errorf("%s: voice is required for type: audio stages", ctx)
			}
			if s.Text == "" {
				return fmt.Errorf("%s: text is required for type: audio stages", ctx)
			}
			if !strings.HasSuffix(s.Output, ".wav") {
				return fmt.Errorf("%s: output %q must end in .wav for type: audio stages", ctx, s.Output)
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
		case StageTypeFFmpeg:
			// ffmpeg stages drive a local subprocess to assemble media
			// from prior stage outputs. The only required schema is the
			// rendered argv list (FFmpegArgs) and the output path; the
			// executor appends `-y <output>` after the user args so users
			// don't manage the destination themselves.
			if len(s.FFmpegArgs) == 0 {
				return fmt.Errorf("%s: ffmpeg_args is required for type: ffmpeg stages", ctx)
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
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
			if err := rejectConfirmFields(ctx, s); err != nil {
				return err
			}
			// (youtube fields are valid here — already checked above)
		case StageTypeWebhook:
			// retry_on_5xx defaults to true (per the documented schema). The
			// transient-error classifier already picks up the
			// "webhook returned HTTP 5xx" error string the executor
			// produces; we just need to ensure a retry policy exists so the
			// classifier is consulted at all. If the user supplied their own
			// retry: block we respect it (their explicit choice wins). When
			// retry_on_5xx is explicitly false we leave Retry alone.
			if s.URL == "" {
				return fmt.Errorf("%s: url is required for type: webhook stages", ctx)
			}
			wantsRetry := s.RetryOn5xx == nil || *s.RetryOn5xx
			if wantsRetry && p.Stages[i].Retry == nil {
				p.Stages[i].Retry = &RetryPolicy{
					MaxAttempts: defaultWebhookRetryAttempts,
					RetryOn:     []string{retryOnTransient},
				}
			}
			// Body and BodyTemplateFile are mutually exclusive AND at least
			// one is required. Anything else either silently sends an empty
			// body (surprising for a notification stage) or accepts both and
			// makes precedence rules a footgun.
			hasInlineBody := len(s.Body) > 0
			hasBodyFile := s.BodyTemplateFile != ""
			if hasInlineBody && hasBodyFile {
				return fmt.Errorf("%s: exactly one of body or body_template_file is required (both set)", ctx)
			}
			if !hasInlineBody && !hasBodyFile {
				return fmt.Errorf("%s: exactly one of body or body_template_file is required (neither set)", ctx)
			}
			if s.Method != "" {
				m := strings.ToUpper(s.Method)
				switch m {
				case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				default:
					return fmt.Errorf("%s: method %q is not supported (allowed: GET, POST, PUT, PATCH, DELETE)", ctx, s.Method)
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
			if err := rejectYouTubeFields(ctx, s); err != nil {
				return err
			}
			if err := rejectWebhookFields(ctx, s); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: type %q is not supported (allowed: \"\", text, comfyui, audio, ffmpeg, youtube, webhook, confirm)", ctx, s.Type)
		}
		if s.OutputFormat != "" && s.OutputFormat != "json" {
			return fmt.Errorf("%s: output_format %q is not supported (allowed: \"\", json)", ctx, s.OutputFormat)
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
	return nil
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
