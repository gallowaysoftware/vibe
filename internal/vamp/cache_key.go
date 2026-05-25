package vamp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
)

// VampNoCacheEnv, when set to "1", forces the cache layer off regardless of
// the pipeline / stage cache fields. This is the env-var counterpart to
// `vamp run --no-cache`; CI runners and one-off debugging sessions can set
// it without editing the pipeline file or threading a flag.
const VampNoCacheEnv = "VAMP_NO_CACHE"

// EnvCacheDisabled reports whether the env-var disable switch is active.
// Centralised so the CLI flag handling and any future call site agree on the
// canonical truthy values (we only honor "1" today; future "true" parsing
// can land here without touching every reader).
func EnvCacheDisabled() bool {
	return os.Getenv(VampNoCacheEnv) == "1"
}

// stageCacheable reports whether the stage's type is eligible for
// content-addressed caching. Webhook and YouTube stages perform network side
// effects with no idempotency guarantee from the receiver; replaying a
// cached "success" would skip the side effect that gave the pipeline its
// reason for existing. Future "confirm" stages (human-in-loop) would also
// land here. The check is centralised so the cache write site and the
// CLI/docs all agree on the same allow-list.
func stageCacheable(st *Stage) bool {
	switch stageTypeOrDefault(st) {
	case StageTypeText, StageTypeComfyUI, StageTypeAudio, StageTypeFFmpeg, StageTypeRender, StageTypeCompact, StageTypePandoc, StageTypeMix:
		return true
	case StageTypeWebhook:
		// Webhook stages are NOT cacheable by default — most are
		// side-effecting POSTs (notifications) where serving a cached
		// "success" would skip the side effect that gave the pipeline
		// its reason for existing. Opt in with `cache: true` on the
		// stage for idempotent reads (e.g. SearXNG queries) where
		// re-serving the same response across runs is the entire
		// point of having a cache.
		return st != nil && st.Cache != nil && *st.Cache
	default:
		return false
	}
}

// pipelineCacheEnabled returns true when the pipeline-level cache toggle is
// either absent (default-on) or explicitly true. Per-stage opt-out happens
// in stageCacheOptedOut below; this helper only consults the top-level field.
func pipelineCacheEnabled(p *Pipeline) bool {
	if p == nil || p.Cache == nil {
		return true
	}
	return *p.Cache
}

// stageCacheOptedOut returns true when EITHER the pipeline-level OR the
// stage-level cache field explicitly disables caching. Stages whose type is
// not cacheable bail out earlier (stageCacheable returns false), so this
// helper only matters for the on/off discrimination among cacheable types.
func stageCacheOptedOut(p *Pipeline, st *Stage) bool {
	if !pipelineCacheEnabled(p) {
		return true
	}
	if st != nil && st.Cache != nil && !*st.Cache {
		return true
	}
	return false
}

// computeCacheKey produces the content-addressed cache key for one stage
// invocation. For foreach stages the caller passes the rendered per-item
// prompt/params; for ordinary stages the caller passes the rendered prompt
// and the stage's static params. The function returns ("", nil) when the
// stage is not eligible for caching, including for opt-out cases — that's
// the signal to the runner that the cache layer is dormant for this call.
//
// keyInput carries every per-stage / per-item bit we need; constructing it at
// the call site (rather than re-rendering inside this function) keeps the
// runner's template render path the single source of truth for rendered
// values.
type keyInput struct {
	StageType      StageType
	Capability     string
	RenderedPrompt string            // text only
	ModelID        string            // text only
	Params         map[string]any    // text only
	OutputFormat   string            // text only
	ImageHashes    []string          // text/vision only — sha of each image attached via image_dir, sorted by source path
	Workflow       map[string]any    // comfyui only — the pre-applied workflow JSON
	WorkflowParams map[string]string // comfyui only — rendered parameter values
	Voice          string            // audio only
	RenderedText   string            // audio only
	Binary         string            // audio/ffmpeg only
	VoiceModelSize int64             // audio only — piper engine
	AudioEngine    string            // audio only — "" / piper / kokoro
	AudioURL       string            // audio only — kokoro endpoint base
	FFmpegArgs     []string          // ffmpeg only — rendered argv
	FFmpegInputs   []string          // ffmpeg only — absolute paths to inputs, sha-mixed below
}

