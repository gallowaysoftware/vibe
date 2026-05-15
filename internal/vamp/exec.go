package vamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// InferenceFunc runs a single stage's chat completion against baseURL.
// baseURL is the proxy root (e.g. http://127.0.0.1:9000) without /v1. When
// onToken is non-nil the implementation should stream incremental content
// deltas to it as they arrive; the returned string is still the full
// accumulated content.
type InferenceFunc func(ctx context.Context, baseURL, model, prompt string, params map[string]any, onToken StreamFunc) (string, error)

// StageType selects which executor handles a stage. Empty defaults to text.
type StageType string

const (
	// StageTypeText is the LLM chat-completion stage type. Empty Stage.Type
	// is treated as this in Run.
	StageTypeText StageType = "text"
	// StageTypeComfyUI is an image-generation stage that submits a ComfyUI
	// workflow to a vibe-managed ComfyUI backend and copies the rendered
	// file(s) into the run dir.
	StageTypeComfyUI StageType = "comfyui"
	// StageTypeAudio synthesizes speech by shelling out to a Piper TTS
	// binary. The stage does not talk to vibe and does not activate a
	// profile; the runner short-circuits profile activation for audio
	// groups since the underlying binary is a local subprocess.
	StageTypeAudio StageType = "audio"
	// StageTypeFFmpeg invokes ffmpeg as a local subprocess to combine prior
	// stage outputs (audio, images, video) into a final media file. Like
	// audio stages it does not talk to vibe and does not activate a
	// profile; the runner short-circuits profile activation for groups
	// whose stages are all profile-less subprocess types.
	StageTypeFFmpeg StageType = "ffmpeg"
	// StageTypeYouTube uploads a video file (typically produced by an
	// upstream ffmpeg stage) to YouTube via the YouTube Data API v3.
	// Like the subprocess stage types it does not activate a vibe profile;
	// it talks directly to Google's OAuth / upload endpoints over HTTPS.
	StageTypeYouTube StageType = "youtube"
)

// StageExecutor implements the run of a single stage instance. The receiver
// is shared across stages of the same type, so any per-stage state belongs
// in StageInput, not on the executor.
type StageExecutor interface {
	Execute(ctx context.Context, in StageInput) (*StageOutput, error)
}

// StageInput is the per-invocation packet handed to a StageExecutor. For
// foreach stages the runner calls Execute once per item with Item / ItemIdx
// bound; for ordinary stages Item is nil and ItemIdx is 0.
//
// BaseURL and ModelID are populated by the DAG scheduler from the active
// profile; they are only meaningful for stage types that talk to vibe's
// inference endpoint (today, the text executor). PipelineDir is the
// directory the pipeline YAML was loaded from, used to resolve relative
// prompt_file paths.
type StageInput struct {
	Stage       *Stage
	Inputs      map[string]string       // CLI inputs
	Prior       map[string]*stageResult // outputs of already-completed stages
	RunDir      string
	PipelineDir string
	Log         io.Writer          // for token streaming when group size == 1
	Vibe        *vibeclient.Client // available to TextExecutor
	Item        any                // foreach item bound here (nil when not in a foreach)
	ItemIdx     int                // foreach index (0 when not in a foreach)

	// BaseURL is the inference root for this invocation (e.g.
	// http://127.0.0.1:9000); /v1 is NOT included. Populated by the
	// scheduler from the currently-active vibe profile. Text stages talk
	// here; comfyui stages talk to BackendAddr instead.
	BaseURL string
	// ModelID is the chat-completion model id for this invocation, resolved
	// once per profile activation and cached. Only meaningful for text stages.
	ModelID string
	// BackendAddr is the raw backend URL the active vibe profile is running
	// (e.g. "http://127.0.0.1:8188" for a ComfyUI backend). Stage types that
	// talk directly to a non-OpenAI backend (today: comfyui) use this.
	BackendAddr string
}

// StageOutput is what an executor returns for ONE invocation (one item if
// foreach). The DAG runner aggregates per-item outputs into the existing
// stageResult{Output, Outputs} shape; executors don't need to know about that.
type StageOutput struct {
	Text  string   // for text stages
	Files []string // for binary outputs (Phase 2+); paths relative to RunDir
}

// stageResult holds the output(s) of a completed stage. For ordinary stages
// only Output is meaningful; for foreach stages Outputs holds the per-item
// content in input-array order and Output is a deterministic newline-joined
// concatenation so legacy `.stages.<id>.output` references keep working.
type stageResult struct {
	Output  string
	Outputs []string
}

// defaultMaxForeachConcurrency caps in-flight items inside a single foreach
// stage when the caller doesn't set Executor.MaxForeachConcurrency. Four is a
// pragmatic default: large enough to overlap latency-bound work (e.g. ComfyUI
// renders against the same loaded model) without flooding the backend.
const defaultMaxForeachConcurrency = 4

