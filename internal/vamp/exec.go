package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	stageOutputs       map[string]string
	cachedModelID      string
	cachedModelProfile string
}

func defaultInference() InferenceFunc {
	cc := &ChatCompletion{}
	return cc.Call
}

// Run executes every stage sequentially. Earlier stage outputs are exposed to
// later stages via .stages.<id>.output in their templates.
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

	e.stageOutputs = make(map[string]string, len(e.Pipeline.Stages))
	for i, st := range e.Pipeline.Stages {
		e.logf("[%d/%d] stage %q (capability %q)", i+1, len(e.Pipeline.Stages), st.ID, st.Capability)
		if err := e.executeStage(ctx, &st); err != nil {
			return fmt.Errorf("stage %s: %w", st.ID, err)
		}
		// Read the output for downstream stages.
		data, err := os.ReadFile(filepath.Join(e.RunDir, st.Output))
		if err != nil {
			return fmt.Errorf("read output %s: %w", st.Output, err)
		}
		e.stageOutputs[st.ID] = string(data)
	}
	e.logf("pipeline %q finished, outputs in %s", e.Pipeline.Name, e.RunDir)
	return nil
}

func (e *Executor) executeStage(ctx context.Context, st *Stage) error {
	profile, err := e.Capabilities.Profile(st.Capability)
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

	prompt, err := e.renderPrompt(st)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	modelID, err := e.modelIDForCurrent(ctx, status)
	if err != nil {
		return fmt.Errorf("resolve model id: %w", err)
	}
	e.logf("  -> %d-char prompt → model %s", len(prompt), modelID)

	baseURL := strings.TrimSuffix(status.ProxyAddr, "/v1")
	start := time.Now()
	var onToken StreamFunc
	if e.Log != nil {
		onToken = func(delta string) {
			_, _ = e.Log.Write([]byte(delta))
		}
	}
	out, err := e.Inference(ctx, baseURL+"/v1", modelID, prompt, st.Params, onToken)
	if err != nil {
		return fmt.Errorf("inference: %w", err)
	}
	if e.Log != nil {
		// Ensure the next status line starts on a fresh line.
		_, _ = e.Log.Write([]byte("\n"))
	}
	e.logf("  <- %d-char response in %s", len(out), time.Since(start).Round(time.Millisecond))

	if st.OutputFormat == "json" {
		if err := validateJSON(out); err != nil {
			return fmt.Errorf("stage output is not valid JSON: %w", err)
		}
	}

	outPath := filepath.Join(e.RunDir, st.Output)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(out), 0o644)
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
	for _, dep := range st.Inputs {
		text, ok := e.stageOutputs[dep]
		if !ok {
			return "", fmt.Errorf("input %q has no output yet (must be a prior stage)", dep)
		}
		stages[dep] = map[string]string{"output": text}
	}
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
	if status.Profile == e.cachedModelProfile && e.cachedModelID != "" {
		return e.cachedModelID, nil
	}
	id, err := ResolveModelID(ctx, http.DefaultClient, strings.TrimSuffix(status.ProxyAddr, "/v1"))
	if err != nil {
		return "", err
	}
	e.cachedModelID = id
	e.cachedModelProfile = status.Profile
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
	fmt.Fprintf(e.Log, time.Now().Format("15:04:05")+" "+format+"\n", args...)
}

func validateJSON(s string) error {
	var v any
	return json.Unmarshal([]byte(s), &v)
}