// stageCacheKey is the package-visible entry point used by the runner. It
// dispatches to the per-type composer based on the stage type and returns
// the hex sha256 cache key.
func stageCacheKey(in keyInput) (string, error) {
	switch in.StageType {
	case StageTypeText, "":
		paramsJSON, err := cache.CanonicalJSON(coerceNilMap(in.Params))
		if err != nil {
			return "", fmt.Errorf("text cache key: marshal params: %w", err)
		}
		imagesJSON, err := cache.CanonicalJSON(stringSliceToAny(in.ImageHashes))
		if err != nil {
			return "", fmt.Errorf("text cache key: marshal image hashes: %w", err)
		}
		return cache.HashStrings("text",
			in.Capability,
			in.RenderedPrompt,
			in.ModelID,
			string(paramsJSON),
			in.OutputFormat,
			string(imagesJSON),
		), nil
	case StageTypeComfyUI:
		// The workflow we hash here is the post-substitution form: each
		// rendered Stage.Parameters entry has already been written into the
		// workflow's node inputs. That captures the user's literal prompt
		// text and any seed overrides without us repeating the substitution
		// logic in two places (the substitution code path lives in
		// comfyui_executor's applyParameters).
		workflowJSON, err := cache.CanonicalJSON(in.Workflow)
		if err != nil {
			return "", fmt.Errorf("comfyui cache key: marshal workflow: %w", err)
		}
		// Also fold the rendered parameters map in directly so a workflow
		// that happens to ignore a parameter (typo in the node id) still
		// changes its key when the user changes the parameter value. Belt
		// + braces: the workflow JSON already reflects valid substitutions;
		// the parameter map catches the no-op-substitution edge case.
		paramsJSON, err := cache.CanonicalJSON(stringMapToAny(in.WorkflowParams))
		if err != nil {
			return "", fmt.Errorf("comfyui cache key: marshal params: %w", err)
		}
		return cache.HashStrings("comfyui",
			in.Capability,
			string(workflowJSON),
			string(paramsJSON),
		), nil
	case StageTypeAudio:
		engine := in.AudioEngine
		if engine == "" {
			engine = "piper"
		}
		// The piper path keys on the voice-file size so a swapped .onnx
		// (same name, different bytes) invalidates. Kokoro is HTTP-served
		// and there's no local artefact to hash; the endpoint URL goes in
		// instead so two different Kokoro servers / two different voices
		// won't collide.
		if engine == "kokoro" {
			return cache.HashStrings("audio",
				engine,
				in.AudioURL,
				in.Voice,
				in.RenderedText,
			), nil
		}
		binary := in.Binary
		if binary == "" {
			binary = "piper"
		}
		return cache.HashStrings("audio",
			engine,
			in.Voice,
			in.RenderedText,
			binary,
			fmt.Sprintf("%d", in.VoiceModelSize),
		), nil
	case StageTypeFFmpeg:
		binary := in.Binary
		if binary == "" {
			binary = "ffmpeg"
		}
		argsJSON, err := cache.CanonicalJSON(stringSliceToAny(in.FFmpegArgs))
		if err != nil {
			return "", fmt.Errorf("ffmpeg cache key: marshal args: %w", err)
		}
		// Hash every declared input file's content into the key so a stage
		// that consumes "audio.wav + image.png" misses cache when either
		// upstream changes. Files that don't exist (e.g. paths that
		// resolve only at ffmpeg runtime, not before) are folded in as a
		// missing-marker so the key still differs.
		inputs := make([]string, 0, len(in.FFmpegInputs))
		for _, p := range in.FFmpegInputs {
			h, err := cache.HashFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					inputs = append(inputs, "missing:"+p)
					continue
				}
				return "", fmt.Errorf("ffmpeg cache key: hash %s: %w", p, err)
			}
			inputs = append(inputs, h)
		}
		inputsJSON, err := cache.CanonicalJSON(stringSliceToAny(inputs))
		if err != nil {
			return "", fmt.Errorf("ffmpeg cache key: marshal inputs: %w", err)
		}
		return cache.HashStrings("ffmpeg",
			binary,
			string(argsJSON),
			string(inputsJSON),
		), nil
	case StageTypeRender:
		return cache.HashStrings("render", in.RenderedPrompt), nil
	case StageTypeCompact:
		return cache.HashStrings("compact", in.RenderedPrompt, in.ModelID), nil
	default:
		// Non-cacheable types should never reach here — stageCacheable
		// gates that upstream. Return an empty key as a defensive no-op so
		// a future caller that forgets the gate doesn't accidentally cache
		// a webhook side effect.
		return "", nil
	}
}

