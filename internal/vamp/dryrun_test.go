package vamp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecutor_DryRunRendersAllStages verifies the cross-cutting dry-run
// happy path: a pipeline with a text stage, a comfyui stage, and an ffmpeg
// stage produces a plan containing (a) the rendered prompt text, (b) the
// substituted workflow node value, and (c) the assembled ffmpeg argv —
// without any executor calls firing (inference, comfyui submit, ffmpeg
// subprocess are absent). The inference function is wired to fail loudly
// if invoked so a regression that calls Run instead of DryRun trips it.
func TestExecutor_DryRunRendersAllStages(t *testing.T) {
	t.Helper()
	// Inference would only fire under Run, never DryRun. Mark a flag if
	// it's called so we fail with a clear message rather than silently
	// passing.
	inf := func(_ context.Context, _, _, _ string, _ map[string]any, _ StreamFunc) (string, error) {
		t.Errorf("inference must not be invoked during DryRun")
		return "", nil
	}

	// Minimal comfyui workflow on disk. Two nodes: a CLIP encoder whose
	// "text" input is the parameter target, and a SaveImage to give the
	// workflow a sink. We don't submit it; the dry-run loads it, runs
	// applyParameters in memory, and prints the substituted value.
	tmp := t.TempDir()
	workflowJSON := `{
  "4": {"class_type": "CLIPTextEncode", "inputs": {"text": "placeholder", "clip": ["3", 1]}},
  "9": {"class_type": "SaveImage", "inputs": {"filename_prefix": "vamp", "images": ["4", 0]}}
}`
	if err := os.WriteFile(filepath.Join(tmp, "wf.json"), []byte(workflowJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code", "image_gen": "comfy"}}
	pipeline := &Pipeline{
		Name: "all_types",
		Stages: []Stage{
			{ID: "outline", Capability: "reasoning", Prompt: "Outline: {{.inputs.topic}}", Output: "outline.md"},
			{
				ID: "render", Type: StageTypeComfyUI, Capability: "image_gen",
				Inputs:   []string{"outline"},
				Workflow: "wf.json",
				Parameters: map[string]string{
					"4.text": "cover for {{.inputs.topic}}",
				},
				Output: "cover.png",
			},
			{
				ID: "assemble", Type: StageTypeFFmpeg,
				Inputs: []string{"render", "outline"},
				FFmpegArgs: []string{
					"-loop", "1",
					"-i", "{{.stages.render.output}}",
				},
				Output: "final.mp4",
			},
		},
	}

	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline:     pipeline,
		PipelineDir:  tmp,
		Capabilities: caps,
		Inputs:       map[string]string{"topic": "robots"},
		RunDir:       t.TempDir(),
		Log:          &logBuf,
		Inference:    inf,
	}
	if err := exec.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	got := logBuf.String()
	for _, want := range []string{
		`stage "outline"`,
		`capability: reasoning -> profile: code`,
		"Outline: robots",
		`stage "render"`,
		`capability: image_gen -> profile: comfy`,
		`workflow: wf.json`,
		"4.text = cover for robots",
		`stage "assemble"`,
		"subprocess: ffmpeg",
		// argv must contain the rendered output substitution and the trailing
		// -y <abs path> appended by the dry-run mirror of the executor.
		"-loop",
		"-y",
		"final.mp4",
		"dry-run:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DryRun output missing %q.\nFull output:\n%s", want, got)
		}
	}
	// Ensure NO actual run-side write happened: the RunDir should be empty.
	entries, err := os.ReadDir(exec.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("DryRun must not write any files to RunDir, found: %v", names)
	}
}

// TestExecutor_DryRunDetectsTemplateError verifies that a prompt referencing
// an undefined input surfaces a clear error. The Go template "missingkey=error"
// path is the same one Run uses, so the diagnostic is identical.
func TestExecutor_DryRunDetectsTemplateError(t *testing.T) {
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	pipeline := &Pipeline{
		Name: "bad_template",
		Stages: []Stage{
			{ID: "a", Capability: "reasoning", Prompt: "Hello {{.inputs.missing}}", Output: "a.txt"},
		},
	}
	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Inputs:       map[string]string{}, // no "missing" entry
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	err := exec.DryRun(context.Background())
	if err == nil {
		t.Fatal("expected DryRun to fail on undefined template variable")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected error to mention the missing key, got: %v", err)
	}
}

