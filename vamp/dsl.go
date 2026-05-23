package vamp

import (
	"io/fs"
	"testing/fstest"
	"time"

	internalvamp "github.com/gallowaysoftware/vibe/internal/vamp"
)

// Ref is anything a stage builder will accept where a cross-stage reference
// is needed (After / Foreach.From). The DSL never expects you to write stage
// IDs as bare strings — you hand the *TextStage / *WebhookStage / … the
// other builders returned, so a renamed stage stays type-safe and an
// undeclared stage is a Go compile error rather than a runtime "input %q
// does not reference any declared stage" surprise.
type Ref interface{ ID() string }

// stageBase carries the methods every stage builder shares. T is the
// concrete builder type (e.g. *TextStage) so each chainable method returns
// the type the caller is mid-chain on — no upcasts to a generic base type
// mid-fluent-expression.
type stageBase[T any] struct {
	s    *Stage
	self T
}

// ID exposes the stage's id, satisfying the Ref interface.
func (b *stageBase[T]) ID() string { return b.s.ID }

// stage returns the wrapped *Stage. Exposed for tests + the Pipeline.Build
// validation step; user code goes through the fluent methods.
func (b *stageBase[T]) stage() *Stage { return b.s }

// After declares this stage's dependencies — every dep must finish before
// this stage runs. The stage builder accepts *TextStage / *WebhookStage /
// … because they satisfy Ref; pass them as Go variables to keep cross-stage
// wiring type-safe.
func (b *stageBase[T]) After(deps ...Ref) T {
	for _, dep := range deps {
		b.s.Inputs = append(b.s.Inputs, dep.ID())
	}
	return b.self
}

// Output sets the stage's output template (path relative to the run dir).
// The template renders against the standard binding (.inputs / .stages /
// .runDir / foreach item).
func (b *stageBase[T]) Output(path string) T {
	b.s.Output = path
	return b.self
}

// RunWhen overrides the stage's gating policy. Accepts the three reserved
// keywords (RunWhenSuccess / RunWhenFailure / RunWhenAlways) or any Go
// text/template expression containing "{{" — see Stage.RunWhen for the
// full semantics.
func (b *stageBase[T]) RunWhen(spec string) T {
	b.s.RunWhen = spec
	return b.self
}

// Cache toggles per-stage caching. nil-pointer default (no call) inherits
// the pipeline-level setting; calling Cache(false) opts this stage out
// regardless of the pipeline default.
func (b *stageBase[T]) Cache(enabled bool) T {
	b.s.Cache = &enabled
	return b.self
}

// Cleanup adds glob patterns scrubbed from the run dir after the stage's
// success path completes. See Stage.Cleanup for the validation rules
// (relative paths only, no .. escapes, only valid on stage types that
// produce a stable on-disk output).
func (b *stageBase[T]) Cleanup(patterns ...string) T {
	b.s.Cleanup = append(b.s.Cleanup, patterns...)
	return b.self
}

// Retry attaches a retry policy. Pass nil to disable retries explicitly;
// otherwise the policy's zero-value fields get filled in by Normalize at
// Validate time.
func (b *stageBase[T]) Retry(policy *RetryPolicy) T {
	b.s.Retry = policy
	return b.self
}

// Foreach turns the stage into a fan-out: it runs once per element of the
// JSON-array output produced by `from`. `from` must also be passed to
// After (the DSL enforces it implicitly — see foreachAfter). `varname`
// names the template variable bound to each element; empty defaults to
// DefaultForeachVar ("item").
func (b *stageBase[T]) Foreach(from Ref, varname string) T {
	if varname == "" {
		varname = DefaultForeachVar
	}
	b.s.Foreach = &ForeachSpec{From: from.ID(), Var: varname}
	// foreach implies dependency; make the After call optional. Validation
	// requires foreach.from to also appear in Inputs, so we add it here if
	// the caller forgot.
	seen := false
	for _, dep := range b.s.Inputs {
		if dep == from.ID() {
			seen = true
			break
		}
	}
	if !seen {
		b.s.Inputs = append(b.s.Inputs, from.ID())
	}
	return b.self
}

// ---- Pipeline ----

// Pipeline wraps an internal *Pipeline so the fluent constructors can return
// stage builders while the underlying value remains the same Pipeline the
// orchestrator already knows how to run.
type Pipeline struct {
	inner *internalvamp.Pipeline
}

