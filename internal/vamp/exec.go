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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	// StageTypeWebhook POSTs a template-rendered JSON body to a
	// Slack/Discord/Mattermost-compatible incoming webhook URL. Typically
	// the final stage of a pipeline to announce completion. Like the
	// network-only youtube type it does not activate a vibe profile.
	StageTypeWebhook StageType = "webhook"
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
	// PipelineName is the parent Pipeline.Name from YAML. Exposed to
	// executors so e.g. the webhook stage can template `{{ .pipeline_name }}`
	// into its body without us threading it through every per-stage call
	// site. Empty when the runner is invoked directly (some tests) without
	// a populated Pipeline.
	PipelineName string
	Log          io.Writer          // for token streaming when group size == 1
	Vibe         *vibeclient.Client // available to TextExecutor
	Item         any                // foreach item bound here (nil when not in a foreach)
	ItemIdx      int                // foreach index (0 when not in a foreach)

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

	// PipelineStatus is the run's current aggregated status ("ok" or
	// "error") at the moment the stage is dispatched. Surfaced to templates
	// as {{ .pipeline_status }} and intended for run_when: failure / always
	// stages that need to mention the outcome in their body.
	PipelineStatus string
	// FailureSummary is a short "<stage-id>: <error>" string describing the
	// FIRST stage error seen during the run, or "" when no stage has
	// errored. Surfaced to templates as {{ .failure_summary }}; only
	// meaningful for failure/always stages.
	FailureSummary string
}

