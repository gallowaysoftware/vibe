package vamp

import (
	"bytes"
	"context"
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

// stageResult holds the output(s) of a completed stage. For ordinary stages
// only Output is meaningful; for foreach stages Outputs holds the per-item
// content in input-array order and Output is a deterministic newline-joined
// concatenation so legacy `.stages.<id>.output` references keep working.
type stageResult struct {
	Output  string
	Outputs []string
}

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

	// in-memory state populated during Run
	mu                 sync.Mutex // guards stageOutputs and the model cache
	stageOutputs       map[string]*stageResult
	cachedModelID      string
	cachedModelProfile string
	// logMu serializes writes to Log from concurrent stages so buffered
	// stage outputs land contiguously even when status lines from the
	// scheduler are interleaved.
	logMu sync.Mutex
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
	if err := os.MkdirAll(e.RunDir, 0o755); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	if err := e.snapshot(); err != nil {
		return fmt.Errorf("snapshot run inputs: %w", err)
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
	modelID, err := e.modelIDForCurrent(ctx, status)
	if err != nil {
		return fmt.Errorf("resolve model id: %w", err)
	}
	baseURL := strings.TrimSuffix(status.ProxyAddr, "/v1")

	// Single-stage group: preserve the live-token UX by streaming directly
	// to Log, exactly as the sequential path used to. (A foreach stage with
	// more than one item still buffers per-item internally — see executeStage.)
	if len(group) == 1 {
		st := group[0]
		if err := e.executeStage(ctx, st, baseURL, modelID, nil); err != nil {
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
			err := e.executeStage(groupCtx, st, baseURL, modelID, buf)
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

// executeStage runs a single stage. For ordinary stages this renders the
// prompt, calls inference (with tokens streamed either to tokenSink, when
// non-nil, or directly to Log when nil), validates the output, and writes it
// to disk. For foreach stages it fans out one inference per item in the
// resolved JSON array, runs them in parallel against the same profile, and
// aggregates the per-item outputs.
func (e *Executor) executeStage(ctx context.Context, st *Stage, baseURL, modelID string, tokenSink io.Writer) error {
	if st.Foreach != "" {
		return e.executeForeachStage(ctx, st, baseURL, modelID, tokenSink)
	}
	prompt, err := e.renderPrompt(st, nil)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}
	e.logf("  -> stage %q: %d-char prompt → model %s", st.ID, len(prompt), modelID)

	out, err := e.runInference(ctx, st, baseURL, modelID, prompt, tokenSink)
	if err != nil {
		return err
	}
	if st.OutputFormat == "json" {
		if err := validateJSON(out); err != nil {
			return fmt.Errorf("stage output is not valid JSON: %w", err)
		}
	}

	outPath, err := e.renderOutputPath(st, nil)
	if err != nil {
		return fmt.Errorf("render output path: %w", err)
	}
	if err := writeFile(filepath.Join(e.RunDir, outPath), out); err != nil {
		return err
	}
	e.mu.Lock()
	e.stageOutputs[st.ID] = &stageResult{Output: out}
	e.mu.Unlock()
	return nil
}

// executeForeachStage implements the fan-out semantics for stages with a
// non-empty Foreach. The items list is parsed from the rendered foreach
// template, which must produce a JSON array. Each item runs in its own
// goroutine and writes its own output file.
func (e *Executor) executeForeachStage(ctx context.Context, st *Stage, baseURL, modelID string, tokenSink io.Writer) error {
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

	// Pre-render each item's prompt and output path so we can detect path
	// collisions and report errors before launching any inference work.
	prompts := make([]string, len(items))
	outPaths := make([]string, len(items))
	seenPaths := make(map[string]int, len(items))
	for i, item := range items {
		extra := map[string]any{st.ForeachAs: item}
		p, err := e.renderPrompt(st, extra)
		if err != nil {
			return fmt.Errorf("render prompt for item %d: %w", i, err)
		}
		path, err := e.renderOutputPath(st, extra)
		if err != nil {
			return fmt.Errorf("render output path for item %d: %w", i, err)
		}
		if prev, ok := seenPaths[path]; ok {
			return fmt.Errorf("stage %s: foreach output path collision: items %d and %d both produce %q", st.ID, prev, i, path)
		}
		seenPaths[path] = i
		prompts[i] = p
		outPaths[i] = path
	}

	// Streaming policy:
	//   - n == 1 AND tokenSink == nil (single-stage group): stream live.
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
	for i := range items {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var perItemSink io.Writer
			var buf *bytes.Buffer
			switch {
			case singleLive:
				perItemSink = nil // live stream to Log via runInference
			case tokenSink != nil:
				// We're already inside a parallel group whose log block is
				// owned by the caller. Append per-item tokens to that block
				// with our own [index] sub-headers so individual items are
				// distinguishable.
				buf = &bytes.Buffer{}
				perItemSink = buf
			default:
				// Single-stage group, multiple items: each item gets its own
				// buffer and flushes under its own header.
				buf = &bytes.Buffer{}
				perItemSink = buf
			}
			out, runErr := e.runInference(groupCtx, st, baseURL, modelID, prompts[i], perItemSink)
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
				cancel()
				return
			}
			if st.OutputFormat == "json" {
				if err := validateJSON(out); err != nil {
					errs[i] = fmt.Errorf("item %d: stage output is not valid JSON: %w", i, err)
					cancel()
					return
				}
			}
			if err := writeFile(filepath.Join(e.RunDir, outPaths[i]), out); err != nil {
				errs[i] = fmt.Errorf("item %d: write output: %w", i, err)
				cancel()
				return
			}
			outputs[i] = out
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

// runInference wraps a single inference call with the executor's token-sink
// plumbing. When tokenSink is nil and Log is set, tokens stream live to Log
// under logMu so they can't interleave with scheduler status lines.
func (e *Executor) runInference(ctx context.Context, st *Stage, baseURL, modelID, prompt string, tokenSink io.Writer) (string, error) {
	start := time.Now()
	var onToken StreamFunc
	if tokenSink != nil {
		onToken = func(delta string) {
			_, _ = tokenSink.Write([]byte(delta))
		}
	} else if e.Log != nil {
		onToken = func(delta string) {
			e.logMu.Lock()
			_, _ = e.Log.Write([]byte(delta))
			e.logMu.Unlock()
		}
	}
	out, err := e.Inference(ctx, baseURL+"/v1", modelID, prompt, st.Params, onToken)
	if err != nil {
		return "", fmt.Errorf("inference: %w", err)
	}
	if tokenSink == nil && e.Log != nil {
		// Ensure the next status line starts on its own line after a
		// live-streamed completion.
		e.logMu.Lock()
		_, _ = e.Log.Write([]byte("\n"))
		e.logMu.Unlock()
	}
	e.logf("  <- stage %q: %d-char response in %s", st.ID, len(out), time.Since(start).Round(time.Millisecond))
	return out, nil
}

// resolveForeachItems renders the Foreach template and parses the resulting
// string as a JSON array. Items may be strings, numbers, booleans, or
// objects; null is rejected as inadmissible.
func (e *Executor) resolveForeachItems(st *Stage) ([]any, error) {
	rendered, err := e.renderTemplate("foreach:"+st.ID, st.Foreach, st.Inputs, nil)
	if err != nil {
		return nil, fmt.Errorf("render foreach template: %w", err)
	}
	rendered = strings.TrimSpace(rendered)
	if rendered == "" {
		return nil, fmt.Errorf("foreach template rendered to empty string")
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

// renderPrompt renders the stage's prompt template. extra is merged on top of
// the standard template data (inputs/stages/runDir) so foreach can inject the
// current item under its ForeachAs name.
func (e *Executor) renderPrompt(st *Stage, extra map[string]any) (string, error) {
	raw := st.Prompt
	if raw == "" && st.PromptFile != "" {
		path := st.PromptFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.PipelineDir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read prompt_file %s: %w", path, err)
		}
		raw = string(data)
	}
	return e.renderTemplate(st.ID, raw, st.Inputs, extra)
}

// renderOutputPath renders the stage's Output template with the same data
// shape as renderPrompt. The returned path is always relative to RunDir.
func (e *Executor) renderOutputPath(st *Stage, extra map[string]any) (string, error) {
	return e.renderTemplate(st.ID+":output", st.Output, st.Inputs, extra)
}

// renderTemplate is the common rendering routine for prompt / output / foreach
// templates. It exposes inputs, stages (with both .output and .outputs), and
// runDir, plus any extra bindings supplied by the caller (foreach injects the
// per-item value under its ForeachAs name). Only stage ids listed in deps are
// exposed under .stages, matching the dependency declared in YAML.
func (e *Executor) renderTemplate(name, raw string, deps []string, extra map[string]any) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Funcs(templateFuncs()).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	stages := make(map[string]map[string]any, len(deps))
	e.mu.Lock()
	for _, dep := range deps {
		res, ok := e.stageOutputs[dep]
		if !ok {
			e.mu.Unlock()
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
	e.mu.Unlock()
	data := map[string]any{
		"inputs": e.Inputs,
		"stages": stages,
		"runDir": e.RunDir,
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
	if err := os.WriteFile(filepath.Join(e.RunDir, "pipeline.yaml.snapshot"), nil, 0o644); err != nil {
		// best-effort; don't fail the run on snapshot errors
		slog.Warn("snapshot placeholder failed", "err", err)
	}
	data, err := json.MarshalIndent(e.Inputs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.RunDir, "inputs.json"), data, 0o644)
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