// coerceNilMap returns an empty map when m is nil. CanonicalJSON would happily
// marshal nil to "null", but we want a stable `{}` so an absent params block
// hashes identically to an explicit `params: {}`.
func coerceNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// stringMapToAny converts map[string]string to map[string]any. cache.CanonicalJSON's
// signature is map-agnostic but Go's generic type system can't infer this
// from a typed map without a copy; this helper keeps the call sites tidy.
func stringMapToAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// stringSliceToAny converts []string to []any so CanonicalJSON can iterate.
// Same reasoning as stringMapToAny — type-system bookkeeping, no semantic
// transformation.
func stringSliceToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// computeStageCacheKey returns the cache key for one stage invocation.
//
// `item` and `itemIdx` are set when the runner is computing a per-foreach-item
// key (item == nil for plain stages). The function re-renders every templated
// input the cache key depends on so the cache layer stays a pure side-channel:
// the live execution path doesn't need to thread rendered values into the
// cache call, and any future renderer change (e.g. new template funcs) keeps
// the key composition mechanically in lockstep.
//
// Returns ("", nil) for non-cacheable types or when the stage opts out.
// Returns an error only on rendering failures the caller should surface
// (typically: a broken Output template that the live path would also error
// on); the runner's caller wraps these into the live-execution error path.
func (e *Executor) computeStageCacheKey(st *Stage, item any, itemIdx int) (string, error) {
	if !stageCacheable(st) {
		return "", nil
	}
	if stageCacheOptedOut(e.Pipeline, st) {
		return "", nil
	}
	extra := map[string]any{}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = item
		extra["i"] = itemIdx
	}
	switch stageTypeOrDefault(st) {
	case StageTypeText:
		prompt, err := renderPrompt(st, e.PipelineDir, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render prompt: %w", err)
		}
		// Image bytes attached via image_dir contribute to the model's
		// output as much as the prompt does; fold a hash of each one into
		// the key so edits to an SVG or PNG in a lesson dir invalidate
		// just that lesson's cached output. Empty when ImageDir is unset.
		imageHashes, err := e.hashStageImages(st, extra)
		if err != nil {
			return "", err
		}
		// We hash the chat-completion model id alongside the rendered prompt
		// + params so a profile candidate-fallback (e.g. 27B → 14B due to
		// VRAM) produces a different key and the smaller model's output
		// isn't served on a later large-VRAM rerun.
		return stageCacheKey(keyInput{
			StageType:      StageTypeText,
			Capability:     st.Capability,
			RenderedPrompt: prompt,
			ModelID:        e.cachedKeyModelID(),
			Params:         st.Params,
			OutputFormat:   st.OutputFormat,
			ImageHashes:    imageHashes,
		})
	case StageTypeComfyUI:
		workflow, err := loadWorkflow(st, e.PipelineDir)
		if err != nil {
			return "", fmt.Errorf("cache key: load workflow: %w", err)
		}
		// Mirror applyParameters' rendered values without depending on a
		// StageInput plumbing (the cache-key call site predates the
		// scheduler's per-invocation StageInput construction). We render
		// each Stage.Parameters template against the same standard binding.
		rendered := make(map[string]string, len(st.Parameters))
		for k, tmpl := range st.Parameters {
			val, err := renderTemplate(st.ID+":param:"+k, tmpl, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render parameter %s: %w", k, err)
			}
			rendered[k] = val
			// Mirror applyParameters' destructive substitution into the
			// workflow map so the canonical JSON used for the cache key
			// reflects what the executor will actually submit.
			parts := strings.SplitN(k, ".", 2)
			if len(parts) != 2 {
				continue
			}
			node, ok := workflow[parts[0]].(map[string]any)
			if !ok {
				continue
			}
			inputs, ok := node["inputs"].(map[string]any)
			if !ok {
				continue
			}
			inputs[parts[1]] = coerceParamValue(val)
		}
		return stageCacheKey(keyInput{
			StageType:      StageTypeComfyUI,
			Capability:     st.Capability,
			Workflow:       workflow,
			WorkflowParams: rendered,
		})
	case StageTypeAudio:
		text, err := renderTemplate(st.ID+":text", st.Text, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render audio text: %w", err)
		}
		// Voice is template-rendered too (mirroring audioExecutor.Execute)
		// so per-item voice routing produces per-item cache keys. Static
		// voice IDs (no `{{`) template-render to themselves so legacy
		// audio stages hit the same key as before this change.
		voice, err := renderTemplate(st.ID+":voice", st.Voice, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render audio voice: %w", err)
		}
		engine := st.Engine
		if engine == "" {
			engine = AudioEnginePiper
		}
		if engine == AudioEngineKokoro {
			url := strings.TrimRight(st.EngineURL, "/")
			if url == "" {
				url = defaultKokoroURL
			}
			return stageCacheKey(keyInput{
				StageType:    StageTypeAudio,
				Voice:        voice,
				RenderedText: text,
				AudioEngine:  AudioEngineKokoro,
				AudioURL:     url,
			})
		}
		// Resolve the voice's .onnx path the same way audioExecutor does so
		// the file-size mixed into the key reflects the same file the live
		// path will load. A missing model surfaces as a zero size + the
		// "missing:" mark on the path string in the key, so two different
		// missing voices still produce different keys.
		voicesDir := st.VoicesDir
		if voicesDir == "" {
			voicesDir = defaultVoicesDirPath
		}
		voicesDir = expandAudioTilde(voicesDir)
		voicePath := filepath.Join(voicesDir, voice+".onnx")
		size, err := cache.FileSize(voicePath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("cache key: stat voice %s: %w", voicePath, err)
			}
			size = 0
		}
		return stageCacheKey(keyInput{
			StageType:      StageTypeAudio,
			Voice:          voice,
			RenderedText:   text,
			Binary:         st.Binary,
			VoiceModelSize: size,
			AudioEngine:    AudioEnginePiper,
		})
	case StageTypeFFmpeg:
		args := make([]string, 0, len(st.FFmpegArgs))
		for i, raw := range st.FFmpegArgs {
			rendered, err := renderTemplate(fmt.Sprintf("%s:arg[%d]", st.ID, i), raw, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render arg[%d]: %w", i, err)
			}
			args = append(args, rendered)
		}
		// Heuristic input detection: any rendered argv entry that names an
		// existing file is treated as an input whose bytes should mix into
		// the key. ffmpeg argv mixes flags and file paths freely, so we
		// can't statically distinguish "input file" from "flag value";
		// stat-existence is the safest approximation and matches what
		// users intuitively expect (changing a referenced .wav -> cache
		// miss).
		inputs := make([]string, 0, len(args))
		for _, a := range args {
			if _, err := os.Stat(a); err == nil {
				inputs = append(inputs, a)
			}
		}
		// For concat_wavs stages the per-iteration context lives in fields
		// the user-args heuristic above can't see (st.FFmpegArgs is empty,
		// the WAVs are walked by the executor, not named on argv). Without
		// the supplemental fields below, every foreach iteration of an
		// `mp3` stage with `concat_wavs: true` produces an identical cache
		// key and serves the first iteration's output to all iterations.
		// Fold in: the rendered output path, the rendered concat_wavs_dir,
		// and the sha of every WAV that walk would pick up.
		if st.ConcatWavs {
			outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render output: %w", err)
			}
			// The "v2" tag invalidates concat_wavs cache entries from the
			// pre-per-iteration-concat-list-path era. Those entries were
			// keyed correctly on inputs but produced wrong output because
			// parallel foreach mp3 iterations raced on a shared
			// .ffmpeg-concat.txt. Bump this tag if a future executor fix
			// makes existing cached MP3s invalid again.
			args = append(args, "concat_wavs:v2:output:"+outRel)
			walkRoot := e.RunDir
			if st.ConcatWavsDir != "" {
				dir, err := renderTemplate(st.ID+":concat_wavs_dir", st.ConcatWavsDir, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
				if err != nil {
					return "", fmt.Errorf("cache key: render concat_wavs_dir: %w", err)
				}
				args = append(args, "concat_wavs:dir:"+dir)
				walkRoot = filepath.Join(e.RunDir, dir)
			}
			// Add every WAV under walkRoot to FFmpegInputs so the cache
			// key reflects the actual bytes that ffmpeg will glue together.
			err = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					if os.IsNotExist(walkErr) {
						return nil
					}
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				if strings.EqualFold(filepath.Ext(d.Name()), ".wav") {
					inputs = append(inputs, path)
				}
				return nil
			})
			if err != nil {
				return "", fmt.Errorf("cache key: walk %s: %w", walkRoot, err)
			}
			sort.Strings(inputs)
		}
		// M4B mode keys on the rendered per-chapter file list + chapter
		// titles + cover image bytes. Without this, an m4b stage with
		// empty FFmpegArgs collides in cache with any other empty-args
		// ffmpeg stage (e.g. an older concat_wavs entry from a prior
		// run) and serves bytes from the wrong file. Mirrors the
		// concat_wavs:v2 fix.
		if st.M4BFrom != "" {
			args = append(args, "m4b:v1:from:"+st.M4BFrom)
			outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render m4b output: %w", err)
			}
			args = append(args, "m4b:output:"+outRel)
			args = append(args, "m4b:file_template:"+st.M4BFile)
			args = append(args, "m4b:chapter_template:"+st.M4BChapter)
			if st.CoverImage != "" {
				coverRel, err := renderTemplate(st.ID+":cover_image", st.CoverImage, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
				if err != nil {
					return "", fmt.Errorf("cache key: render cover_image: %w", err)
				}
				args = append(args, "m4b:cover:"+coverRel)
				coverAbs := coverRel
				if !filepath.IsAbs(coverAbs) {
					coverAbs = filepath.Join(e.RunDir, coverRel)
				}
				if _, err := os.Stat(coverAbs); err == nil {
					inputs = append(inputs, coverAbs)
				}
			}
			// Fold the upstream stage's output (the chapter list source)
			// into the key so a different parent list invalidates.
			if upstream, ok := e.snapshotPrior(st.Inputs)[st.M4BFrom]; ok && upstream != nil {
				args = append(args, "m4b:from_output:"+upstream.Output)
			}
		}
		return stageCacheKey(keyInput{
			StageType:    StageTypeFFmpeg,
			Binary:       st.Binary,
			FFmpegArgs:   args,
			FFmpegInputs: inputs,
		})
	case StageTypeRender:
		prompt, err := renderPrompt(st, e.PipelineDir, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render prompt: %w", err)
		}
		return cache.HashStrings("render", prompt), nil
	case StageTypeCompact:
		// Cache key reflects the source + target_chars + chunk_chars + preserve
		// directive; identical inputs and parameters yield identical compaction.
		source, err := renderTemplate(st.ID+":source", st.Source, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render compact source: %w", err)
		}
		return cache.HashStrings("compact",
			st.Capability,
			source,
			fmt.Sprintf("%d", st.TargetChars),
			fmt.Sprintf("%d", st.ChunkChars),
			st.Preserve,
			e.cachedKeyModelID(),
		), nil
	case StageTypeWebhook:
		// Only reached when the stage opts in via `cache: true`.
		// Key folds in the rendered URL, method, headers, and body —
		// everything that determines the response. Two runs hitting
		// the same opt-in webhook with identical inputs serve the
		// cached response without re-issuing the HTTP call.
		url, err := renderTemplate(st.ID+":url", st.URL, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render url: %w", err)
		}
		// Body is a YAML map; serialise canonically so reordered keys
		// don't change the hash. BodyTemplateFile (if set) is hashed
		// via its rendered content.
		bodyJSON := ""
		if len(st.Body) > 0 {
			b, err := cache.CanonicalJSON(st.Body)
			if err != nil {
				return "", fmt.Errorf("cache key: marshal body: %w", err)
			}
			bodyJSON = string(b)
		}
		if st.BodyTemplateFile != "" {
			bf, err := os.ReadFile(filepath.Join(e.PipelineDir, st.BodyTemplateFile))
			if err != nil {
				return "", fmt.Errorf("cache key: read body_template_file: %w", err)
			}
			rendered, err := renderTemplate(st.ID+":body_file", string(bf), st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render body_file: %w", err)
			}
			bodyJSON = rendered
		}
		hdrKeys := make([]string, 0, len(st.Headers))
		for k := range st.Headers {
			hdrKeys = append(hdrKeys, k)
		}
		sort.Strings(hdrKeys)
		hdr := make([]string, 0, len(hdrKeys))
		for _, k := range hdrKeys {
			rv, err := renderTemplate(st.ID+":header:"+k, st.Headers[k], st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render header %s: %w", k, err)
			}
			hdr = append(hdr, k+"="+rv)
		}
		// Fold the assert spec into the key so changing it (e.g.
		// status_code 200 → 4xx) invalidates the cache. Without this,
		// a cached "passing" response would continue to be served
		// silently even after the user changed the assertion contract.
		assertJSON := ""
		if st.Assert != nil {
			b, err := cache.CanonicalJSON(st.Assert)
			if err != nil {
				return "", fmt.Errorf("cache key: marshal assert: %w", err)
			}
			assertJSON = string(b)
		}
		return cache.HashStrings("webhook",
			strings.ToUpper(st.Method),
			url,
			bodyJSON,
			strings.Join(hdr, "\n"),
			assertJSON,
		), nil
	default:
		return "", nil
	}
}

