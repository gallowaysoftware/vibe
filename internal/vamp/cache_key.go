package vamp

import (
	"context"
	"encoding/json"
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
// reason for existing. Confirm stages (human-in-loop) land there too: a
// cached "accepted" is a decision nobody made this run.
//
// This is only the ADVERTISEMENT. It decides whether the timing report
// prints `cache: hit/miss` for the stage at all; whether anything is
// actually cached is computeStageCacheKey's business, and a type on this
// list with no branch there advertises a saving it never makes. That is
// not hypothetical — `pandoc` shipped exactly that way. The two are held
// together by TestCacheableAndKeyableAgree, which walks allStageTypes and
// fails if either half knows a type the other does not.
func stageCacheable(st *Stage) bool {
	switch stageTypeOrDefault(st) {
	case StageTypeText, StageTypeComfyUI, StageTypeAudio, StageTypeFFmpeg, StageTypeRender, StageTypeCompact, StageTypePandoc, StageTypeMix, StageTypeShort:
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
	StageType        StageType
	Capability       string
	RenderedPrompt   string            // text only
	ModelID          string            // text only
	Params           map[string]any    // text only
	OutputFormat     string            // text only
	ImageHashes      []string          // text/vision only — sha of each image attached via image_dir, sorted by source path
	Workflow         map[string]any    // comfyui only — the pre-applied workflow JSON
	WorkflowParams   map[string]string // comfyui only — rendered parameter values
	InputImageHashes []string          // comfyui only — sha of each input_images source, sorted by node-input key
	Voice            string            // audio only
	RenderedText     string            // audio only
	Binary           string            // audio/ffmpeg only
	VoiceModelSize   int64             // audio only — piper engine
	AudioEngine      string            // audio only — "" / piper / kokoro
	AudioURL         string            // audio only — kokoro endpoint base
	AudioEffect      string            // audio only — ffmpeg -af chain applied in place
	FFmpegArgs       []string          // ffmpeg only — rendered argv
	FFmpegInputs     []string          // ffmpeg only — absolute paths to inputs, sha-mixed below
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
		// Gate the input_images element so a parameters-only stage keeps its
		// historical key (cache.HashStrings is positional — appending an
		// element unconditionally would invalidate every existing comfyui
		// cache entry on upgrade). When input_images IS used we fold the
		// source images' content hashes (NOT the transient uploaded
		// filenames), so the key is content-addressed on the actual bytes fed
		// into the workflow.
		if len(in.InputImageHashes) == 0 {
			return cache.HashStrings("comfyui",
				in.Capability,
				string(workflowJSON),
				string(paramsJSON),
			), nil
		}
		imagesJSON, err := cache.CanonicalJSON(stringSliceToAny(in.InputImageHashes))
		if err != nil {
			return "", fmt.Errorf("comfyui cache key: marshal input image hashes: %w", err)
		}
		return cache.HashStrings("comfyui",
			in.Capability,
			string(workflowJSON),
			string(paramsJSON),
			string(imagesJSON),
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
			parts := []string{
				engine,
				in.AudioURL,
				in.Voice,
				in.RenderedText,
				in.AudioEffect,
			}
			// A capability-routed stage resolves its live endpoint from the
			// active profile's proxy URL (in.BaseURL in the executor), which
			// isn't known at key time — AudioURL above only carries the
			// explicit engine_url or the localhost default. Fold the
			// capability name in so two capabilities fronting different TTS
			// servers don't collide. Gated on non-empty (HashStrings is
			// positional) so non-capability stages keep their historical
			// keys; their live URL resolution matches AudioURL exactly.
			// This distinguishes capabilities, not the same capability
			// repointed at a different server — the same tradeoff the text
			// and comfyui keys make.
			if in.Capability != "" {
				parts = append(parts, in.Capability)
			}
			return cache.HashStrings("audio", parts...), nil
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
			in.AudioEffect,
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
	default:
		// Non-cacheable types should never reach here — stageCacheable
		// gates that upstream. Return an empty key as a defensive no-op so
		// a future caller that forgets the gate doesn't accidentally cache
		// a webhook side effect.
		//
		// A type gets a branch HERE only if computeStageCacheKey routes it
		// here; the rest compose their key in place, and a type must never
		// be defined in both. This case list held `render` and `compact`
		// entries that nothing reached, and compact's was already WRONG —
		// prompt+model, where the live composer keys on
		// capability+source+target_chars+chunk_chars+preserve+model. A
		// second definition of a cache key that no caller can reach is not
		// dead weight, it is a wrong answer waiting for its first caller.
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
		// Fold each input_images source's content hash so a regenerated
		// upstream image (same path, new bytes) misses cache. Sorted by
		// node-input key to match applyInputImages' deterministic order.
		var imgHashes []string
		if len(st.InputImages) > 0 {
			iiKeys := make([]string, 0, len(st.InputImages))
			for k := range st.InputImages {
				iiKeys = append(iiKeys, k)
			}
			sort.Strings(iiKeys)
			for _, k := range iiKeys {
				src, err := renderTemplate(st.ID+":input_image:"+k, st.InputImages[k], st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
				if err != nil {
					return "", fmt.Errorf("cache key: render input_images %s: %w", k, err)
				}
				if !filepath.IsAbs(src) {
					src = filepath.Join(e.RunDir, src)
				}
				h, err := cache.HashFile(src)
				if err != nil {
					if os.IsNotExist(err) {
						imgHashes = append(imgHashes, "missing:"+src)
						continue
					}
					return "", fmt.Errorf("cache key: hash input_images %s: %w", k, err)
				}
				imgHashes = append(imgHashes, h)
			}
		}
		return stageCacheKey(keyInput{
			StageType:        StageTypeComfyUI,
			Capability:       st.Capability,
			Workflow:         workflow,
			WorkflowParams:   rendered,
			InputImageHashes: imgHashes,
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
				Capability:   st.Capability,
				Voice:        voice,
				RenderedText: text,
				AudioEngine:  AudioEngineKokoro,
				AudioURL:     url,
				AudioEffect:  st.Effect,
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
		voicesDir = expandTilde(voicesDir)
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
			AudioEffect:    st.Effect,
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
				// The key must hash the set the executor will actually
				// concatenate; executeConcatWavs skips vamp's scratch, so
				// this walk has to skip it too or a crashed run's leftovers
				// would change the key without changing the output.
				if isScratchName(d.Name()) {
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
		// concat_video mode keys on the rendered clip list + each clip's
		// bytes + the normalize dims, like m4b. Empty FFmpegArgs would
		// otherwise collide with any other empty-args ffmpeg stage.
		if st.ConcatVideoFrom != "" {
			w, h, fps := normalizeVideoDims(st.ConcatVideoWidth, st.ConcatVideoHeight, st.ConcatVideoFPS)
			outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render concat_video output: %w", err)
			}
			args = append(args,
				"concat_video:v1:from:"+st.ConcatVideoFrom,
				"concat_video:output:"+outRel,
				"concat_video:file_template:"+st.ConcatVideoFile,
				fmt.Sprintf("concat_video:dims:%dx%d@%d", w, h, fps),
			)
			variable := st.ConcatVideoVar
			if variable == "" {
				variable = DefaultForeachVar
			}
			if upstream, ok := e.snapshotPrior(st.Inputs)[st.ConcatVideoFrom]; ok && upstream != nil {
				args = append(args, "concat_video:from_output:"+upstream.Output)
				items, derr := decodeUpstreamArray(stripModelArtifacts(upstream.Output))
				if derr == nil {
					for i, raw := range items {
						itemExtra := map[string]any{variable: raw, "i": i}
						for k, v := range extra {
							itemExtra[k] = v
						}
						clip, rerr := renderTemplate(fmt.Sprintf("%s:concat_video_file[%d]", st.ID, i), st.ConcatVideoFile, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, itemExtra)
						if rerr != nil {
							return "", fmt.Errorf("cache key: render concat_video_file[%d]: %w", i, rerr)
						}
						if !filepath.IsAbs(clip) {
							clip = filepath.Join(e.RunDir, clip)
						}
						inputs = append(inputs, clip)
					}
				}
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
		// don't change the hash. String leaves are template-rendered
		// first, mirroring the executor's renderWebhookMap — hashing the
		// raw templates would give every foreach item / every run with
		// different bindings the SAME key, so item 0's response would be
		// silently served for all of them. BodyTemplateFile (if set) is
		// hashed via its rendered content.
		bodyJSON := ""
		if len(st.Body) > 0 {
			renderedBody, err := e.renderCacheBodyValue(st, st.ID+":body", st.Body, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render body: %w", err)
			}
			b, err := cache.CanonicalJSON(renderedBody)
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
	case StageTypePandoc:
		// pandoc was on stageCacheable's allow-list from the day the type
		// landed and never had a branch here, so it fell through to the
		// default's empty key: no Get, no Put, and `cache: miss` in the
		// timing report on every run forever. The stage advertised a
		// saving it never made — and a full EPUB build (docker pull on
		// first use, then a whole-book conversion) is one of the most
		// expensive things vamp does per byte of output.
		//
		// The conversion is a pure function of the source bytes and the
		// argv, exactly like ffmpeg, so caching it is right on the merits
		// rather than merely convenient. Everything buildPandocArgs reads
		// goes in — that function's own comment already assumed as much
		// ("deterministic metadata-key order so the cache key (rendered
		// argv) doesn't oscillate"), which is how long this has been
		// believed to work.
		//
		// Note what this makes load-bearing: pandoc's 0-byte guard
		// (`requireNonEmptyOutput`) was previously belt-only, because a
		// stage that never caches cannot cache an empty book. It now
		// carries the braces too — the runner only Puts on a nil error, so
		// the guard is the only thing standing between a swallowed pandoc
		// failure and a permanent, content-addressed empty EPUB. It has a
		// test and a mutation-registry entry; keep both.
		srcRel, err := renderTemplate(st.ID+":source_file", st.SourceFile, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render pandoc source_file: %w", err)
		}
		outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if err != nil {
			return "", fmt.Errorf("cache key: render pandoc output: %w", err)
		}
		// Content, not paths: a regenerated markdown source at the same
		// path must miss. hashMediaFiles folds an absent file in as a
		// stable "missing:" marker, which is the right answer here too —
		// the executor is about to fail on it, and if it later appears
		// the key changes.
		media := []string{srcRel}
		coverRel := ""
		if st.CoverImage != "" {
			coverRel, err = renderTemplate(st.ID+":cover_image", st.CoverImage, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if err != nil {
				return "", fmt.Errorf("cache key: render pandoc cover_image: %w", err)
			}
			media = append(media, coverRel)
		}
		metaKeys := make([]string, 0, len(st.PandocMetadata))
		for k := range st.PandocMetadata {
			metaKeys = append(metaKeys, k)
		}
		sort.Strings(metaKeys)
		meta := make([]string, 0, len(metaKeys))
		for _, k := range metaKeys {
			meta = append(meta, k+"="+st.PandocMetadata[k])
		}
		argsJSON, err := cache.CanonicalJSON(stringSliceToAny(st.PandocArgs))
		if err != nil {
			return "", fmt.Errorf("cache key: marshal pandoc args: %w", err)
		}
		return cache.HashStrings("pandoc",
			st.Binary,
			st.PandocFrom,
			st.PandocTo,
			outRel,
			coverRel,
			strings.Join(e.hashMediaFiles(media), "\n"),
			strings.Join(meta, "\n"),
			string(argsJSON),
		), nil
	case StageTypeMix:
		// (Bug fix) mix was cacheable but had no key branch, so it fell
		// through to default's empty key — silently never cached / collision
		// risk. Key on the script bytes (segment order, chapters) + every
		// referenced media file's content + the rendered output/metadata.
		scriptBytes, mediaHashes, outRel, metaParts, err := e.scriptStageKeyParts(st, []string{st.IntroMusic, st.OutroMusic, st.CoverImage}, mixMediaPaths, extra)
		if err != nil {
			return "", fmt.Errorf("mix cache key: %w", err)
		}
		parts := []string{
			string(scriptBytes),
			strings.Join(mediaHashes, "\n"),
			outRel,
			fmt.Sprintf("%g", st.LoudnessTarget),
			strings.Join(metaParts, "\n"),
		}
		// Gated because HashStrings is positional (see the comfyui
		// input_images comment above): the empty default resolves to
		// "ffmpeg" in the executor, so only a non-default binary needs to
		// change the key, and existing entries keep their keys.
		if st.Binary != "" {
			parts = append(parts, "binary:"+st.Binary)
		}
		return cache.HashStrings("mix", parts...), nil
	case StageTypeShort:
		scriptBytes, mediaHashes, outRel, metaParts, err := e.scriptStageKeyParts(st, nil, shortMediaPaths, extra)
		if err != nil {
			return "", fmt.Errorf("short cache key: %w", err)
		}
		w, h, fps := normalizeVideoDims(firstNonZero(st.ShortWidth, 0), firstNonZero(st.ShortHeight, 0), firstNonZero(st.ShortFPS, 0))
		parts := []string{
			string(scriptBytes),
			strings.Join(mediaHashes, "\n"),
			outRel,
			fmt.Sprintf("%dx%d@%d", w, h, fps),
			fmt.Sprintf("%g", st.LoudnessTarget),
			strings.Join(metaParts, "\n"),
		}
		// stretch_video swaps the rendered filtergraph (freeze-frame tpad
		// vs setpts time-stretch) — visibly different output bytes, so it
		// must key. Both extras are gated because HashStrings is
		// positional: default configs (stretch off, binary "" = ffmpeg)
		// keep their historical keys.
		if st.ShortStretchVideo {
			parts = append(parts, "stretch_video")
		}
		if st.Binary != "" {
			parts = append(parts, "binary:"+st.Binary)
		}
		return cache.HashStrings("short", parts...), nil
	default:
		return "", nil
	}
}

// renderCacheBodyValue renders every string leaf of an opt-in webhook's
// inline body for cache keying, mirroring renderWebhookValue's type switch
// (including the map[any]any coercion and []any recursion) so the key stays
// mechanically in lockstep with what the executor actually sends. It renders
// via the plain renderTemplate binding: bodies that use the webhook-only
// {{ env }} helper fail at key time — the same pre-existing limitation the
// URL and header renders in the webhook branch already have.
func (e *Executor) renderCacheBodyValue(st *Stage, name string, v any, extra map[string]any) (any, error) {
	switch tv := v.(type) {
	case string:
		return renderTemplate(name, tv, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
	case map[string]any:
		out := make(map[string]any, len(tv))
		for k, val := range tv {
			r, err := e.renderCacheBodyValue(st, name+"."+k, val, extra)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case map[any]any:
		coerced := make(map[string]any, len(tv))
		for k, val := range tv {
			coerced[fmt.Sprint(k)] = val
		}
		return e.renderCacheBodyValue(st, name, coerced, extra)
	case []any:
		out := make([]any, len(tv))
		for i, item := range tv {
			r, err := e.renderCacheBodyValue(st, fmt.Sprintf("%s[%d]", name, i), item, extra)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// scriptStageKeyParts assembles the shared cache-key inputs for the
// script-driven binary stages (mix, short): the raw script bytes, the content
// hashes of every media file the script + the stage's optional path fields
// reference, the rendered output path, and the rendered metadata. mediaFromScript
// extracts the script-relative media paths for the specific script shape.
func (e *Executor) scriptStageKeyParts(st *Stage, optionalFields []string, mediaFromScript func([]byte) []string, extra map[string]any) (scriptBytes []byte, mediaHashes []string, outRel string, metaParts []string, err error) {
	scriptPath, err := renderTemplate(st.ID+":script_file", st.ScriptFile, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("render script_file: %w", err)
	}
	scriptPath = resolveInRunDir(scriptPath, e.RunDir)
	scriptBytes, _ = os.ReadFile(scriptPath) // empty when absent — folds in as ""

	media := mediaFromScript(scriptBytes)
	for _, f := range optionalFields {
		if f == "" {
			continue
		}
		r, rerr := renderTemplate(st.ID+":optfield", f, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
		if rerr == nil && strings.TrimSpace(r) != "" {
			media = append(media, r)
		}
	}
	mediaHashes = e.hashMediaFiles(media)

	outRel, err = renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("render output: %w", err)
	}

	if len(st.Metadata) > 0 {
		keys := make([]string, 0, len(st.Metadata))
		for k := range st.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			rv, rerr := renderTemplate(st.ID+":metadata:"+k, st.Metadata[k], st.Inputs, e.Inputs, e.snapshotPrior(st.Inputs), e.RunDir, extra)
			if rerr != nil {
				return nil, nil, "", nil, fmt.Errorf("render metadata %s: %w", k, rerr)
			}
			metaParts = append(metaParts, k+"="+rv)
		}
	}
	return scriptBytes, mediaHashes, outRel, metaParts, nil
}

// hashMediaFiles content-hashes each path (resolved against the run dir),
// folding an absent file in as a stable "missing:" marker so its later
// appearance still changes the key.
func (e *Executor) hashMediaFiles(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		ap := resolveInRunDir(strings.TrimSpace(p), e.RunDir)
		h, err := cache.HashFile(ap)
		if err != nil {
			out = append(out, "missing:"+ap)
			continue
		}
		out = append(out, h)
	}
	return out
}

// mixMediaPaths / shortMediaPaths pull the media file references out of each
// script shape for cache keying. Parse failures yield no paths (the raw
// script bytes already differ, so the key still changes).
func mixMediaPaths(b []byte) []string {
	var sc mixScript
	if json.Unmarshal(b, &sc) != nil {
		return nil
	}
	return sc.VoiceSegments
}

func shortMediaPaths(b []byte) []string {
	var sc shortScript
	if json.Unmarshal(b, &sc) != nil {
		return nil
	}
	paths := make([]string, 0, len(sc.Shots)*2+1)
	for _, s := range sc.Shots {
		paths = append(paths, s.Video, s.Audio)
	}
	if sc.BackgroundMusic != "" {
		paths = append(paths, sc.BackgroundMusic)
	}
	return paths
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
					// Fold a marker (mirroring the ffmpeg-inputs branch)
					// instead of collapsing to the no-images key: image_files
					// entries are explicit, so a missing one is a
					// misconfiguration the live executor will surface — the
					// key must not alias a stale no-image (or
					// different-missing-image) cache entry over that error.
					hashes = append(hashes, "missing:"+p)
					continue
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
		// A missing image_dir deliberately keys as "no images":
		// collectImages degrades to text-only inference on a missing dir
		// (foreach items legitimately lack an images/ subdir), so those
		// items must share cache entries with plain text-only runs rather
		// than fold a missing-marker that would split them.
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
	// Same resolution the live executors use: a cache hit writes to the
	// stage's output path without any executor running, so it needs the
	// containment rule as much as they do — more, since it is the one
	// write path where no subprocess would have failed first.
	outRel, err := stageOutputPath(st, in, extra)
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
	if err := writeOutputAtomically(st.ID, "cache materialize", outAbs, hit.Output); err != nil {
		return nil, err
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