// Executor runs a Pipeline end-to-end against vibe.
type Executor struct {
	Pipeline     *Pipeline
	PipelineDir  string // for resolving prompt_file relative paths
	Capabilities *Capabilities
	Vibe         *vibeclient.Client
	Inputs       map[string]string
	RunDir       string
	Inference    InferenceFunc // defaults to a real chat-completion client
	Log          io.Writer

	// PipelineSource is the raw bytes of the pipeline YAML file the caller
	// loaded. It powers two run-dir artifacts: pipeline.yaml.snapshot
	// (written verbatim at run start so resume can detect pipeline drift)
	// and the resume hash check itself. When nil the snapshot file is
	// written empty and resume can't validate the pipeline hasn't changed
	// since the original run.
	PipelineSource []byte

	// ResumeDir, when non-empty, switches Run into resume mode. The
	// directory MUST exist and be a previous vamp run dir (i.e. it
	// already contains pipeline.yaml.snapshot + per-stage outputs from
	// the original run). RunDir is set from ResumeDir at Run start so
	// every stage's output path resolves to the existing on-disk files.
	// Resume checks each stage's rendered output path(s) for an existing
	// non-empty file; matching stages are loaded back into stageOutputs
	// and skipped during scheduling. Mismatching / missing files cause
	// the stage to run normally.
	ResumeDir string

	// ResumeForce overrides the pipeline-drift safety check. When the
	// pipeline.yaml.snapshot bytes hash differently from PipelineSource,
	// Run normally aborts with an instruction to either pass the original
	// pipeline file or omit --resume. Set ResumeForce to bypass that
	// check (the user explicitly told us they know the schema diverged).
	ResumeForce bool

	// MaxForeachConcurrency bounds the number of foreach items that may be
	// running concurrently inside a single stage. Zero or negative uses
	// defaultMaxForeachConcurrency. This is independent of the DAG
	// scheduler's per-wave/per-capability concurrency above; it only governs
	// fan-out *within* one Stage.Foreach invocation.
	MaxForeachConcurrency int

	// in-memory state populated during Run
	mu                 sync.Mutex // guards stageOutputs and the model cache
	stageOutputs       map[string]*stageResult
	completedStages    map[string]bool // stage ids whose outputs were loaded from a prior run via Resume
	cachedModelID      string
	cachedModelProfile string
	// logMu serializes writes to Log from concurrent stages so buffered
	// stage outputs land contiguously even when status lines from the
	// scheduler are interleaved.
	logMu sync.Mutex

	// registry maps StageType -> executor. Built once in Run and shared by
	// every stage; per-stage state must travel via StageInput.
	registry map[StageType]StageExecutor
}

func defaultInference() InferenceFunc {
	cc := &ChatCompletion{}
	return cc.Call
}