// New starts a new pipeline. Validation runs at Build() time; the
// constructor itself is just allocation.
func New(name string) *Pipeline {
	return &Pipeline{inner: &internalvamp.Pipeline{Name: name}}
}

// Describe sets the pipeline-level description (free-form, surfaced by
// `vamp list` and the like).
func (p *Pipeline) Describe(desc string) *Pipeline {
	p.inner.Description = desc
	return p
}

// InputOption configures a pipeline-level Input declaration. See Required
// and WithDefault for the canonical options.
type InputOption func(*InputSpec)

// Required marks the input as required (CLI must supply --input <name>=…
// or the pipeline rejects the run).
func Required() InputOption { return func(s *InputSpec) { s.Required = true } }

// WithDefault sets the input's default value when --input <name>=… is
// omitted. The default is rendered as a Go text/template against the
// previously-resolved inputs, so a default may reference another input via
// `{{ .inputs.other }}`.
func WithDefault(v string) InputOption { return func(s *InputSpec) { s.Default = v } }

// WithType records the input's nominal type. Only "string" is supported
// today; reserved here so future numeric / boolean inputs can be added
// without an API churn.
func WithType(t string) InputOption { return func(s *InputSpec) { s.Type = t } }

// Describe attaches a one-line description to the input. Surfaced in
// Pipeline.Requirements() and used by pipeline binaries that expose
// first-class Cobra flags to populate --help.
func Describe(text string) InputOption { return func(s *InputSpec) { s.Description = text } }

// Input declares a pipeline-level input. The name is what `--input <name>=…`
// expects on the CLI; the options configure required-ness, defaults, type.
func (p *Pipeline) Input(name string, opts ...InputOption) *Pipeline {
	if p.inner.Inputs == nil {
		p.inner.Inputs = map[string]InputSpec{}
	}
	spec := InputSpec{Type: "string"}
	for _, opt := range opts {
		opt(&spec)
	}
	p.inner.Inputs[name] = spec
	return p
}

// Cache toggles caching at the pipeline level (the default for every
// cacheable stage type unless the stage overrides via *Stage.Cache).
func (p *Pipeline) Cache(enabled bool) *Pipeline {
	p.inner.Cache = &enabled
	return p
}

// RequireService declares a non-vibe HTTP service the pipeline needs
// reachable at run time. setupHint is a one-line shell command surfaced
// verbatim by tooling when the service is unreachable (e.g.
// "docker compose -f ~/.config/vibe/compose/searxng/docker-compose.yaml up -d").
// Pass "" for setupHint when no command applies.
func (p *Pipeline) RequireService(name, url, description, setupHint string) *Pipeline {
	p.inner.RequiredServices = append(p.inner.RequiredServices, ServiceRequirement{
		Name:        name,
		URL:         url,
		Description: description,
		SetupHint:   setupHint,
	})
	return p
}

// RequireGPUMemory records a human-readable hint for the GPU footprint
// the run will draw (e.g. "32GB"). Informational; no enforcement.
func (p *Pipeline) RequireGPUMemory(amount string) *Pipeline {
	p.inner.RequiredGPUMemory = amount
	return p
}

// RequireDiskSpace records a human-readable hint for the disk footprint
// intermediate artefacts will draw.
func (p *Pipeline) RequireDiskSpace(amount string) *Pipeline {
	p.inner.RequiredDiskSpace = amount
	return p
}

// Note appends a free-form string to the pipeline's requirements report
// — used for things that don't fit the structured fields (e.g.
// "first-run downloads ~30GB of models" or "expect ~5h wall time on a
// single RTX 5090").
func (p *Pipeline) Note(text string) *Pipeline {
	p.inner.RequirementNotes = append(p.inner.RequirementNotes, text)
	return p
}

// CapabilityModel attaches a model hint to a capability slot — recorded
// in Pipeline.Requirements() so tooling can warn before wiring the
// capability up to a model that obviously can't satisfy the prompts.
func (p *Pipeline) CapabilityModel(capability string, hint ModelHint) *Pipeline {
	if p.inner.CapabilityModelHints == nil {
		p.inner.CapabilityModelHints = map[string]ModelHint{}
	}
	p.inner.CapabilityModelHints[capability] = hint
	return p
}

// Requirements returns the pipeline's runtime contract: capabilities
// auto-derived from each stage, services + GPU/disk/notes from the
// builder methods above. Safe to call on an unbuilt pipeline; the
// returned value is a snapshot, not a live view.
func (p *Pipeline) Requirements() Requirements {
	return p.inner.Requirements()
}