// hashStageImages resolves the stage's image inputs against the current
// template binding and returns one sha256 per file in sorted path order.
// Two modes:
//   - ImageDir: scan a directory for supported extensions.
//   - ImageFiles: render each templated path and hash the resolved file.
//
// SVGs hash against their source bytes (not the rasterized PNG) so the cache
// key stays stable across rsvg-convert version bumps. Returns nil when
// neither field is set so the keyInput slice marshals to a stable empty
// representation.
func (e *Executor) hashStageImages(st *Stage, extra map[string]any) ([]string, error) {
	if st == nil {
		return nil, nil
	}
	if st.ImageDir == "" && len(st.ImageFiles) == 0 {
		return nil, nil
	}
	if len(st.ImageFiles) > 0 {
		paths := make([]string, 0, len(st.ImageFiles))
		for idx, tmpl := range st.ImageFiles {
			resolved, err := renderTemplate(fmt.Sprintf("%s:image_files[%d]", st.ID, idx), tmpl, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return nil, fmt.Errorf("cache key: render image_files[%d]: %w", idx, err)
			}
			p := expandTilde(strings.TrimSpace(resolved))
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(e.PipelineDir, p)
			}
			paths = append(paths, p)
		}
		sort.Strings(paths)
		hashes := make([]string, 0, len(paths))
		for _, p := range paths {
			h, err := cache.HashFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, nil
				}
				return nil, fmt.Errorf("cache key: hash image %s: %w", p, err)
			}
			hashes = append(hashes, h)
		}
		return hashes, nil
	}
	resolved, err := renderTemplate(st.ID+":image_dir", st.ImageDir, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("cache key: render image_dir: %w", err)
	}
	dir := expandTilde(resolved)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(e.PipelineDir, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing image_dir is not a cache-key error: the live executor
		// will surface the same os.ReadDir failure with a clearer message
		// from collectImages. Returning an empty slice here keeps the key
		// composition deterministic and lets the failure happen once, in
		// the executor.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache key: read image_dir %s: %w", dir, err)
	}
	allowed := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
		".webp": true, ".bmp": true, ".svg": true,
	}
	var paths []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if allowed[strings.ToLower(filepath.Ext(ent.Name()))] {
			paths = append(paths, filepath.Join(dir, ent.Name()))
		}
	}
	sort.Strings(paths)
	hashes := make([]string, 0, len(paths))
	for _, p := range paths {
		h, err := cache.HashFile(p)
		if err != nil {
			return nil, fmt.Errorf("cache key: hash image %s: %w", p, err)
		}
		hashes = append(hashes, h)
	}
	return hashes, nil
}