// Run executes the pipeline as a DAG. Stages whose dependencies are satisfied
// run in waves; within each wave they are grouped by capability so each group
// shares a single profile activation, and stages in a group execute
// concurrently. Earlier stage outputs are exposed to dependents via
// .stages.<id>.output in their templates.
func (e *Executor) Run(ctx context.Context) error {
	if e.Inference == nil {
		e.Inference = defaultInference()
	}
	// Resume mode: the run dir already exists, snapshot drift is checked
	// against the prior run's pipeline.yaml.snapshot, and per-stage outputs
	// from disk seed stageOutputs so completed work is skipped. We do this
	// before MkdirAll so the drift check sees the original snapshot bytes;
	// MkdirAll is idempotent on an existing dir.
	if e.ResumeDir != "" {
		e.RunDir = e.ResumeDir
		if err := e.checkResumeSnapshot(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(e.RunDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	if err := e.snapshot(); err != nil {
		return fmt.Errorf("snapshot run inputs: %w", err)
	}
	e.completedStages = make(map[string]bool)

	// Build the per-type executor registry. Per-stage routing happens through
	// Stage.Type (empty defaults to text); the executor's per-call deps travel
	// via StageInput.
	e.registry = map[StageType]StageExecutor{
		StageTypeText:    &textExecutor{inference: e.Inference},
		StageTypeComfyUI: &comfyuiExecutor{pollInterval: time.Second},
		StageTypeAudio:   &audioExecutor{},
		StageTypeFFmpeg:  &ffmpegExecutor{},
		StageTypeYouTube: &youtubeExecutor{},
	}

	// Stage lookup and dependency counts for wave-based scheduling.
	byID := make(map[string]*Stage, len(e.Pipeline.Stages))
	indeg := make(map[string]int, len(e.Pipeline.Stages))
	dependents := make(map[string][]string, len(e.Pipeline.Stages))
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		byID[st.ID] = st
		indeg[st.ID] = len(st.Inputs)
	}
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		for _, dep := range st.Inputs {
			dependents[dep] = append(dependents[dep], st.ID)
		}
	}

	e.stageOutputs = make(map[string]*stageResult, len(e.Pipeline.Stages))
	remaining := len(e.Pipeline.Stages)
	wave := 0
	for remaining > 0 {
		// Collect every stage whose deps are satisfied. Sort by id so the
		// scheduling order is deterministic across runs.
		var ready []*Stage
		for id, deg := range indeg {
			if deg == 0 {
				ready = append(ready, byID[id])
			}
		}
		if len(ready) == 0 {
			// All remaining stages have unmet deps but none are runnable;
			// Validate() should have prevented this. Bail with a clear error.
			return fmt.Errorf("scheduler deadlock: %d stages remain with unmet dependencies", remaining)
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		// Remove them from indeg so the next iteration doesn't re-pick them.
		for _, st := range ready {
			delete(indeg, st.ID)
		}
		wave++

		// Group ready stages by capability, preserving alphabetical capability
		// order for determinism.
		groups := make(map[string][]*Stage)
		var capOrder []string
		for _, st := range ready {
			if _, ok := groups[st.Capability]; !ok {
				capOrder = append(capOrder, st.Capability)
			}
			groups[st.Capability] = append(groups[st.Capability], st)
		}
		sort.Strings(capOrder)

		for _, capName := range capOrder {
			group := groups[capName]
			e.logf("wave %d: capability %q running %d stage(s)", wave, capName, len(group))
			if err := e.runGroup(ctx, capName, group); err != nil {
				return err
			}
			// Decrement indegree for everything that depended on stages in
			// this group; their outputs are now in stageOutputs.
			for _, st := range group {
				for _, child := range dependents[st.ID] {
					if _, ok := indeg[child]; ok {
						indeg[child]--
					}
				}
				remaining--
			}
		}
	}
	e.logf("pipeline %q finished, outputs in %s", e.Pipeline.Name, e.RunDir)
	return nil
}

// runGroup activates `capability`'s profile once, then runs every stage in the
// group. Single-stage groups stream tokens live to Log; multi-stage groups
// buffer per-stage tokens and emit them contiguously after each stage
// completes to keep concurrent output readable.
func (e *Executor) runGroup(ctx context.Context, capability string, group []*Stage) error {
	// Resume pre-pass: try to seed stageOutputs for each group stage from the
	// run dir's existing files. Stages whose outputs are all present drop out
	// of the working group; if the group empties entirely we skip profile
	// activation altogether (the whole point of resume — don't even talk to
	// vibe for work that's already done).
	if e.ResumeDir != "" {
		remaining := make([]*Stage, 0, len(group))
		for _, st := range group {
			resumed, err := e.tryResumeStage(st)
			if err != nil {
				return fmt.Errorf("resume stage %s: %w", st.ID, err)
			}
			if resumed {
				e.logf("  -> stage %q: already completed, skipping (resume)", st.ID)
				e.mu.Lock()
				e.completedStages[st.ID] = true
				e.mu.Unlock()
				continue
			}
			remaining = append(remaining, st)
		}
		if len(remaining) == 0 {
			e.logf("  -> capability %q: all stages already completed, skipping activation", capability)
			return nil
		}
		group = remaining
	}
	// Some stage types (audio, ffmpeg) are local subprocesses; they don't
	// talk to a vibe-managed backend, don't need a profile activation, and
	// may legitimately ship with an empty capability. Short-circuit profile
	// resolution when every stage in the group is one of these so the
	// scheduler doesn't demand a capability mapping for a stage type that
	// doesn't use one.
	allProfileless := true
	for _, st := range group {
		if stageRequiresVibeProfile(st) {
			allProfileless = false
			break
		}
	}
	if allProfileless {
		e.logf("  -> subprocess group (%d stage(s)): no profile activation", len(group))
		if len(group) == 1 {
			st := group[0]
			if err := e.executeStage(ctx, st, "", "", "", nil); err != nil {
				return fmt.Errorf("stage %s: %w", st.ID, err)
			}
			return nil
		}
		groupCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var wg sync.WaitGroup
		errs := make([]error, len(group))
		for i, st := range group {
			i, st := i, st
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := &bytes.Buffer{}
				err := e.executeStage(groupCtx, st, "", "", "", buf)
				e.flushStageLog(st.ID, buf.Bytes())
				if err != nil {
					errs[i] = fmt.Errorf("stage %s: %w", st.ID, err)
					cancel()
				}
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	}

	profile, err := e.Capabilities.Profile(capability)
	if err != nil {
		return err
	}
	e.logf("  -> activating profile %q", profile)
	status, err := e.Vibe.EnsureActive(ctx, profile)
	if err != nil {
		return fmt.Errorf("activate %q: %w", profile, err)
	}
	if !status.Ready {
		return fmt.Errorf("profile %q is not ready", profile)
	}
	// Only resolve the chat-completion model id when at least one stage in
	// this group needs it. ComfyUI backends don't speak OpenAI /v1/models, so
	// a pure-comfyui group would fail this check unnecessarily.
	needsModelID := false
	for _, st := range group {
		if stageTypeOrDefault(st) == StageTypeText {
			needsModelID = true
			break
		}
	}
	var modelID string
	if needsModelID {
		modelID, err = e.modelIDForCurrent(ctx, status)
		if err != nil {
			return fmt.Errorf("resolve model id: %w", err)
		}
	}
	baseURL := strings.TrimSuffix(status.ProxyAddr, "/v1")
	backendAddr := status.BackendAddr

	// Single-stage group: preserve the live-token UX by streaming directly
	// to Log, exactly as the sequential path used to. (A foreach stage with
	// more than one item still buffers per-item internally — see executeStage.)
	if len(group) == 1 {
		st := group[0]
		if err := e.executeStage(ctx, st, baseURL, modelID, backendAddr, nil); err != nil {
			return fmt.Errorf("stage %s: %w", st.ID, err)
		}
		return nil
	}

	// Multi-stage group: cancel siblings on first failure, aggregate every
	// error, and buffer per-stage tokens for readable output.
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(group))
	for i, st := range group {
		i, st := i, st
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := &bytes.Buffer{}
			err := e.executeStage(groupCtx, st, baseURL, modelID, backendAddr, buf)
			// Emit the buffered output as a contiguous block under a
			// stage header so concurrent streams don't interleave.
			e.flushStageLog(st.ID, buf.Bytes())
			if err != nil {
				errs[i] = fmt.Errorf("stage %s: %w", st.ID, err)
				// Cancel siblings so we fail fast instead of running every
				// stage to completion after one is doomed.
				cancel()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// executeStage runs a single stage. For ordinary stages it invokes the
// registered StageExecutor once and stores its output; for foreach stages it
// fans out one Execute call per JSON-array item, runs them in parallel against
// the same profile activation, and aggregates the per-item outputs.
//
// Resume skipping happens upstream in runGroup (so an all-resumed group can
// skip profile activation entirely); by the time we get here, every stage in
// the group needs a real executor call.
func (e *Executor) executeStage(ctx context.Context, st *Stage, baseURL, modelID, backendAddr string, tokenSink io.Writer) error {
	exec, err := e.executorFor(st)
	if err != nil {
		return err
	}
	if st.Foreach != nil {
		return e.executeForeachStage(ctx, st, exec, baseURL, modelID, backendAddr, tokenSink)
	}

	in := e.makeStageInput(st, baseURL, modelID, backendAddr, tokenSink, nil, 0)
	out, err := exec.Execute(ctx, in)
	if err != nil {
		return err
	}

	// ComfyUI stages own their own output-path rendering + writing because
	// the executor copies a binary file (or files) to disk inside Execute and
	// reports the result via out.Files. Treat out.Files (when set) as the
	// canonical record and keep stageOutputs.Output as the joined paths so
	// downstream `.stages.<id>.output` references still resolve.
	if len(out.Files) > 0 {
		e.mu.Lock()
		e.stageOutputs[st.ID] = &stageResult{Output: strings.Join(out.Files, "\n")}
		e.mu.Unlock()
		return nil
	}

	outPath, err := e.renderOutputPath(st, nil)
	if err != nil {
		return fmt.Errorf("render output path: %w", err)
	}
	if err := writeFile(filepath.Join(e.RunDir, outPath), out.Text); err != nil {
		return err
	}
	e.mu.Lock()
	e.stageOutputs[st.ID] = &stageResult{Output: out.Text}
	e.mu.Unlock()
	return nil
}

// executeForeachStage implements the fan-out semantics for stages with a
// non-nil Foreach. The items list is read from the upstream stage's JSON
// output (referenced by Foreach.From). Each item runs in its own goroutine
// through the registered executor and writes its own output file.
func (e *Executor) executeForeachStage(ctx context.Context, st *Stage, exec StageExecutor, baseURL, modelID, backendAddr string, tokenSink io.Writer) error {
	items, err := e.resolveForeachItems(st)
	if err != nil {
		return fmt.Errorf("resolve foreach: %w", err)
	}
	if len(items) == 0 {
		// Nothing to fan out over. Still record an empty result so dependents
		// can reference .stages.<id>.outputs without a missing-key error.
		e.mu.Lock()
		e.stageOutputs[st.ID] = &stageResult{Output: "", Outputs: nil}
		e.mu.Unlock()
		e.logf("  -> stage %q: foreach array empty, no items to run", st.ID)
		return nil
	}
	e.logf("  -> stage %q: foreach fanning out %d item(s)", st.ID, len(items))

	// Pre-render each item's output path so we can detect collisions before
	// launching any executor work.
	outPaths := make([]string, len(items))
	seenPaths := make(map[string]int, len(items))
	for i, item := range items {
		// `.i` is the per-iteration index, also bound in the prompt template
		// below; lets users template paths like `assets/img_{{.i}}.png`.
		extra := map[string]any{st.Foreach.Var: item, "i": i}
		path, err := e.renderOutputPath(st, extra)
		if err != nil {
			return fmt.Errorf("render output path for item %d: %w", i, err)
		}
		if prev, ok := seenPaths[path]; ok {
			return fmt.Errorf("stage %s: foreach output path collision: items %d and %d both produce %q", st.ID, prev, i, path)
		}
		seenPaths[path] = i
		outPaths[i] = path
	}

	// Streaming policy:
	//   - n == 1 AND tokenSink == nil (single-stage group): stream live to Log.
	//   - otherwise: buffer per-item and flush with [index] headers so the
	//     log stays readable when multiple items run in parallel.
	singleLive := len(items) == 1 && tokenSink == nil
	outputs := make([]string, len(items))
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(items))
	// sinkMu serializes writes to a caller-owned tokenSink (only used when
	// this foreach stage is nested inside a parallel multi-stage group).
	// Concurrent per-item flushes would otherwise race on the same buffer.
	var sinkMu sync.Mutex
	// Bound in-flight items with a buffered-channel semaphore. The cap is
	// Executor.MaxForeachConcurrency (defaulted), independent of any
	// per-wave concurrency the DAG scheduler enforces above us. Items still
	// own their index-keyed slot in outputs/errs, so completion order does
	// not affect the final `stageResult.Outputs` ordering.
	maxConc := e.MaxForeachConcurrency
	if maxConc <= 0 {
		maxConc = defaultMaxForeachConcurrency
	}
	if maxConc > len(items) {
		maxConc = len(items)
	}
	sem := make(chan struct{}, maxConc)
	for i := range items {
		i := i
		// Acquire BEFORE goroutine launch so we don't spawn N goroutines for
		// a 1000-item foreach with cap=4. Honor cancellation while waiting
		// for a slot: if a sibling has already errored and called cancel(),
		// we want to bail out of the wait promptly. Treat acquisition
		// failure as a recorded cancellation error so errors.Join still
		// surfaces the original sibling failure (errs[i] for siblings that
		// did run) and downstream callers see the partial-cancel via
		// errors.Is(err, context.Canceled).
		select {
		case sem <- struct{}{}:
		case <-groupCtx.Done():
			errs[i] = fmt.Errorf("item %d: %w", i, groupCtx.Err())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var itemSink io.Writer
			var buf *bytes.Buffer
			switch {
			case singleLive:
				// nil here means "use Log via logMu" — makeStageInput handles
				// that translation.
				itemSink = nil
			case tokenSink != nil:
				buf = &bytes.Buffer{}
				itemSink = buf
			default:
				buf = &bytes.Buffer{}
				itemSink = buf
			}
			in := e.makeStageInput(st, baseURL, modelID, backendAddr, itemSink, items[i], i)
			out, runErr := exec.Execute(groupCtx, in)
			if buf != nil {
				if tokenSink != nil {
					sinkMu.Lock()
					fmt.Fprintf(tokenSink, "--- item %d ---\n", i)
					_, _ = tokenSink.Write(buf.Bytes())
					_, _ = tokenSink.Write([]byte("\n"))
					sinkMu.Unlock()
				} else {
					e.flushStageLog(fmt.Sprintf("%s[%d]", st.ID, i), buf.Bytes())
				}
			}
			if runErr != nil {
				errs[i] = fmt.Errorf("item %d: %w", i, runErr)
				// Cancel siblings so in-flight items observe ctx.Done()
				// promptly and pending acquirers above bail out of the
				// semaphore wait. We do NOT cancel the outer ctx — the DAG
				// scheduler owns whether other stages keep running.
				cancel()
				return
			}
			// Binary-output stages (comfyui today) already wrote their file(s)
			// from inside Execute and reported the paths in out.Files; the
			// runner only needs to record per-item output for downstream
			// `.outputs` references. Text stages return text we still need to
			// write to the pre-rendered outPath.
			if len(out.Files) > 0 {
				outputs[i] = strings.Join(out.Files, "\n")
			} else {
				if err := writeFile(filepath.Join(e.RunDir, outPaths[i]), out.Text); err != nil {
					errs[i] = fmt.Errorf("item %d: write output: %w", i, err)
					cancel()
					return
				}
				outputs[i] = out.Text
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}

	// Aggregate: .output is a deterministic newline-joined concatenation of
	// per-item outputs (in input-array order) so legacy `.stages.<id>.output`
	// references degrade gracefully; .outputs is the per-item slice.
	combined := strings.Join(outputs, "\n\n")
	e.mu.Lock()
	e.stageOutputs[st.ID] = &stageResult{Output: combined, Outputs: outputs}
	e.mu.Unlock()
	return nil
}

// makeStageInput assembles a StageInput for one invocation. tokenSink is the
// caller-provided buffer for token streaming, or nil to stream directly to
// e.Log under logMu. item / itemIdx are bound for foreach invocations.
func (e *Executor) makeStageInput(st *Stage, baseURL, modelID, backendAddr string, tokenSink io.Writer, item any, itemIdx int) StageInput {
	return StageInput{
		Stage:       st,
		Inputs:      e.Inputs,
		Prior:       e.snapshotPrior(st.Inputs),
		RunDir:      e.RunDir,
		PipelineDir: e.PipelineDir,
		Log:         e.tokenLog(tokenSink),
		Vibe:        e.Vibe,
		Item:        item,
		ItemIdx:     itemIdx,
		BaseURL:     baseURL,
		ModelID:     modelID,
		BackendAddr: backendAddr,
	}
}

// stageTypeOrDefault returns st.Type with the empty-default rule applied.
// Centralized so the dispatcher and the runGroup pre-check stay in sync.
func stageTypeOrDefault(st *Stage) StageType {
	if st.Type == "" {
		return StageTypeText
	}
	return st.Type
}

// stageRequiresVibeProfile reports whether the stage's executor needs a vibe
// profile activated before Execute. Subprocess-only types (audio, ffmpeg) and
// the network-only youtube type don't talk to a vibe-managed backend, so the
// scheduler skips EnsureActive for groups consisting entirely of these stages.
func stageRequiresVibeProfile(st *Stage) bool {
	switch stageTypeOrDefault(st) {
	case StageTypeAudio, StageTypeFFmpeg, StageTypeYouTube:
		return false
	default:
		return true
	}
}

// executorFor selects the StageExecutor for st based on its type. An unknown
// type produces a clear error so Phase 2 stage types that ship without a
// matching executor fail loudly instead of silently defaulting to text.
func (e *Executor) executorFor(st *Stage) (StageExecutor, error) {
	t := stageTypeOrDefault(st)
	exec, ok := e.registry[t]
	if !ok {
		return nil, fmt.Errorf("stage %s: no executor registered for type %q", st.ID, t)
	}
	return exec, nil
}

// snapshotPrior returns a copy of the executor's stageOutputs limited to the
// declared dependency ids. Callers receive a snapshot so concurrent stage
// completions can't mutate the map under them while they render templates.
func (e *Executor) snapshotPrior(deps []string) map[string]*stageResult {
	out := make(map[string]*stageResult, len(deps))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, dep := range deps {
		if res, ok := e.stageOutputs[dep]; ok {
			out[dep] = res
		}
	}
	return out
}

// tokenLog returns the io.Writer that an executor should hand to its
// inference call for live token streaming. When tokenSink is non-nil the
// caller (a buffered parallel group) owns it directly; when nil and Log is
// set we wrap Log with a writer that takes logMu so live tokens can't
// interleave with scheduler status lines.
func (e *Executor) tokenLog(tokenSink io.Writer) io.Writer {
	if tokenSink != nil {
		return tokenSink
	}
	if e.Log == nil {
		return nil
	}
	return &lockedWriter{mu: &e.logMu, w: e.Log}
}

// lockedWriter serializes Write calls on an underlying writer with a shared
// mutex. Used so live token deltas can't interleave with scheduler logf lines
// on the same Log sink.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// textExecutor handles StageTypeText stages: render the prompt template, call
// vibe's OpenAI-compatible inference endpoint with optional token streaming,
// validate the JSON output_format if requested, and return the response.
//
// The inference function is held as a field rather than passed through
// StageInput because (a) it's a stable per-Executor dependency, not per-stage
// routing info, and (b) the StageInput.Vibe client is the gRPC control plane,
// distinct from the OpenAI-compatible chat-completion HTTP client.
type textExecutor struct {
	inference InferenceFunc
}

func (t *textExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}
	prompt, err := renderPrompt(st, in.PipelineDir, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("render prompt: %w", err)
	}

	var onToken StreamFunc
	if in.Log != nil {
		onToken = func(delta string) {
			_, _ = in.Log.Write([]byte(delta))
		}
	}
	out, err := t.inference(ctx, in.BaseURL+"/v1", in.ModelID, prompt, st.Params, onToken)
	if err != nil {
		return nil, fmt.Errorf("inference: %w", err)
	}
	if in.Log != nil {
		// Ensure the next status line starts on its own line after a
		// live-streamed completion.
		_, _ = in.Log.Write([]byte("\n"))
	}

	if st.OutputFormat == "json" {
		if err := validateJSON(out); err != nil {
			return nil, fmt.Errorf("stage output is not valid JSON: %w", err)
		}
	}
	return &StageOutput{Text: out}, nil
}

// tryResumeStage attempts to load this stage's output(s) from the run dir
// without invoking the real executor. Returns (true, nil) iff every output
// file required by the stage exists with size > 0 (and, for json stages,
// parses as valid JSON) — in which case stageOutputs[st.ID] is populated and
// the scheduler can skip the stage. Returns (false, nil) for any partial /
// missing case so the caller falls through to the normal execution path.
// An error is only returned for unexpected I/O failures.
//
// Output integrity: the spec mandates "size > 0". We layer on one cheap extra
// check — stages declared as output_format: json must contain valid JSON —
// because a truncated/corrupted JSON output from a crashed prior run would
// otherwise satisfy "size > 0" and poison every downstream foreach. Any
// stronger check (checksum sidecars, stage-specific schema) is deferred to a
// follow-up; the size-plus-JSON heuristic catches the common crash patterns
// without persisting extra state.
func (e *Executor) tryResumeStage(st *Stage) (bool, error) {
	if st.Foreach != nil {
		return e.tryResumeForeachStage(st)
	}
	outPath, err := e.renderOutputPath(st, nil)
	if err != nil {
		// Output template may reference an upstream's .output that won't
		// be available until a prior resumed-or-run stage populated
		// stageOutputs. If template rendering fails we can't decide
		// resumability — bail out to normal execution rather than
		// surfacing a confusing render error.
		return false, nil
	}
	full := filepath.Join(e.RunDir, outPath)
	body, ok, err := readNonEmpty(full)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	// Binary stages (comfyui/audio) only record the relative path in
	// stageResult.Output; templates that reference `.stages.<id>.output`
	// for those stages already see a path, not file contents. YouTube is
	// the exception: its "output" is the watch URL we wrote to disk, and
	// downstream stages expect the URL string itself — same shape as text.
	t := stageTypeOrDefault(st)
	if t != StageTypeText && t != StageTypeYouTube {
		e.mu.Lock()
		e.stageOutputs[st.ID] = &stageResult{Output: outPath}
		e.mu.Unlock()
		return true, nil
	}
	if st.OutputFormat == "json" {
		if err := validateJSON(string(body)); err != nil {
			// Treat as incomplete; downstream foreach would otherwise
			// blow up on the corrupted JSON.
			return false, nil
		}
	}
	e.mu.Lock()
	e.stageOutputs[st.ID] = &stageResult{Output: string(body)}
	e.mu.Unlock()
	return true, nil
}

// tryResumeForeachStage is the foreach analogue of tryResumeStage. Phase 1
// per-item granularity: if ANY rendered per-item output is missing or empty,
// the whole foreach reruns. That's intentional — partial recovery for foreach
// is gated on us tracking per-item state across runs, which is a future
// polish.
func (e *Executor) tryResumeForeachStage(st *Stage) (bool, error) {
	items, err := e.resolveForeachItems(st)
	if err != nil {
		// The upstream's output is missing or unparseable in this run dir;
		// rerun the foreach so the upstream's failure (if any) surfaces
		// naturally. Don't propagate the error — the upstream's own resume
		// check already had its chance to bail out.
		return false, nil
	}
	if len(items) == 0 {
		// Empty foreach is trivially "complete" with no outputs. Mirror
		// the empty-array behaviour from executeForeachStage exactly so
		// downstream .outputs references resolve.
		e.mu.Lock()
		e.stageOutputs[st.ID] = &stageResult{Output: "", Outputs: nil}
		e.mu.Unlock()
		return true, nil
	}
	outputs := make([]string, len(items))
	// YouTube records the watch URL as the file body; downstream stages see
	// the URL string the same way they'd see a text-stage completion, so we
	// treat youtube like text for resume purposes.
	rt := stageTypeOrDefault(st)
	isText := rt == StageTypeText || rt == StageTypeYouTube
	for i, item := range items {
		extra := map[string]any{st.Foreach.Var: item, "i": i}
		path, err := e.renderOutputPath(st, extra)
		if err != nil {
			return false, nil
		}
		full := filepath.Join(e.RunDir, path)
		body, ok, err := readNonEmpty(full)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if isText {
			if st.OutputFormat == "json" {
				if err := validateJSON(string(body)); err != nil {
					return false, nil
				}
			}
			outputs[i] = string(body)
		} else {
			outputs[i] = path
		}
	}
	combined := strings.Join(outputs, "\n\n")
	e.mu.Lock()
	e.stageOutputs[st.ID] = &stageResult{Output: combined, Outputs: outputs}
	e.mu.Unlock()
	return true, nil
}

// readNonEmpty reads path and reports whether the file exists with non-zero
// size. Missing files report (nil, false, nil); other I/O errors propagate.
func readNonEmpty(path string) ([]byte, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if info.Size() == 0 {
		return nil, false, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// resolveForeachItems reads the upstream stage's stored JSON output (the stage
// named by Foreach.From) and parses it as an array. Items may be strings,
// numbers, booleans, or objects; null is rejected as inadmissible.
func (e *Executor) resolveForeachItems(st *Stage) ([]any, error) {
	from := st.Foreach.From
	e.mu.Lock()
	res, ok := e.stageOutputs[from]
	e.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("foreach.from %q has no output yet (scheduler bug or unresolved dependency)", from)
	}
	rendered := strings.TrimSpace(res.Output)
	if rendered == "" {
		return nil, fmt.Errorf("foreach upstream %q produced empty output", from)
	}
	var raw any
	if err := json.Unmarshal([]byte(rendered), &raw); err != nil {
		preview := rendered
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("foreach value is not valid JSON: %w (got %q)", err, preview)
	}
	switch v := raw.(type) {
	case []any:
		return v, nil
	case map[string]any:
		// Convenience: {"items": [...]} unwraps to the inner array. This
		// lets a producer wrap its JSON array in an object without forcing
		// downstream stages to know.
		inner, ok := v["items"]
		if !ok {
			return nil, fmt.Errorf("foreach object must have an \"items\" array")
		}
		arr, ok := inner.([]any)
		if !ok {
			return nil, fmt.Errorf("foreach object's \"items\" field must be an array, got %T", inner)
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("foreach value must be a JSON array or {\"items\":[...]} object, got %T", raw)
	}
}

// flushStageLog writes a per-stage header followed by the stage's buffered
// token stream and a trailing newline. The whole block is emitted under
// logMu so two concurrent stages can't interleave their flushes.
func (e *Executor) flushStageLog(id string, body []byte) {
	if e.Log == nil {
		return
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	fmt.Fprintf(e.Log, "=== stage %s ===\n", id)
	_, _ = e.Log.Write(body)
	_, _ = e.Log.Write([]byte("\n"))
}

// renderPrompt is the package-level prompt renderer used by stage executors.
// It loads prompt_file content from disk when needed (resolving relative
// paths against pipelineDir) and then renders the resulting template.
//
// extra is merged on top of the standard template data (inputs/stages/runDir)
// so foreach can inject the current item under its Var name.
func renderPrompt(st *Stage, pipelineDir string, cliInputs map[string]string, prior map[string]*stageResult, runDir string, extra map[string]any) (string, error) {
	raw := st.Prompt
	if raw == "" && st.PromptFile != "" {
		path := st.PromptFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(pipelineDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read prompt_file %s: %w", path, err)
		}
		raw = string(data)
	}
	return renderTemplate(st.ID, raw, st.Inputs, cliInputs, prior, runDir, extra)
}

// renderOutputPath renders the stage's Output template against the executor's
// current state. The returned path is always relative to RunDir.
func (e *Executor) renderOutputPath(st *Stage, extra map[string]any) (string, error) {
	prior := e.snapshotPrior(st.Inputs)
	return renderTemplate(st.ID+":output", st.Output, st.Inputs, e.Inputs, prior, e.RunDir, extra)
}

// renderTemplate is the common rendering routine for prompt / output
// templates. It exposes inputs, stages (with both .output and .outputs), and
// runDir, plus any extra bindings supplied by the caller (foreach injects
// the per-item value under its Var name). Only stage ids listed in deps are
// exposed under .stages, matching the dependency declared in YAML.
func renderTemplate(name, raw string, deps []string, cliInputs map[string]string, prior map[string]*stageResult, runDir string, extra map[string]any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Funcs(templateFuncs()).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	stages := make(map[string]map[string]any, len(deps))
	for _, dep := range deps {
		res, ok := prior[dep]
		if !ok {
			return "", fmt.Errorf("input %q has no output yet (scheduler bug or unresolved dependency)", dep)
		}
		// Always expose .output. .outputs is the per-item slice for foreach
		// stages and nil otherwise; range over a nil slice is a no-op, so
		// downstream `{{range .stages.x.outputs}}` works in both cases.
		stages[dep] = map[string]any{
			"output":  res.Output,
			"outputs": res.Outputs,
		}
	}
	data := map[string]any{
		"inputs": cliInputs,
		"stages": stages,
		"runDir": runDir,
	}
	for k, v := range extra {
		data[k] = v
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// templateFuncs returns the function map registered on every stage template.
// Currently only slugify; intentionally small to keep templates predictable.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"slugify": slugify,
	}
}

// slugify converts an arbitrary value to a filesystem-safe slug: lowercase
// ASCII alphanumerics and single hyphens, leading/trailing hyphens trimmed,
// capped at 60 bytes. Intended for foreach output paths so a stage like
// `output: hooks/{{.title | slugify}}.md` produces deterministic, distinct
// filenames per item.
func slugify(v any) string {
	in := strings.ToLower(fmt.Sprint(v))
	var b strings.Builder
	b.Grow(len(in))
	prevHyphen := false
	for _, r := range in {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		// Everything else (whitespace, punctuation, unicode) becomes a single
		// hyphen separator; consecutive separators collapse.
		if !prevHyphen && b.Len() > 0 {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	return s
}

// writeFile creates the parent directory and writes content atomically enough
// for our purposes (truncating write).
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func (e *Executor) modelIDForCurrent(ctx context.Context, status *vibev1.Status) (string, error) {
	e.mu.Lock()
	if status.Profile == e.cachedModelProfile && e.cachedModelID != "" {
		id := e.cachedModelID
		e.mu.Unlock()
		return id, nil
	}
	e.mu.Unlock()
	id, err := ResolveModelID(ctx, http.DefaultClient, strings.TrimSuffix(status.ProxyAddr, "/v1"))
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.cachedModelID = id
	e.cachedModelProfile = status.Profile
	e.mu.Unlock()
	return id, nil
}

func (e *Executor) snapshot() error {
	// Write the original pipeline file bytes verbatim. Resume relies on
	// these bytes to detect mid-run pipeline edits via a content hash
	// compare. Best-effort: if the caller didn't populate PipelineSource
	// (older callers, some tests), write an empty file rather than
	// failing the run — resume just won't be able to validate drift.
	snapPath := filepath.Join(e.RunDir, "pipeline.yaml.snapshot")
	// In resume mode the snapshot is the original-run artifact we already
	// validated; don't overwrite it with the current PipelineSource bytes
	// (which may legitimately differ when --resume-force was used).
	if e.ResumeDir == "" {
		if err := os.WriteFile(snapPath, e.PipelineSource, 0o644); err != nil {
			slog.Warn("snapshot write failed", "err", err)
		}
	}
	data, err := json.MarshalIndent(e.Inputs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.RunDir, "inputs.json"), data, 0o644)
}

// checkResumeSnapshot loads the prior run's pipeline.yaml.snapshot and
// compares its SHA-256 to the current PipelineSource. The hashes match iff
// the user is resuming the same pipeline file (byte-identical) — any whitespace
// or comment edit counts as drift, since the safer default is to err on
// "schema may have changed" rather than try to reason about semantic equality.
// Bypass with ResumeForce.
//
// Returns nil on hash match (or when the user opted out via ResumeForce).
// Returns an error mentioning "pipeline file changed" on mismatch so the CLI
// surface stays grep-able.
func (e *Executor) checkResumeSnapshot() error {
	info, err := os.Stat(e.RunDir)
	if err != nil {
		return fmt.Errorf("resume: stat run dir %s: %w", e.RunDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("resume: %s is not a directory", e.RunDir)
	}
	if e.ResumeForce {
		// Skip drift detection entirely. We do NOT require the snapshot
		// file to exist when force is set; that keeps the escape hatch
		// usable on legacy run dirs that pre-date the snapshot-population
		// fix in this PR.
		return nil
	}
	snapPath := filepath.Join(e.RunDir, "pipeline.yaml.snapshot")
	snap, err := os.ReadFile(snapPath)
	if err != nil {
		return fmt.Errorf("resume: read pipeline snapshot %s: %w", snapPath, err)
	}
	want := sha256Hex(snap)
	got := sha256Hex(e.PipelineSource)
	if want != got {
		return fmt.Errorf("resume: pipeline file changed since this run started (snapshot %s != current %s); either pass the original pipeline file or start a fresh run (omit --resume), or use --resume-force to override", want[:12], got[:12])
	}
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (e *Executor) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	fmt.Fprintf(e.Log, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

func validateJSON(s string) error {
	var v any
	return json.Unmarshal([]byte(s), &v)
}