// TestExecutor_DryRunDetectsCapabilityMissing verifies that a stage whose
// capability is absent from the loaded capabilities map fails with a
// diagnostic that names the missing capability.
func TestExecutor_DryRunDetectsCapabilityMissing(t *testing.T) {
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	pipeline := &Pipeline{
		Name: "bad_capability",
		Stages: []Stage{
			{ID: "a", Capability: "vision", Prompt: "x", Output: "a.txt"},
		},
	}
	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	err := exec.DryRun(context.Background())
	if err == nil {
		t.Fatal("expected DryRun to fail on missing capability mapping")
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("expected error to mention the missing capability %q, got: %v", "vision", err)
	}
}

// TestExecutor_DryRunForeachItemsFromUpstream verifies that when the upstream
// stage hasn't been executed (DryRun never executes anything), the foreach
// resolver falls back to the canned 2-item stub and the printed plan
// explicitly says so.
func TestExecutor_DryRunForeachItemsFromUpstream(t *testing.T) {
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	pipeline := &Pipeline{
		Name: "foreach_stub",
		Stages: []Stage{
			{
				ID: "titles", Capability: "reasoning",
				Prompt: "list 3 titles", Output: "titles.json", OutputFormat: "json",
			},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Prompt:  "expand {{.title}}",
				Output:  "items/{{.title | slugify}}.txt",
			},
		},
	}
	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	if err := exec.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	got := logBuf.String()
	// The text-stage producer is declared with OutputFormat: json, so the
	// dry-run state's synthetic .output is a parseable JSON array, NOT the
	// stub fallback. The consumer should iterate over the synthetic array
	// without a stubbed warning. Verify both shapes:
	//   - the producer stage is listed first;
	//   - the consumer renders two items (the canonical stub size) and the
	//     output paths are derived from those items.
	for _, want := range []string{
		`stage "titles"`,
		`stage "consumer"`,
		`foreach over titles`,
		"item 0 (id=item-1)",
		"items/item-1.txt",
		"item 1 (id=item-2)",
		"items/item-2.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DryRun output missing %q.\nFull output:\n%s", want, got)
		}
	}

	// Now exercise the truly-stubbed path: a foreach whose upstream's
	// .output isn't valid JSON (or is empty) must fall back to the canned
	// stub and the dry-run must print the explanatory note. Easiest way to
	// trigger that here is a foreach whose producer is a plain-text stage
	// (so the producer's synthetic .output is the "<dry-run output of ...>"
	// placeholder, which is NOT a JSON array).
	logBuf.Reset()
	pipeline2 := &Pipeline{
		Name: "foreach_stub_plain",
		Stages: []Stage{
			{ID: "src", Capability: "reasoning", Prompt: "P", Output: "src.json", OutputFormat: "json"},
			{ID: "passthrough", Capability: "reasoning", Inputs: []string{"src"}, Prompt: "P2", Output: "passthrough.txt"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"passthrough"},
				Foreach: &ForeachSpec{From: "passthrough", Var: "title"},
				Prompt:  "do {{.title}}",
				Output:  "items/{{.title | slugify}}.txt",
			},
		},
	}
	exec2 := &Executor{
		Pipeline:     pipeline2,
		Capabilities: caps,
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	if err := exec2.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun (stubbed): %v", err)
	}
	stubGot := logBuf.String()
	if !strings.Contains(stubGot, "not yet executed") || !strings.Contains(stubGot, "stub item") {
		t.Errorf("expected stubbed-upstream notice in dry-run output, got:\n%s", stubGot)
	}
	// And the consumer still prints both stubbed items.
	for _, want := range []string{"item 0 (id=item-1)", "item 1 (id=item-2)"} {
		if !strings.Contains(stubGot, want) {
			t.Errorf("expected %q in stubbed dry-run output, got:\n%s", want, stubGot)
		}
	}
}

// TestExecutor_DryRunDetectsForeachCollision verifies that two foreach items
// whose rendered output paths collide cause an error during dry-run, with the
// failing item indices included in the diagnostic so the user can find them.
func TestExecutor_DryRunDetectsForeachCollision(t *testing.T) {
	caps := &Capabilities{Mapping: map[string]string{"reasoning": "code"}}
	pipeline := &Pipeline{
		Name: "collide",
		Stages: []Stage{
			{ID: "src", Capability: "reasoning", Prompt: "P", Output: "src.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"src"},
				Foreach: &ForeachSpec{From: "src", Var: "title"},
				Prompt:  "do {{.title}}",
				// Constant output path: every item produces "out.txt".
				// The dry-run pre-render must catch the collision before
				// printing any items.
				Output: "out.txt{{ printf \"\" }}",
			},
		},
	}
	var logBuf bytes.Buffer
	exec := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	err := exec.DryRun(context.Background())
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("expected 'collision' in error, got: %v", err)
	}
}