// cachedKeyModelID returns the resolved chat-completion model id for the
// active profile when known, or "" when the runner hasn't activated a
// profile yet. The empty-string fallback means a cache key computed before
// the first activation still differs from one computed after — but in
// practice every text stage call goes through the activation path before we
// compute its key, so the empty-string branch is defensive only.
func (e *Executor) cachedKeyModelID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cachedModelID
}

// tryCacheGet looks up the entry for `key` in the executor's cache store and
// returns it on hit, or (nil, nil) on miss. nil store / empty key disables
// the lookup entirely so callers can use a uniform code path.
func (e *Executor) tryCacheGet(key string) (*cache.Entry, error) {
	if e.Cache == nil || key == "" {
		return nil, nil
	}
	return e.Cache.Get(key)
}

// materializeCacheHit copies a cached payload into the run dir so downstream
// stages see exactly the same shape they'd see on a live executor call:
//
//   - text stages: returns the cached text as StageOutput.Text and the
//     runner's normal write-back path (executeStage / executeForeachStage)
//     persists it to <RunDir>/<rendered output path>.
//   - binary stages (comfyui/audio/ffmpeg): writes the cached bytes to
//     <RunDir>/<rendered output path> and returns StageOutput.Files with
//     the absolute path, mirroring the live executor's contract.
//
// Returning an error here causes the runWithRetry wrapper to fall through to
// a live executor call — the cache layer must never wedge a run.
func (e *Executor) materializeCacheHit(_ context.Context, st *Stage, in StageInput, hit *cache.Entry) (*StageOutput, error) {
	extra := map[string]any{}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}
	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("render output path: %w", err)
	}
	// Text stages return their content via StageOutput.Text; the runner's
	// caller writes it. Binary stages own the on-disk write so the cache
	// layer mirrors that contract by writing the bytes here and reporting
	// an absolute path in Files.
	if !hit.IsBinary {
		return &StageOutput{Text: string(hit.Output)}, nil
	}
	outAbs := filepath.Join(e.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache materialize dir: %w", err)
	}
	if err := os.WriteFile(outAbs, hit.Output, 0o644); err != nil {
		return nil, fmt.Errorf("write cached output to %s: %w", outAbs, err)
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// tryCachePut writes a fresh entry for `key`. Cache write failures are
// returned to the caller but the caller's contract is to log them as warnings
// (a failed cache write must not fail the run — the live output already
// landed in the run dir). Empty key / nil store skip the write.
func (e *Executor) tryCachePut(key string, stageType string, out *StageOutput) error {
	if e.Cache == nil || key == "" || out == nil {
		return nil
	}
	in := cache.PutInput{
		Hash:         key,
		StageType:    stageType,
		SourceRunDir: e.RunDir,
	}
	if len(out.Files) > 0 {
		// Multi-file outputs are rare today (comfyui rejects > 1) but the
		// schema allows it. We cache the FIRST file's bytes — the runner
		// records exactly one file in StageOutput.Files at the live path
		// per stage, and downstream `.stages.X.output` references already
		// assume that.
		body, err := os.ReadFile(out.Files[0])
		if err != nil {
			return err
		}
		in.Bytes = body
	} else {
		in.Text = out.Text
	}
	return e.Cache.Put(in)
}
