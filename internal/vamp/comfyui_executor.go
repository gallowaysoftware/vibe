package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/comfyui"
)

// comfyuiExecutor implements StageExecutor for ComfyUI image-generation stages.
// It loads a workflow JSON, substitutes templated parameter values into the
// workflow's node inputs, submits the workflow to a vibe-managed ComfyUI
// backend, waits for completion, and copies the rendered file(s) into the run
// dir. The DAG scheduler is responsible for activating the right vibe profile
// before Execute is invoked (via the same capability-grouped path text stages
// use); we just consume StageInput.BackendAddr.
type comfyuiExecutor struct {
	// pollInterval is the history-polling cadence handed to
	// comfyui.Client.WaitForCompletion. Zero or negative falls back to one
	// second to keep tests cheap.
	pollInterval time.Duration
	// newClient is the constructor for the underlying ComfyUI client; tests
	// override it to inject a custom http.Client. Defaults to
	// comfyui.New(baseURL, http.DefaultClient).
	newClient func(baseURL string) *comfyui.Client
}

// Compile-time guarantee that comfyuiExecutor satisfies StageExecutor.
var _ StageExecutor = (*comfyuiExecutor)(nil)

// Execute runs one ComfyUI workflow invocation. For foreach stages the runner
// calls this once per item with StageInput.Item populated; non-foreach
// invocations have Item=nil and ItemIdx=0.
//
// Side effects: writes one rendered file to <RunDir>/<rendered output path>
// and reports that path in StageOutput.Files as an ABSOLUTE path. Downstream
// stages render subprocesses (ffmpeg/Piper) from the daemon's CWD, so a
// relative path can't be opened by them; absolute is the only safe shape.
// StageOutput.Text is empty so the runner knows to skip its text-stage
// write-back path.
func (e *comfyuiExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("comfyui: missing stage")
	}
	if in.BackendAddr == "" {
		return nil, fmt.Errorf("stage %s: comfyui backend address is empty (vibe profile must expose a ComfyUI backend)", st.ID)
	}

	// Load the workflow JSON. UseNumber preserves int seeds as exact
	// integers — important because ComfyUI rejects 42.0 for an int input.
	workflow, err := loadWorkflow(st, in.PipelineDir)
	if err != nil {
		return nil, fmt.Errorf("stage %s: %w", st.ID, err)
	}

	// Foreach binding goes into both parameter-template rendering and the
	// output-path render below so users can template both per item.
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}

	if err := applyParameters(workflow, st, in, extra); err != nil {
		return nil, fmt.Errorf("stage %s: %w", st.ID, err)
	}

	client := e.clientFor(in.BackendAddr)
	promptID, err := client.Submit(ctx, workflow)
	if err != nil {
		return nil, fmt.Errorf("stage %s: submit workflow: %w", st.ID, err)
	}

	interval := e.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	history, err := client.WaitForCompletion(ctx, promptID, interval)
	if err != nil {
		return nil, fmt.Errorf("stage %s: wait for completion: %w", st.ID, err)
	}

	files, counts := collectOutputs(history)
	if len(files) == 0 {
		return nil, fmt.Errorf("stage %s: workflow %s produced no output files", st.ID, promptID)
	}
	if len(files) > 1 {
		// TODO(multi-output): support an `outputs:` plural in the stage schema
		// that maps each file to a templated path. For Phase 2 we fail clearly
		// rather than guess how to fan out multiple files into a single
		// `output:` template. When the files span more than one kind (e.g. an
		// image + a video), say so explicitly so users know to drop one of the
		// save nodes; otherwise nudge them at batch_size like the
		// single-kind multi-file case.
		if mixedKindCount(counts) > 1 {
			return nil, fmt.Errorf("stage %s: workflow produced multiple output kinds: %s; vamp currently supports one output per stage (drop the extra save node, or split into multiple stages)", st.ID, formatOutputCounts(counts))
		}
		return nil, fmt.Errorf("stage %s: workflow produced %d output files; vamp currently supports one output per stage (set batch_size: 1 in your workflow, or split into multiple stages)", st.ID, len(files))
	}

	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	dest := filepath.Join(in.RunDir, outRel)
	if _, err := client.SaveOutputToFile(ctx, files[0], dest); err != nil {
		return nil, fmt.Errorf("stage %s: save output: %w", st.ID, err)
	}
	// Report ABSOLUTE path: downstream {{ .stages.X.output(s) }} references
	// land on argv strings consumed by ffmpeg/Piper subprocesses running
	// from the daemon's CWD, which can't resolve a path relative to RunDir.
	return &StageOutput{Files: []string{dest}}, nil
}

// clientFor returns a ComfyUI client targeting baseURL, threading the test
// override hook when set.
func (e *comfyuiExecutor) clientFor(baseURL string) *comfyui.Client {
	if e.newClient != nil {
		return e.newClient(baseURL)
	}
	return comfyui.New(baseURL, http.DefaultClient)
}