// StageOutput is what an executor returns for ONE invocation (one item if
// foreach). The DAG runner aggregates per-item outputs into the existing
// stageResult{Output, Outputs} shape; executors don't need to know about that.
type StageOutput struct {
	Text  string   // for text stages
	Files []string // for binary outputs (Phase 2+); ABSOLUTE paths so subprocess argv strings resolve from the daemon's CWD
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

	// stageStatus tracks the live success/failure outcome of every stage as
	// the DAG progresses. Values are "ok", "error", or "skipped"; populated
	// the moment a stage's group finishes (or it's decided not to run).
	// run_when scheduling consults this map to decide whether each newly
	// ready stage should fire. Guarded by mu.
	stageStatus map[string]string
	// stageErrors records the first error seen for each stage that produced
	// one (matched 1:1 with stageStatus["error"] entries). Drives the
	// {{ .failure_summary }} template binding for run_when failure/always
	// stages. Guarded by mu.
	stageErrors map[string]error
	// failureOrder preserves the order in which stages errored so the
	// failure_summary picks the FIRST failure deterministically, even when
	// multiple stages fail in parallel within the same wave. Guarded by mu.
	failureOrder []string
	// failedSoFar mirrors len(failureOrder) > 0; centralised flag so the
	// run_when scheduler can short-circuit without taking the lock just to
	// check. Guarded by mu.
	failedSoFar bool
	// logMu serializes writes to Log from concurrent stages so buffered
	// stage outputs land contiguously even when status lines from the
	// scheduler are interleaved.
	logMu sync.Mutex

	// registry maps StageType -> executor. Built once in Run and shared by
	// every stage; per-stage state must travel via StageInput.
	registry map[StageType]StageExecutor

	// stageTimings is populated by executeStage as stages finish; powers
	// pipeline.json (used by `vamp runs ls`). recordMu guards it.
	recordMu     sync.Mutex
	stageTimings map[string]StageRecord

	// timing accumulates per-stage / per-item wall-clock durations and
	// becomes the run-end summary table plus pipeline_timing.json. Built
	// once at Run start; nil-safe so older code paths (and tests that
	// invoke executor internals directly without Run) don't have to know
	// about it.
	timing *Tracker
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
func (e *Executor) Run(ctx context.Context) (runErr error) {
	if e.Inference == nil {
		e.Inference = defaultInference()
	}
	// Capture wall-clock start before any I/O so pipeline.json reflects
	// "when the user kicked off the run".
	runStart := time.Now()
	e.stageTimings = make(map[string]StageRecord)
	// Initialize the timing tracker. Nil-safe so internal-API tests don't
	// need to set it up themselves.
	pipelineName := ""
	if e.Pipeline != nil {
		pipelineName = e.Pipeline.Name
	}
	e.timing = NewTracker(pipelineName)
	// Defer summary + JSON writes on every exit path. Both writes are
	// best-effort; failures log via slog and never change Run's exit code.
	defer func() {
		e.writePipelineJSON(runStart, time.Now(), runErr)
		e.timing.Finish()
		if e.RunDir != "" {
			if err := e.timing.WriteJSON(e.RunDir); err != nil {
				slog.Warn("timing report: write json failed", "err", err)
			}
		}
		if e.Log != nil {
			e.logMu.Lock()
			defer e.logMu.Unlock()
			_ = e.timing.FormatTable(e.Log)
			if e.RunDir != "" {
				fmt.Fprintf(e.Log, "outputs: %s\n", e.RunDir)
			}
		}
	}()
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
		StageTypeWebhook: &webhookExecutor{},
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
	e.stageStatus = make(map[string]string, len(e.Pipeline.Stages))
	e.stageErrors = make(map[string]error)
	remaining := len(e.Pipeline.Stages)
	wave := 0
	for remaining > 0 {
		// Collect every stage whose deps are satisfied. A dep is "satisfied"
		// as soon as it has reached a terminal status (ok / error / skipped);
		// run_when scheduling decides per-stage whether to actually fire,
		// skip, or short-circuit based on the deps' outcomes.
		// Sort by id so the scheduling order is deterministic across runs.
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

		// Decide per-stage whether to run or skip based on RunWhen and the
		// deps' current statuses. Skipped stages do not run an executor and
		// do not propagate output to dependents, but they DO release any
		// further dependents (whose own RunWhen will see "skipped" deps,
		// equivalent to a pipeline failure for the purposes of failure/always
		// gating).
		var toRun []*Stage
		var toSkip []*Stage
		for _, st := range ready {
			if e.shouldRunStage(st) {
				toRun = append(toRun, st)
			} else {
				toSkip = append(toSkip, st)
			}
		}
		// Mark skipped stages first so their status is visible before any
		// of the parallel runners in this wave check dep statuses for
		// downstream scheduling. (Within a wave dep checks already happened
		// above; this is a belt-and-braces ordering.)
		for _, st := range toSkip {
			e.markStageSkipped(st.ID)
			e.logf("  -> stage %q: skipped (run_when=%s, pipeline status=%s)", st.ID, runWhenOrDefault(st), e.pipelineStatusString())
			for _, child := range dependents[st.ID] {
				if _, ok := indeg[child]; ok {
					indeg[child]--
				}
			}
			remaining--
		}

		// Group runnable stages by capability, preserving alphabetical
		// capability order for determinism.
		groups := make(map[string][]*Stage)
		var capOrder []string
		for _, st := range toRun {
			if _, ok := groups[st.Capability]; !ok {
				capOrder = append(capOrder, st.Capability)
			}
			groups[st.Capability] = append(groups[st.Capability], st)
		}
		sort.Strings(capOrder)

		for _, capName := range capOrder {
			group := groups[capName]
			e.logf("wave %d: capability %q running %d stage(s)", wave, capName, len(group))
			// runGroup returns an aggregated error when one or more stages
			// in the group failed OR when group setup itself failed
			// (capability not mapped, profile activation refused, etc.).
			// Per-stage failures are already recorded via markStageStatus
			// inside executeStage's defer; setup-time failures predate any
			// stage's executor call, so we attribute them to every stage
			// in the group here so downstream run_when=failure stages can
			// still observe the failure.
			groupErr := e.runGroup(ctx, capName, group)
			if groupErr != nil {
				e.logf("wave %d: capability %q produced error(s): %v", wave, capName, groupErr)
				for _, st := range group {
					e.markStageStatus(st.ID, groupErr)
				}
			}
			// Decrement indegree for everything that depended on stages in
			// this group regardless of their outcomes; the dependents'
			// scheduling decision will consult stageStatus when they're
			// considered.
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
	// If any stage errored over the course of the run, surface a single
	// aggregated error so callers (including tests) still observe the
	// failure. The aggregated form joins every per-stage error in the
	// order they were recorded; the first error remains accessible via
	// errors.Unwrap chains for callers that inspect them.
	if e.failedSoFar {
		errs := make([]error, 0, len(e.failureOrder))
		e.mu.Lock()
		for _, id := range e.failureOrder {
			if err, ok := e.stageErrors[id]; ok {
				errs = append(errs, fmt.Errorf("stage %s: %w", id, err))
			}
		}
		e.mu.Unlock()
		return errors.Join(errs...)
	}
	// The deferred timing summary runs after this return and prints the
	// detailed per-stage table + "outputs:" line, so we keep this top-line
	// log for backward compatibility with anything scanning Log for the
	// "finished" sentinel (e.g. tests).
	return nil
}

// shouldRunStage decides whether a freshly-ready stage should fire based
// on its RunWhen qualifier and the current statuses of its declared deps
// (or, for empty-Inputs failure stages, the overall pipeline status).
//
// Semantics (matching the documented schema):
//   - "success": run iff every dep reached status "ok". Skip if any dep
//     errored or was skipped, OR if the dep is missing a status entry
//     (shouldn't happen — wave gating ensures deps have run before this
//     check — but err on "skip" rather than fire on a half-tracked dep).
//   - "failure": run iff at least one dep errored OR was skipped (a
//     skipped dep means an upstream failure cascaded past it). When the
//     stage has no Inputs, fall back to the pipeline-wide failedSoFar
//     flag so users can place a notify_on_failure stage at the top
//     level of the DAG.
//   - "always": run unconditionally — the cleanup/finally case.
func (e *Executor) shouldRunStage(st *Stage) bool {
	mode := runWhenOrDefault(st)
	e.mu.Lock()
	defer e.mu.Unlock()
	switch mode {
	case RunWhenAlways:
		return true
	case RunWhenFailure:
		if len(st.Inputs) == 0 {
			return e.failedSoFar
		}
		for _, dep := range st.Inputs {
			s := e.stageStatus[dep]
			if s == "error" || s == "skipped" {
				return true
			}
		}
		return false
	default: // RunWhenSuccess
		for _, dep := range st.Inputs {
			if e.stageStatus[dep] != "ok" {
				return false
			}
		}
		return true
	}
}

// runWhenOrDefault returns Stage.RunWhen with the empty-default rule
// applied. Centralised so the executor and any future callers agree on
// the canonical value when the YAML omits the field.
func runWhenOrDefault(st *Stage) string {
	if st.RunWhen == "" {
		return RunWhenSuccess
	}
	return st.RunWhen
}

// markStageStatus records the terminal outcome of a stage. err==nil ->
// status "ok"; otherwise the error is recorded under stageErrors and the
// stage joins failureOrder so failure_summary can pick a deterministic
// "first failure" later. Idempotent: only the first call wins so a retry
// loop or a sibling cancel can't overwrite a real error with a follow-up
// context.Canceled.
func (e *Executor) markStageStatus(stageID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Lazy init so tests that bypass Run() (and call runGroup or
	// executeStage directly) don't panic on nil-map writes. Production
	// callers go through Run() which initialises the maps up front.
	if e.stageStatus == nil {
		e.stageStatus = make(map[string]string)
	}
	if e.stageErrors == nil {
		e.stageErrors = make(map[string]error)
	}
	if _, already := e.stageStatus[stageID]; already {
		return
	}
	if err == nil {
		e.stageStatus[stageID] = "ok"
		return
	}
	e.stageStatus[stageID] = "error"
	e.stageErrors[stageID] = err
	e.failureOrder = append(e.failureOrder, stageID)
	e.failedSoFar = true
}

// markStageSkipped is the run_when="success" sibling of markStageStatus
// for stages that never invoked an executor because their deps failed
// (or the pipeline status didn't match the stage's run_when qualifier).
// Skipped stages produce no output and do NOT contribute to the
// aggregated run error, but they still get a timing-record entry so
// pipeline.json reflects the full DAG.
func (e *Executor) markStageSkipped(stageID string) {
	e.mu.Lock()
	if e.stageStatus == nil {
		e.stageStatus = make(map[string]string)
	}
	if _, already := e.stageStatus[stageID]; !already {
		e.stageStatus[stageID] = "skipped"
	}
	e.mu.Unlock()
	e.recordSkippedStage(stageID)
	e.timing.StageStart(stageID, "")
	e.timing.StageEnd(stageID, "skipped", nil)
}

// pipelineStatusString returns the current pipeline-wide status for
// templating and logging. Mirrors the {{ .pipeline_status }} binding
// exposed to failure/always stages: "ok" while no stage has errored,
// "error" once one has. Centralised so the wave loop log and the
// template renderer agree on the string.
func (e *Executor) pipelineStatusString() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failedSoFar {
		return "error"
	}
	return "ok"
}

// failureSummary composes the {{ .failure_summary }} template binding.
// When multiple stages have failed (possibly in parallel within a wave),
// we pick the FIRST recorded failure (failureOrder[0]) — markStageStatus
// guarantees deterministic ordering by the order it was first called per
// stage. The string is "<stage-id>: <error>" so users can wire it
// directly into a Slack/Discord webhook body without extra formatting.
// Returns "" when no stage has errored (kept so always-mode stages can
// distinguish success runs).
func (e *Executor) failureSummary() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.failureOrder) == 0 {
		return ""
	}
	first := e.failureOrder[0]
	if err, ok := e.stageErrors[first]; ok {
		return fmt.Sprintf("%s: %s", first, err.Error())
	}
	return first
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
				// Resumed stages still get an entry in pipeline.json so
				// `vamp runs show` can see they participated. Duration
				// is zero (we don't know what the original run took) and
				// the status string differentiates them from fresh runs.
				e.recordSkippedStage(st.ID)
				// For run_when scheduling treat a resumed stage as having
				// succeeded — it produced its output in the original run,
				// so downstream "success" stages should still consider it
				// satisfied.
				e.markStageStatus(st.ID, nil)
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

	candidates, err := e.Capabilities.Profiles(capability)
	if err != nil {
		return err
	}
	// Walk the candidate list in declared order (biggest first). Stop on
	// the first one that activates; skip on a VRAM rejection (the daemon's
	// FailedPrecondition pre-flight error) so we can fall back to a
	// smaller profile; abort on any other error so genuine failures aren't
	// silently re-tried against a different profile.
	var (
		status  *vibev1.Status
		lastErr error
		picked  bool
	)
	for _, cand := range candidates {
		if len(candidates) > 1 {
			e.logf("  -> activating profile %q (candidate for %q)", cand, capability)
		} else {
			e.logf("  -> activating profile %q", cand)
		}
		st, actErr := e.Vibe.EnsureActive(ctx, cand)
		if actErr != nil {
			if vibeclient.IsVRAMRejection(actErr) {
				e.logf("  -> skipping %q: %s", cand, actErr.Error())
				lastErr = fmt.Errorf("activate %q: %w", cand, actErr)
				continue
			}
			return fmt.Errorf("activate %q: %w", cand, actErr)
		}
		if !st.Ready {
			// Treat a not-ready response from EnsureActive the same as
			// a generic activation failure: don't try the next
			// candidate, since "not ready" is usually a daemon
			// integration bug rather than a VRAM problem.
			return fmt.Errorf("profile %q is not ready", cand)
		}
		status = st
		picked = true
		break
	}
	if !picked {
		// All candidates VRAM-rejected. Return the last error so the
		// caller still sees a useful "needs N GiB" message rather than a
		// vague "no candidates fit".
		return lastErr
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
func (e *Executor) executeStage(ctx context.Context, st *Stage, baseURL, modelID, backendAddr string, tokenSink io.Writer) (stageErr error) {
	// Capture per-stage wall-clock duration for pipeline.json. The defer
	// records the StageRecord regardless of which return path we take so
	// failure stages still show up in `vamp runs show` with status=error
	// and a real timestamp delta. Foreach stages delegate to
	// executeForeachStage below, which records its own timing; we skip
	// recording here in that case to avoid double-counting.
	stageStart := time.Now()
	recordTiming := true
	defer func() {
		if recordTiming {
			e.recordStageFinish(st.ID, stageStart, stageErr)
		}
		// Mark the live run_when status regardless of which return path we
		// took. Foreach stages set their own timing record (above) but we
		// still want the per-stage status published here so downstream
		// run_when scheduling sees the outcome promptly.
		e.markStageStatus(st.ID, stageErr)
	}()
	exec, err := e.executorFor(st)
	if err != nil {
		return err
	}
	// Track wall time for this stage. Foreach stages handle their own
	// StageStart/End inside executeForeachStage so the parent's duration
	// always covers the full fan-out (and matches the wall time the user
	// observes); per-item rows are recorded by ItemStart/End below.
	stageType := string(stageTypeOrDefault(st))
	if st.Foreach != nil {
		recordTiming = false
		e.timing.StageStart(st.ID, stageType)
		err := e.executeForeachStage(ctx, st, exec, baseURL, modelID, backendAddr, tokenSink)
		status := "ok"
		if err != nil {
			status = "error"
		}
		e.timing.StageEnd(st.ID, status, e.stageNotes(st))
		return err
	}

	e.timing.StageStart(st.ID, stageType)
	in := e.makeStageInput(st, baseURL, modelID, backendAddr, tokenSink, nil, 0)
	out, err := e.runWithRetry(ctx, st, exec, in)
	if err != nil {
		e.timing.StageEnd(st.ID, "error", e.stageNotes(st))
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
		notes := e.stageNotes(st)
		notes["files"] = len(out.Files)
		e.timing.StageEnd(st.ID, "ok", notes)
		return nil
	}

	outPath, err := e.renderOutputPath(st, nil)
	if err != nil {
		e.timing.StageEnd(st.ID, "error", e.stageNotes(st))
		return fmt.Errorf("render output path: %w", err)
	}
	if err := writeFile(filepath.Join(e.RunDir, outPath), out.Text); err != nil {
		e.timing.StageEnd(st.ID, "error", e.stageNotes(st))
		return err
	}
	e.mu.Lock()
	e.stageOutputs[st.ID] = &stageResult{Output: out.Text}
	e.mu.Unlock()
	notes := e.stageNotes(st)
	notes["chars_out"] = len(out.Text)
	e.timing.StageEnd(st.ID, "ok", notes)
	return nil
}

// executeForeachStage implements the fan-out semantics for stages with a
// non-nil Foreach. The items list is read from the upstream stage's JSON
// output (referenced by Foreach.From). Each item runs in its own goroutine
// through the registered executor and writes its own output file.
func (e *Executor) executeForeachStage(ctx context.Context, st *Stage, exec StageExecutor, baseURL, modelID, backendAddr string, tokenSink io.Writer) (stageErr error) {
	// Mirror executeStage's per-stage timing: the foreach stage's
	// "duration" is the elapsed wall-clock for the whole fan-out
	// (resolve + render + run-all-items), which is the user-visible cost.
	stageStart := time.Now()
	defer func() {
		e.recordStageFinish(st.ID, stageStart, stageErr)
	}()
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

	// Runtime check (moved here from Validate so single-item foreach with a
	// non-templated Output is accepted): if the upstream resolves to more
	// than one item and the Output template is static, every item writes to
	// the same file. The validator used to flag this statically; the
	// dynamic form re-uses ForeachNonTemplatedMultiItemErrMsg so the
	// message text users see is unchanged.
	if len(items) > 1 && !strings.Contains(st.Output, "{{") {
		return fmt.Errorf(ForeachNonTemplatedMultiItemErrMsg, "stage "+st.ID)
	}

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
			e.timing.ItemStart(st.ID, i)
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
			// Retry policy applies PER ITEM: a single transient failure on
			// item N does not re-run items 0..N-1. The wrapper respects
			// groupCtx so an erroring sibling that calls cancel() aborts
			// any in-progress backoff sleeps immediately.
			out, runErr := e.runWithRetry(groupCtx, st, exec, in)
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
				e.timing.ItemEnd(st.ID, i, "error", nil)
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
			itemNotes := map[string]any{}
			if len(out.Files) > 0 {
				outputs[i] = strings.Join(out.Files, "\n")
				itemNotes["files"] = len(out.Files)
			} else {
				if err := writeFile(filepath.Join(e.RunDir, outPaths[i]), out.Text); err != nil {
					errs[i] = fmt.Errorf("item %d: write output: %w", i, err)
					e.timing.ItemEnd(st.ID, i, "error", nil)
					cancel()
					return
				}
				outputs[i] = out.Text
				itemNotes["chars_out"] = len(out.Text)
			}
			e.timing.ItemEnd(st.ID, i, "ok", itemNotes)
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

// runWithRetry invokes exec.Execute under the stage's RetryPolicy. When the
// policy is nil or has MaxAttempts <= 1 the call is forwarded verbatim and
// the executor's first error is returned (matching pre-retry behaviour).
//
// On a retryable error we sleep for the current backoff (honoring ctx) and
// loop; the backoff grows by policy.Multiplier, capped at policy.MaxBackoff,
// for each attempt. Non-retryable errors (validation failures, user cancel,
// errors whose classification does not match a configured retry_on mode) are
// returned immediately.
//
// The "current attempt" counter is bumped AFTER each Execute call so the
// final-attempt log message and the early-return path both see the correct
// value. itemTag, when non-empty, is appended to log lines so the foreach
// per-item retry loop produces a unique header.
func (e *Executor) runWithRetry(ctx context.Context, st *Stage, exec StageExecutor, in StageInput) (*StageOutput, error) {
	policy := st.Retry
	// Fast path: no policy (nil) or single-attempt policy means "no retry".
	// Skip the loop entirely so the existing call graph is unchanged for
	// pipelines that don't opt in.
	if policy == nil || policy.MaxAttempts <= 1 {
		return exec.Execute(ctx, in)
	}
	attempt := 0
	backoff := policy.InitialBackoff
	itemTag := ""
	if st.Foreach != nil {
		itemTag = fmt.Sprintf("[%d]", in.ItemIdx)
	}
	for {
		out, err := exec.Execute(ctx, in)
		if err == nil {
			if attempt > 0 {
				e.logf("  -> stage %q%s recovered after %d retry(ies)", st.ID, itemTag, attempt)
			}
			return out, nil
		}
		attempt++
		// Always honor explicit ctx cancellation before deciding to retry.
		// A user-initiated ctrl-C must never trigger another attempt even
		// if the underlying error otherwise looks transient (e.g. an
		// HTTP request aborted with "context canceled" reads as a network
		// error to some libraries).
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		if attempt >= policy.MaxAttempts {
			return nil, err
		}
		if !isRetryable(err, policy) {
			return nil, err
		}
		e.logf("  -> stage %q%s attempt %d failed; retrying in %s: %v", st.ID, itemTag, attempt, backoff, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Grow backoff with the configured multiplier, capped at MaxBackoff.
		// time.Duration is int64 nanoseconds; multiplying by a float is
		// fine here because MaxBackoff is on the order of seconds.
		next := time.Duration(float64(backoff) * policy.Multiplier)
		if next > policy.MaxBackoff || next <= 0 {
			next = policy.MaxBackoff
		}
		backoff = next
	}
}

// isRetryable reports whether err matches at least one of the retry modes in
// policy.RetryOn. The classification is intentionally permissive because the
// stage executors return free-form errors (string-wrapped HTTP statuses, raw
// net errors, etc.) — in Phase 1 we don't want to require every executor to
// surface a typed error.
//
// Classification:
//   - context.Canceled: never retryable (user cancel; caller handles this
//     explicitly above for early-out, but we also gate here defensively).
//   - context.DeadlineExceeded or *net.OpError with Timeout(): retryable
//     under both "timeout" and "transient" modes.
//   - HTTP 5xx errors (matched by substring in the error string — see
//     transientHTTPMatchers below) and common network-layer failures
//     ("connection refused", "connection reset", "i/o timeout", "EOF",
//     "no such host", "network is unreachable") are retryable under
//     "transient" only.
//
// The HTTP 5xx substring match exists because no executor wraps its errors
// in a typed HTTPError; we'd have to thread one through ChatCompletion,
// the ComfyUI executor, and any future Phase 2 backends. For Phase 1 we
// pattern-match the standard "<code> <reason>" form (e.g. "503 Service
// Unavailable") that net/http's resp.Status and most clients embed in
// error strings. If a future executor wraps differently, this list is the
// single place to extend.
func isRetryable(err error, policy *RetryPolicy) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	hasTimeoutMode := false
	hasTransientMode := false
	for _, mode := range policy.RetryOn {
		switch mode {
		case retryOnTimeout:
			hasTimeoutMode = true
		case retryOnTransient:
			hasTransientMode = true
		}
	}
	// Timeout detection: works for both "timeout" and "transient" modes
	// because every timeout is also a transient by definition.
	isTimeout := errors.Is(err, context.DeadlineExceeded)
	if !isTimeout {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			isTimeout = true
		}
	}
	if isTimeout && (hasTimeoutMode || hasTransientMode) {
		return true
	}
	if !hasTransientMode {
		// Only "timeout" was requested and the error isn't a timeout.
		return false
	}
	// Transient-mode classification by substring + 5xx-status regex.
	// Lower-case the haystack once so each substring matcher can be
	// ASCII-lowercase; the 5xx regex is digit-only so case doesn't matter.
	msg := strings.ToLower(err.Error())
	if http5xxRE.MatchString(msg) {
		return true
	}
	for _, m := range transientErrorMatchers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// transientErrorMatchers is the substring set used to classify "transient"
// errors when no typed error is available. Each entry is matched against the
// lower-cased err.Error(); ordering doesn't affect correctness but
// frequently-hit cases come first for cheap short-circuit. Centralized so
// future executors can extend transient-detection in one place.
//
// Entries:
//   - "i/o timeout", "connection refused", "connection reset": Go's
//     standard network-layer error strings.
//   - "no such host", "network is unreachable": DNS / routing failures
//     that are usually transient on flaky networks (and on CI).
//   - "unexpected eof": premature server hang-ups during keepalive reuse
//     or mid-stream SSE drops.
//
// HTTP 5xx codes are NOT in this list; they're matched separately by
// http5xxRE so we can anchor against word boundaries (digit groups
// preceded/followed by non-digits) and avoid matching "5039" in a
// numeric stage ID or a user prompt that happens to contain "503".
var transientErrorMatchers = []string{
	"i/o timeout",
	"connection refused",
	"connection reset",
	"no such host",
	"network is unreachable",
	"unexpected eof",
}

// http5xxRE matches an HTTP 5xx status code embedded in an error string.
// The non-digit anchors on both sides prevent the regex from matching
// numeric substrings like "5039" or "1503ms" that are not status codes.
// Compiled once at init so the per-error check is O(len(msg)).
var http5xxRE = regexp.MustCompile(`(?:^|\D)5[0-9]{2}(?:\D|$)`)

// makeStageInput assembles a StageInput for one invocation. tokenSink is the
// caller-provided buffer for token streaming, or nil to stream directly to
// e.Log under logMu. item / itemIdx are bound for foreach invocations.
func (e *Executor) makeStageInput(st *Stage, baseURL, modelID, backendAddr string, tokenSink io.Writer, item any, itemIdx int) StageInput {
	var name string
	if e.Pipeline != nil {
		name = e.Pipeline.Name
	}
	return StageInput{
		Stage:          st,
		Inputs:         e.Inputs,
		Prior:          e.snapshotPrior(st.Inputs),
		RunDir:         e.RunDir,
		PipelineDir:    e.PipelineDir,
		PipelineName:   name,
		Log:            e.tokenLog(tokenSink),
		Vibe:           e.Vibe,
		Item:           item,
		ItemIdx:        itemIdx,
		BaseURL:        baseURL,
		ModelID:        modelID,
		BackendAddr:    backendAddr,
		PipelineStatus: e.pipelineStatusString(),
		FailureSummary: e.failureSummary(),
	}
}

// stageNotes assembles the per-stage notes map surfaced in the timing
// report (the "notes" column in the table and the notes object in JSON).
// Stage-type-specific facts go here: text stages get max_tokens from
// params if set; comfyui/audio stages declare their workflow / voice
// inputs. Returns a non-nil map so callers can mutate it freely without
// nil-checks.
func (e *Executor) stageNotes(st *Stage) map[string]any {
	n := map[string]any{}
	if st == nil {
		return n
	}
	t := stageTypeOrDefault(st)
	if t == StageTypeText {
		if mt, ok := st.Params["max_tokens"]; ok {
			n["max_tokens"] = mt
		}
	}
	return n
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
	case StageTypeAudio, StageTypeFFmpeg, StageTypeYouTube, StageTypeWebhook:
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
//
// run_when failure/always stages may reference deps whose executors never
// produced an output (because they errored or were skipped); we synthesise
// an empty stageResult for those so `.stages.<id>.output` resolves to "" in
// the template instead of crashing renderTemplate. Successful resume seeding
// always lands a real entry in stageOutputs, so the synthesised-empty path
// only fires for known-failed/skipped deps.
func (e *Executor) snapshotPrior(deps []string) map[string]*stageResult {
	out := make(map[string]*stageResult, len(deps))
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, dep := range deps {
		if res, ok := e.stageOutputs[dep]; ok {
			out[dep] = res
			continue
		}
		// No output recorded. If we've already marked the dep as
		// terminated (error or skipped) make the empty result visible to
		// the template so failure/always stages can reference the dep
		// without erroring. We leave deps that haven't been processed at
		// all out — Validate + the wave-gating prevent that case in
		// practice, and a real "scheduler bug" still surfaces as a clear
		// renderTemplate error rather than silently rendering "".
		switch e.stageStatus[dep] {
		case "error", "skipped":
			out[dep] = &stageResult{}
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
	extra := map[string]any{
		"pipeline_status": in.PipelineStatus,
		"failure_summary": in.FailureSummary,
	}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
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
	// Binary stages (comfyui/audio/ffmpeg) record an ABSOLUTE path in
	// stageResult.Output so downstream subprocesses (ffmpeg/Piper running
	// from the daemon's CWD) can open it without re-prefixing RunDir. This
	// matches the live-run path (executors return absolute paths in
	// out.Files). YouTube is the exception: its "output" is the watch URL we
	// wrote to disk, and downstream stages expect the URL string itself —
	// same shape as text.
	t := stageTypeOrDefault(st)
	if t != StageTypeText && t != StageTypeYouTube {
		e.mu.Lock()
		e.stageOutputs[st.ID] = &stageResult{Output: full}
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
			// Binary foreach items (comfyui) expose an ABSOLUTE path so
			// downstream {{ .stages.X.outputs[i] }} references work as
			// subprocess argv strings (the daemon's CWD is not RunDir).
			outputs[i] = full
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

// recordStageFinish writes one StageRecord to the per-run timing map.
// "ok" maps to nil error; anything else (including context.Canceled
// from a sibling cancel) records "error". The record overwrites any
// existing entry under the same id so a retried stage's last attempt is
// what shows up in pipeline.json — matching what the user would see in
// the live log.
func (e *Executor) recordStageFinish(stageID string, start time.Time, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	rec := StageRecord{
		ID:         stageID,
		DurationMS: time.Since(start).Milliseconds(),
		Status:     status,
	}
	e.recordMu.Lock()
	if e.stageTimings == nil {
		e.stageTimings = make(map[string]StageRecord)
	}
	e.stageTimings[stageID] = rec
	e.recordMu.Unlock()
}

// recordSkippedStage records a "skipped" entry for a stage whose output
// the resume pre-pass loaded from disk. Duration is intentionally zero:
// we don't know what the original run took, and we don't want to mislead
// `runs ls` into thinking a 0ms duration is the real cost.
func (e *Executor) recordSkippedStage(stageID string) {
	rec := StageRecord{ID: stageID, DurationMS: 0, Status: "skipped"}
	e.recordMu.Lock()
	if e.stageTimings == nil {
		e.stageTimings = make(map[string]StageRecord)
	}
	e.stageTimings[stageID] = rec
	e.recordMu.Unlock()
}

// writePipelineJSON emits the run's pipeline.json record. The function is
// best-effort: it logs and swallows every error. We intentionally do not
// return an error because Run already deferred this call and a write
// failure must not change the run's exit code (a successful pipeline
// whose metadata write failed is still a successful pipeline).
//
// Stages are listed in declaration order so the file reads naturally
// alongside the YAML; stages that didn't run (because an earlier stage
// failed) are still listed but without a status entry.
func (e *Executor) writePipelineJSON(start, end time.Time, runErr error) {
	// No run dir → nothing to write. This happens when MkdirAll itself
	// failed; the user already saw that error and we have nowhere to
	// put the file.
	if e.RunDir == "" {
		return
	}
	if _, err := os.Stat(e.RunDir); err != nil {
		return
	}
	status := "ok"
	if runErr != nil {
		status = "error"
	} else {
		// "partial" reflects a clean run where one or more stages were
		// marked error or skipped via resume. Right now `error` already
		// short-circuits Run() (we wouldn't reach here with a stage
		// error and a nil runErr) so this only fires for resume runs
		// where some stages were rehydrated from disk.
		e.recordMu.Lock()
		anySkipped := false
		for _, rec := range e.stageTimings {
			if rec.Status == "skipped" {
				anySkipped = true
				break
			}
		}
		e.recordMu.Unlock()
		if anySkipped {
			status = "partial"
		}
	}
	name := ""
	if e.Pipeline != nil {
		name = e.Pipeline.Name
	}
	rec := RunRecord{
		Name:      name,
		StartTime: start,
		EndTime:   end,
		Status:    status,
	}
	// Walk the pipeline definition in declaration order so the JSON
	// stage list mirrors the YAML. Stages with no recorded timing
	// (i.e. they never got dispatched because an earlier sibling
	// failed) still appear with status "" so the count matches
	// `len(pipeline.Stages)` and downstream tools don't have to
	// special-case missing rows.
	e.recordMu.Lock()
	if e.Pipeline != nil {
		for _, st := range e.Pipeline.Stages {
			if got, ok := e.stageTimings[st.ID]; ok {
				rec.Stages = append(rec.Stages, got)
			} else {
				rec.Stages = append(rec.Stages, StageRecord{ID: st.ID})
			}
		}
	} else {
		// Defensive: some tests construct an Executor without a
		// Pipeline and only the stageTimings map; emit those in
		// alphabetical order so the output stays deterministic.
		ids := make([]string, 0, len(e.stageTimings))
		for id := range e.stageTimings {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rec.Stages = append(rec.Stages, e.stageTimings[id])
		}
	}
	e.recordMu.Unlock()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		slog.Warn("pipeline.json marshal failed", "err", err)
		return
	}
	path := filepath.Join(e.RunDir, "pipeline.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("pipeline.json write failed", "path", path, "err", err)
	}
}