// Build runs the same validation the YAML loader applies and returns the
// canonicalised *internal* Pipeline value (alias to vamp.Pipeline). Callers
// that just want to run the pipeline through cobra should use Main; Build
// is for tests and library callers that want the raw value.
func (p *Pipeline) Build() (*Pipeline, error) {
	if err := p.inner.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// internalPipeline returns the underlying *Pipeline so the cli helpers
// (which take internalvamp.Pipeline) can drive it directly. Unexported
// because external callers should treat the *Pipeline opaquely.
func (p *Pipeline) internalPipeline() *internalvamp.Pipeline { return p.inner }

// appendStage allocates a new Stage with the given id+type, appends it to
// the pipeline's stage list, and returns a pointer the stage builder can
// mutate. Centralising this keeps every per-type constructor honest about
// what it sets up.
func (p *Pipeline) appendStage(id string, t StageType) *Stage {
	p.inner.Stages = append(p.inner.Stages, Stage{ID: id, Type: t})
	return &p.inner.Stages[len(p.inner.Stages)-1]
}

// ---- Text stages ----

// TextStage is the LLM-chat-completion stage builder.
type TextStage struct {
	*stageBase[*TextStage]
}

// Text appends a text stage to the pipeline and returns its builder.
func (p *Pipeline) Text(id string) *TextStage {
	s := p.appendStage(id, StageTypeText)
	t := &TextStage{}
	t.stageBase = &stageBase[*TextStage]{s: s, self: nil}
	t.stageBase.self = t
	return t
}

// Capability names the capability slot this stage's LLM needs; the
// capability → profile mapping in ~/.config/vamp/capabilities.yaml decides
// which vibe profile gets activated.
func (t *TextStage) Capability(c string) *TextStage { t.s.Capability = c; return t }

// Prompt sets an inline prompt template. Mutually exclusive with PromptFS /
// PromptFile.
func (t *TextStage) Prompt(p string) *TextStage { t.s.Prompt = p; return t }

// PromptFS attaches a prompt template loaded from an fs.FS (typically an
// embed.FS shipped alongside the pipeline binary). `name` is the path
// inside the FS. Sets Stage.PromptFile to `name` and Stage.AssetFS to fsys
// so the executor resolves through the FS instead of the on-disk
// PipelineDir.
func (t *TextStage) PromptFS(fsys fs.FS, name string) *TextStage {
	t.s.PromptFile = name
	t.s.AssetFS = fsys
	return t
}

// PromptFile attaches a prompt template loaded from disk at the given path
// (absolute or relative to the working directory the pipeline binary was
// launched from). Use PromptFS for typical embed.FS shipping; PromptFile
// is the escape hatch for prompts the user materialises at runtime.
func (t *TextStage) PromptFile(path string) *TextStage { t.s.PromptFile = path; return t }

// OutputFormat sets the format hint on the stage's output. Today only "json"
// has runtime semantics (validation + downstream foreach consumption);
// other values are recorded but otherwise informational.
func (t *TextStage) OutputFormat(format string) *TextStage { t.s.OutputFormat = format; return t }

// OutputFormatJSON is the canonical shorthand for OutputFormat("json").
func (t *TextStage) OutputFormatJSON() *TextStage { t.s.OutputFormat = "json"; return t }

// Param sets a single chat-completion request parameter (temperature,
// seed, max_tokens, top_p, …). Repeated calls accumulate; later wins on key
// collision.
func (t *TextStage) Param(key string, value any) *TextStage {
	if t.s.Params == nil {
		t.s.Params = map[string]any{}
	}
	t.s.Params[key] = value
	return t
}

// ImageDir attaches a (templated) directory of images to the prompt, taking
// the stage's multimodal path. Requires a vision-capable backend (Gemma 3
// + mmproj) at run time.
func (t *TextStage) ImageDir(dir string) *TextStage { t.s.ImageDir = dir; return t }

// ImageFile appends an explicit (templated) image path to the stage's
// multimodal payload. Use for per-iteration single-image fan-out where
// ImageDir's globbing would be too coarse (e.g. one diagram per
// foreach item). Mutually exclusive with ImageDir at validate time.
func (t *TextStage) ImageFile(tmpl string) *TextStage {
	t.s.ImageFiles = append(t.s.ImageFiles, tmpl)
	return t
}

// ---- Render stages ----

// RenderStage is a deterministic template-only stage (no LLM call).
type RenderStage struct {
	*stageBase[*RenderStage]
}

// Render appends a render stage.
func (p *Pipeline) Render(id string) *RenderStage {
	s := p.appendStage(id, StageTypeRender)
	r := &RenderStage{}
	r.stageBase = &stageBase[*RenderStage]{s: s}
	r.stageBase.self = r
	return r
}

func (r *RenderStage) Prompt(p string) *RenderStage        { r.s.Prompt = p; return r }
func (r *RenderStage) PromptFile(path string) *RenderStage { r.s.PromptFile = path; return r }
func (r *RenderStage) PromptFS(fsys fs.FS, name string) *RenderStage {
	r.s.PromptFile = name
	r.s.AssetFS = fsys
	return r
}

// OutputFormat sets the format hint on the stage's output. Today only
// "json" has runtime semantics (validation + downstream foreach
// consumption).
func (r *RenderStage) OutputFormat(format string) *RenderStage {
	r.s.OutputFormat = format
	return r
}

// OutputFormatJSON is the canonical shorthand for OutputFormat("json").
func (r *RenderStage) OutputFormatJSON() *RenderStage {
	r.s.OutputFormat = "json"
	return r
}

// ---- Compact stages ----

// CompactStage summarises an oversized upstream via LLM calls to fit a
// target size.
type CompactStage struct {
	*stageBase[*CompactStage]
}

// Compact appends a compact stage.
func (p *Pipeline) Compact(id string) *CompactStage {
	s := p.appendStage(id, StageTypeCompact)
	c := &CompactStage{}
	c.stageBase = &stageBase[*CompactStage]{s: s}
	c.stageBase.self = c
	return c
}

func (c *CompactStage) Capability(cap string) *CompactStage     { c.s.Capability = cap; return c }
func (c *CompactStage) Source(template string) *CompactStage    { c.s.Source = template; return c }
func (c *CompactStage) TargetChars(n int) *CompactStage         { c.s.TargetChars = n; return c }
func (c *CompactStage) ChunkChars(n int) *CompactStage          { c.s.ChunkChars = n; return c }
func (c *CompactStage) Preserve(directive string) *CompactStage { c.s.Preserve = directive; return c }

// ---- Audio stages ----

// AudioStage shells out to Piper TTS to synthesise speech.
type AudioStage struct {
	*stageBase[*AudioStage]
}

// Audio appends an audio stage.
func (p *Pipeline) Audio(id string) *AudioStage {
	s := p.appendStage(id, StageTypeAudio)
	a := &AudioStage{}
	a.stageBase = &stageBase[*AudioStage]{s: s}
	a.stageBase.self = a
	return a
}

func (a *AudioStage) Voice(name string) *AudioStage     { a.s.Voice = name; return a }
func (a *AudioStage) TextTemplate(t string) *AudioStage { a.s.Text = t; return a }
func (a *AudioStage) VoicesDir(dir string) *AudioStage  { a.s.VoicesDir = dir; return a }
func (a *AudioStage) Binary(bin string) *AudioStage     { a.s.Binary = bin; return a }

// Capability names the capability slot a kokoro-engined audio stage
// activates (so vibe brings the kokoro-fastapi container up as a
// profile). Piper-engined stages don't activate a profile (subprocess
// + on-disk voice), so this is a no-op for the piper path.
func (a *AudioStage) Capability(c string) *AudioStage { a.s.Capability = c; return a }

// Engine selects the TTS backend: AudioEnginePiper (default, subprocess
// + .onnx) or AudioEngineKokoro (HTTP-backed Kokoro-FastAPI).
func (a *AudioStage) Engine(name string) *AudioStage { a.s.Engine = name; return a }

// EngineURL overrides the kokoro endpoint base. Empty falls back to
// the active vibe profile's proxy URL.
func (a *AudioStage) EngineURL(url string) *AudioStage { a.s.EngineURL = url; return a }

// ---- FFmpeg stages ----

// FFmpegStage invokes ffmpeg locally.
type FFmpegStage struct {
	*stageBase[*FFmpegStage]
}

// FFmpeg appends an ffmpeg stage.
func (p *Pipeline) FFmpeg(id string) *FFmpegStage {
	s := p.appendStage(id, StageTypeFFmpeg)
	f := &FFmpegStage{}
	f.stageBase = &stageBase[*FFmpegStage]{s: s}
	f.stageBase.self = f
	return f
}

// Args sets the literal argv (after the binary, before -y <output>) passed
// to ffmpeg. Each entry is template-rendered before exec.
func (f *FFmpegStage) Args(args ...string) *FFmpegStage {
	f.s.FFmpegArgs = append(f.s.FFmpegArgs, args...)
	return f
}

// ConcatWavs enables the auto-concat-all-WAVs shortcut. When set, Args is
// rejected (the executor constructs argv internally).
func (f *FFmpegStage) ConcatWavs() *FFmpegStage { f.s.ConcatWavs = true; return f }

// ConcatWavsDir scopes ConcatWavs to a subdirectory of the run dir
// (template-rendered).
func (f *FFmpegStage) ConcatWavsDir(dir string) *FFmpegStage { f.s.ConcatWavsDir = dir; return f }

// Binary overrides the ffmpeg binary location (default: "ffmpeg" on $PATH).
func (f *FFmpegStage) Binary(bin string) *FFmpegStage { f.s.Binary = bin; return f }

// M4BFrom switches the stage into chapterised-M4B mode: chapters are
// driven by the upstream's JSON array (preserving its emit order so
// natural unit order survives), one chapter per array element.
func (f *FFmpegStage) M4BFrom(from Ref) *FFmpegStage {
	f.s.M4BFrom = from.ID()
	seen := false
	for _, dep := range f.s.Inputs {
		if dep == from.ID() {
			seen = true
			break
		}
	}
	if !seen {
		f.s.Inputs = append(f.s.Inputs, from.ID())
	}
	return f
}

// M4BVar sets the template variable name bound to each upstream item
// during M4BFile / M4BChapter rendering. Defaults to DefaultForeachVar.
func (f *FFmpegStage) M4BVar(name string) *FFmpegStage { f.s.M4BVar = name; return f }

// M4BFile is a template rendered per upstream item to the per-chapter
// MP3 path (relative to run dir, or absolute).
func (f *FFmpegStage) M4BFile(tmpl string) *FFmpegStage { f.s.M4BFile = tmpl; return f }

// M4BChapter is a template rendered per upstream item to the chapter
// title embedded in the M4B metadata.
func (f *FFmpegStage) M4BChapter(tmpl string) *FFmpegStage { f.s.M4BChapter = tmpl; return f }

// CoverImage attaches a cover-art file (template-rendered path).
// Meaningful on M4B ffmpeg stages (embedded as audiobook art) and on
// pandoc EPUB stages (passed as --epub-cover-image).
func (f *FFmpegStage) CoverImage(path string) *FFmpegStage { f.s.CoverImage = path; return f }

// ---- Pandoc stages ----

// PandocStage runs pandoc to convert one document format to another
// (markdown → EPUB, markdown → PDF, etc.). Inside vibe the executor
// shells out to a docker pandoc/core image so users don't have to
// install pandoc on the host.
type PandocStage struct {
	*stageBase[*PandocStage]
}

// Pandoc appends a pandoc stage.
func (p *Pipeline) Pandoc(id string) *PandocStage {
	s := p.appendStage(id, StageTypePandoc)
	ps := &PandocStage{}
	ps.stageBase = &stageBase[*PandocStage]{s: s}
	ps.stageBase.self = ps
	return ps
}

// SourceFile is a template rendering the input document path.
func (p *PandocStage) SourceFile(tmpl string) *PandocStage { p.s.SourceFile = tmpl; return p }

// From sets the --from format flag (e.g. "markdown").
func (p *PandocStage) From(format string) *PandocStage { p.s.PandocFrom = format; return p }

// To sets the --to format flag (e.g. "epub", "pdf").
func (p *PandocStage) To(format string) *PandocStage { p.s.PandocTo = format; return p }

// Metadata adds a single --metadata KEY=VALUE pair.
func (p *PandocStage) Metadata(key, value string) *PandocStage {
	if p.s.PandocMetadata == nil {
		p.s.PandocMetadata = map[string]string{}
	}
	p.s.PandocMetadata[key] = value
	return p
}

// Args appends raw arguments forwarded verbatim to pandoc.
func (p *PandocStage) Args(args ...string) *PandocStage {
	p.s.PandocArgs = append(p.s.PandocArgs, args...)
	return p
}

// CoverImage attaches an EPUB cover image (template-rendered path,
// relative to the run dir or absolute).
func (p *PandocStage) CoverImage(path string) *PandocStage { p.s.CoverImage = path; return p }

// ---- Mix stages ----

// MixStage renders a structured-script JSON file into a chapterised
// audiobook / podcast m4b via ffmpeg. The script lists voice
// segments in order plus optional music/SFX cues; the executor
// builds the filtergraph (concat + loudnorm in v1, with sidechain
// ducking and SFX punch-ins planned for v2).
type MixStage struct {
	*stageBase[*MixStage]
}

// Mix appends a mix stage.
func (p *Pipeline) Mix(id string) *MixStage {
	s := p.appendStage(id, StageTypeMix)
	m := &MixStage{}
	m.stageBase = &stageBase[*MixStage]{s: s}
	m.stageBase.self = m
	return m
}

// ScriptFile is the path (template-rendered) to the showrunner's
// script JSON the mix executor consumes. Required.
func (m *MixStage) ScriptFile(path string) *MixStage { m.s.ScriptFile = path; return m }

// IntroMusic is an optional audio file prepended before the first
// voice segment.
func (m *MixStage) IntroMusic(path string) *MixStage { m.s.IntroMusic = path; return m }

// OutroMusic is an optional audio file appended after the last
// voice segment.
func (m *MixStage) OutroMusic(path string) *MixStage { m.s.OutroMusic = path; return m }

// LoudnessTarget sets the integrated loudness target in LUFS
// (ffmpeg loudnorm I=). Default -16, the podcast standard.
func (m *MixStage) LoudnessTarget(lufs float64) *MixStage { m.s.LoudnessTarget = lufs; return m }

// CoverImage attaches a cover image embedded in the resulting m4b.
func (m *MixStage) CoverImage(path string) *MixStage { m.s.CoverImage = path; return m }

// Binary overrides the ffmpeg binary (default "ffmpeg" on $PATH).
func (m *MixStage) Binary(bin string) *MixStage { m.s.Binary = bin; return m }

// ---- ComfyUI stages ----

// ComfyUIStage submits a ComfyUI workflow.
type ComfyUIStage struct {
	*stageBase[*ComfyUIStage]
}

// ComfyUI appends a comfyui stage.
func (p *Pipeline) ComfyUI(id string) *ComfyUIStage {
	s := p.appendStage(id, StageTypeComfyUI)
	c := &ComfyUIStage{}
	c.stageBase = &stageBase[*ComfyUIStage]{s: s}
	c.stageBase.self = c
	return c
}

func (c *ComfyUIStage) Capability(cap string) *ComfyUIStage { c.s.Capability = cap; return c }

// WorkflowFile points at a ComfyUI workflow JSON file on disk (absolute or
// relative to the binary's working directory).
func (c *ComfyUIStage) WorkflowFile(path string) *ComfyUIStage { c.s.Workflow = path; return c }

// WorkflowFS attaches a workflow JSON loaded from an fs.FS shipped with the
// pipeline binary.
func (c *ComfyUIStage) WorkflowFS(fsys fs.FS, name string) *ComfyUIStage {
	c.s.Workflow = name
	c.s.AssetFS = fsys
	return c
}

// Parameter sets a per-node-input template-rendered value. `key` must match
// "<node_id>.<input_name>" (e.g. "6.text"); the value is rendered then
// type-coerced and substituted into the workflow before submission.
func (c *ComfyUIStage) Parameter(key, value string) *ComfyUIStage {
	if c.s.Parameters == nil {
		c.s.Parameters = map[string]string{}
	}
	c.s.Parameters[key] = value
	return c
}

// ---- YouTube stages ----

// YouTubeStage uploads a video to YouTube.
type YouTubeStage struct {
	*stageBase[*YouTubeStage]
}

// YouTube appends a youtube stage.
func (p *Pipeline) YouTube(id string) *YouTubeStage {
	s := p.appendStage(id, StageTypeYouTube)
	y := &YouTubeStage{}
	y.stageBase = &stageBase[*YouTubeStage]{s: s}
	y.stageBase.self = y
	return y
}

func (y *YouTubeStage) Video(template string) *YouTubeStage { y.s.Video = template; return y }
func (y *YouTubeStage) Title(template string) *YouTubeStage { y.s.Title = template; return y }
func (y *YouTubeStage) Description(template string) *YouTubeStage {
	y.s.Description = template
	return y
}
func (y *YouTubeStage) Tags(tags ...string) *YouTubeStage {
	y.s.Tags = append(y.s.Tags, tags...)
	return y
}
func (y *YouTubeStage) Privacy(p string) *YouTubeStage          { y.s.Privacy = p; return y }
func (y *YouTubeStage) CategoryID(id string) *YouTubeStage      { y.s.CategoryID = id; return y }
func (y *YouTubeStage) Thumbnail(template string) *YouTubeStage { y.s.Thumbnail = template; return y }
func (y *YouTubeStage) CredentialsFile(path string) *YouTubeStage {
	y.s.CredentialsFile = path
	return y
}

// ---- Webhook stages ----

// WebhookStage POSTs (or GET / PUT / PATCH / DELETE) a body to a URL.
type WebhookStage struct {
	*stageBase[*WebhookStage]
}

// Webhook appends a webhook stage.
func (p *Pipeline) Webhook(id string) *WebhookStage {
	s := p.appendStage(id, StageTypeWebhook)
	w := &WebhookStage{}
	w.stageBase = &stageBase[*WebhookStage]{s: s}
	w.stageBase.self = w
	return w
}

func (w *WebhookStage) URL(template string) *WebhookStage  { w.s.URL = template; return w }
func (w *WebhookStage) Method(method string) *WebhookStage { w.s.Method = method; return w }

// Body sets an inline JSON body. Values are templates (string values are
// template-rendered against the standard binding; non-string values are
// marshalled verbatim).
func (w *WebhookStage) Body(body map[string]any) *WebhookStage { w.s.Body = body; return w }

// BodyTemplate accepts an inline string template that becomes the request
// body verbatim after rendering. Under the hood we stash the template into
// an in-memory fs.FS so the existing BodyTemplateFile code path is reused;
// users don't see this indirection.
func (w *WebhookStage) BodyTemplate(template string) *WebhookStage {
	const name = "__body.tmpl"
	w.s.BodyTemplateFile = name
	w.s.AssetFS = fstest.MapFS{name: &fstest.MapFile{Data: []byte(template)}}
	return w
}

// BodyTemplateFS attaches a body template loaded from an fs.FS shipped
// with the pipeline binary.
func (w *WebhookStage) BodyTemplateFS(fsys fs.FS, name string) *WebhookStage {
	w.s.BodyTemplateFile = name
	w.s.AssetFS = fsys
	return w
}

// BodyTemplateFile points at a body template on disk.
func (w *WebhookStage) BodyTemplateFile(path string) *WebhookStage {
	w.s.BodyTemplateFile = path
	return w
}

// Header sets a single header (template-rendered at run time).
func (w *WebhookStage) Header(key, value string) *WebhookStage {
	if w.s.Headers == nil {
		w.s.Headers = map[string]string{}
	}
	w.s.Headers[key] = value
	return w
}

// Headers sets multiple headers at once.
func (w *WebhookStage) Headers(headers map[string]string) *WebhookStage {
	if w.s.Headers == nil {
		w.s.Headers = map[string]string{}
	}
	for k, v := range headers {
		w.s.Headers[k] = v
	}
	return w
}

// RetryOnTransient enables / disables retry on HTTP 429 + 5xx (default on).
func (w *WebhookStage) RetryOnTransient(enabled bool) *WebhookStage {
	w.s.RetryOnTransient = &enabled
	return w
}

// Assert turns this webhook into an assertion stage: the executor runs the
// request and fails if the response doesn't match expectations.
func (w *WebhookStage) Assert(spec *AssertSpec) *WebhookStage { w.s.Assert = spec; return w }

// ---- Confirm stages ----

// ConfirmStage prompts the operator for approval before the pipeline
// continues.
type ConfirmStage struct {
	*stageBase[*ConfirmStage]
}

// Confirm appends a confirm stage.
func (p *Pipeline) Confirm(id string) *ConfirmStage {
	s := p.appendStage(id, StageTypeConfirm)
	c := &ConfirmStage{}
	c.stageBase = &stageBase[*ConfirmStage]{s: s}
	c.stageBase.self = c
	return c
}

func (c *ConfirmStage) Message(template string) *ConfirmStage { c.s.Message = template; return c }
func (c *ConfirmStage) Timeout(d time.Duration) *ConfirmStage { c.s.Timeout = d; return c }