// loadWorkflow reads st.Workflow (relative to pipelineDir when not absolute,
// or from st.AssetFS when set) and parses it as a ComfyUI workflow map
// (node_id -> node definition).
func loadWorkflow(st *Stage, pipelineDir string) (map[string]any, error) {
	if st == nil || st.Workflow == "" {
		return nil, errors.New("workflow path is empty")
	}
	data, err := readStageAsset(st, pipelineDir, st.Workflow)
	if err != nil {
		return nil, fmt.Errorf("read workflow %s: %w", st.Workflow, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", st.Workflow, err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("workflow %s: top-level must be a JSON object, got %T", st.Workflow, raw)
	}
	return m, nil
}

// applyParameters renders every Stage.Parameters template and writes the
// type-coerced value into the workflow at the requested node/input. Sorted
// iteration so error messages and (in tests) workflow mutation order are
// deterministic.
func applyParameters(workflow map[string]any, st *Stage, in StageInput, extra map[string]any) error {
	keys := make([]string, 0, len(st.Parameters))
	for k := range st.Parameters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		tmpl := st.Parameters[key]
		nodeID, inputName, ok := strings.Cut(key, ".")
		if !ok {
			// Validate() should have rejected this; the runtime check stays as
			// a belt-and-braces guard so an in-memory pipeline that bypasses
			// validation still surfaces a clear error.
			return fmt.Errorf("parameters key %q is malformed (want \"<node_id>.<input_name>\")", key)
		}
		rendered, err := renderTemplate(st.ID+":param:"+key, tmpl, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if err != nil {
			return fmt.Errorf("render parameter %s: %w", key, err)
		}
		node, ok := workflow[nodeID].(map[string]any)
		if !ok {
			return fmt.Errorf("parameter %s: node %q does not exist in workflow (or is not an object)", key, nodeID)
		}
		inputs, ok := node["inputs"].(map[string]any)
		if !ok {
			// ComfyUI workflows always declare inputs as an object; a missing
			// inputs key means the user pointed at the wrong node (a Note
			// node or similar), which is worth surfacing distinctly from
			// "node doesn't exist".
			return fmt.Errorf("parameter %s: node %q has no inputs object (is it a Note / non-executable node?)", key, nodeID)
		}
		if _, exists := inputs[inputName]; !exists {
			return fmt.Errorf("parameter %s: input %q does not exist on node %q", key, inputName, nodeID)
		}
		inputs[inputName] = coerceParamValue(rendered)
	}
	return nil
}

// coerceParamValue maps a rendered string template result to the most
// specific JSON-friendly Go type. ComfyUI's node-input types are mixed —
// seeds and steps are ints, cfg/denoise are floats, prompts are strings,
// some toggles are bools — so we sniff the rendered value against each in
// turn.
//
// The ordering is deliberate:
//   - bools first ("true"/"false") so a literal "true" doesn't get parsed
//     into a number;
//   - ints before floats so "42" stays an int64 and doesn't become 42.0,
//     which trips ComfyUI's int-typed seed/steps inputs;
//   - floats next, gated by hasDigit so strconv doesn't accept odd forms
//     like "." / "+" / "-Inf" as floats;
//   - everything else stays a string (prompts, model paths, etc.).
//
// We restrict bool parsing to canonical lowercase forms so an editorial
// "True." in a prompt stays a string — strconv.ParseBool would happily
// accept "1"/"t"/"True" and surprise users.
func coerceParamValue(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if hasDigit(s) {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	}
	return s
}

// hasDigit reports whether s contains at least one ASCII digit. Cheap guard
// against strconv.ParseFloat accepting weird inputs.
func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// outputCounts is a per-kind tally of files seen across every node in a
// completed workflow's history. Used by the executor's multi-output guard
// to render a precise error message ("got 1 image + 1 video", etc.).
type outputCounts struct {
	Images int
	Videos int
	Gifs   int
}

// collectOutputs flattens every node's outputs into a single slice in
// node-id order, and within each node iterates Images, then Videos, then
// Gifs. That ordering is deterministic so a workflow with multiple save
// nodes produces a stable file list; the per-kind counts are returned
// alongside so the multi-output guard can phrase its error usefully.
// (Phase 2 still errors on len>1; a future `outputs:` schema will need
// this stable order to map files to template names.)
func collectOutputs(h *comfyui.History) ([]comfyui.OutputFile, outputCounts) {
	var counts outputCounts
	if h == nil {
		return nil, counts
	}
	ids := make([]string, 0, len(h.Outputs))
	for id := range h.Outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var files []comfyui.OutputFile
	for _, id := range ids {
		out := h.Outputs[id]
		files = append(files, out.Images...)
		files = append(files, out.Videos...)
		files = append(files, out.Gifs...)
		counts.Images += len(out.Images)
		counts.Videos += len(out.Videos)
		counts.Gifs += len(out.Gifs)
	}
	return files, counts
}

// mixedKindCount returns how many distinct output kinds had at least one
// file. Used to phrase the multi-output guard differently when a workflow
// saves, e.g., both an image and a video.
func mixedKindCount(c outputCounts) int {
	n := 0
	if c.Images > 0 {
		n++
	}
	if c.Videos > 0 {
		n++
	}
	if c.Gifs > 0 {
		n++
	}
	return n
}

// formatOutputCounts renders a per-kind tally as a human-readable list, e.g.
// "1 image + 2 video". Only kinds with a non-zero count appear, in the same
// order as collectOutputs concatenates them so the message matches the
// flatten order.
func formatOutputCounts(c outputCounts) string {
	var parts []string
	if c.Images > 0 {
		parts = append(parts, fmt.Sprintf("%d image", c.Images))
	}
	if c.Videos > 0 {
		parts = append(parts, fmt.Sprintf("%d video", c.Videos))
	}
	if c.Gifs > 0 {
		parts = append(parts, fmt.Sprintf("%d gif", c.Gifs))
	}
	return strings.Join(parts, " + ")
}
