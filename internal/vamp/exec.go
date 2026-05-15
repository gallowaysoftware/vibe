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
	stageOutputs       map[string]string
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

	e.stageOutputs = make(map[string]string, len(e.Pipeline.Stages))
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
	// to Log, exactly as the sequential path used to.
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

// executeStage runs a single stage: renders the prompt, calls inference (with
// tokens streamed either to tokenSink, when non-nil, or directly to Log when
// nil), validates the output, and writes it to disk.
func (e *Executor) executeStage(ctx context.Context, st *Stage, baseURL, modelID string, tokenSink io.Writer) error {
	prompt, err := e.renderPrompt(st)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}
	e.logf("  -> stage %q: %d-char prompt → model %s", st.ID, len(prompt), modelID)

	start := time.Now()
	var onToken StreamFunc
	if tokenSink != nil {
		onToken = func(delta string) {
			_, _ = tokenSink.Write([]byte(delta))
		}
	} else if e.Log != nil {
		// Live-stream path: serialize writes against any logf calls so a
		// scheduler status line can't sneak between two tokens.
		onToken = func(delta string) {
			e.logMu.Lock()
			_, _ = e.Log.Write([]byte(delta))
			e.logMu.Unlock()
		}
	}
	out, err := e.Inference(ctx, baseURL+"/v1", modelID, prompt, st.Params, onToken)
	if err != nil {
		return fmt.Errorf("inference: %w", err)
	}
	if tokenSink == nil && e.Log != nil {
		// Match the previous sequential UX: ensure the next status line
		// starts on its own line after a live-streamed completion.
		e.logMu.Lock()
		_, _ = e.Log.Write([]byte("\n"))
		e.logMu.Unlock()
	}
	e.logf("  <- stage %q: %d-char response in %s", st.ID, len(out), time.Since(start).Round(time.Millisecond))

	if st.OutputFormat == "json" {
		if err := validateJSON(out); err != nil {
			return fmt.Errorf("stage output is not valid JSON: %w", err)
		}
	}

	outPath := filepath.Join(e.RunDir, st.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, []byte(out), 0o644); err != nil {
		return err
	}
	e.mu.Lock()
	e.stageOutputs[st.ID] = out
	e.mu.Unlock()
	return nil
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

func (e *Executor) renderPrompt(st *Stage) (string, error) {
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

	tmpl, err := template.New(st.ID).Option("missingkey=error").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	stages := make(map[string]map[string]string)
	e.mu.Lock()
	for _, dep := range st.Inputs {
		text, ok := e.stageOutputs[dep]
		if !ok {
			e.mu.Unlock()
			return "", fmt.Errorf("input %q has no output yet (scheduler bug or unresolved dependency)", dep)
		}
		stages[dep] = map[string]string{"output": text}
	}
	e.mu.Unlock()
	data := map[string]any{
		"inputs": e.Inputs,
		"stages": stages,
		"runDir": e.RunDir,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
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
