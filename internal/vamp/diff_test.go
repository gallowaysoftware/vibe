package vamp

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeDiffRun is a small helper that constructs a synthetic run dir
// under root, populated with the canonical artifacts vamp.Compare reads:
// pipeline.json, pipeline.yaml.snapshot, inputs.json, and an arbitrary
// set of stage output files. The helper is intentionally minimal — each
// argument maps 1:1 to a file the differ inspects, so tests can focus
// on the comparison logic rather than fixture construction.
func writeDiffRun(t *testing.T, root, base string, rec *RunRecord, yamlSnap, inputs string, files map[string]string) string {
	t.Helper()
	dir := writeRunDir(t, root, base, rec, nil)
	merged := map[string]string{}
	if yamlSnap != "" {
		merged["pipeline.yaml.snapshot"] = yamlSnap
	}
	if inputs != "" {
		merged["inputs.json"] = inputs
	}
	for k, v := range files {
		merged[k] = v
	}
	// Re-use the existing writeRunDir helper for the file payload —
	// we splat them on top of the run dir we just created.
	writeRunDir(t, root, base, nil, merged)
	return dir
}

func basicRec(name string, start time.Time, stages ...StageRecord) *RunRecord {
	return &RunRecord{
		Name:      name,
		StartTime: start,
		EndTime:   start.Add(10 * time.Second),
		Status:    "ok",
		Stages:    stages,
	}
}

const twoStageYAML = `name: demo
inputs:
  topic:
    type: string
stages:
  - id: script
    type: text
    capability: writing
    prompt: |
      Write a one-line narration about {{.inputs.topic}}.
    output: stages/script.md
  - id: render
    type: text
    capability: writing
    prompt: Render {{.stages.script.output}}
    inputs: [script]
    output: stages/render.md
`

// TestCompare_IdenticalRuns: two byte-for-byte identical run dirs
// produce a Report whose every diff field is empty and whose stage
// presence is "both" for every stage. The pipeline yaml diff in
// particular must be empty (we rely on the fast-path bytes.Equal check
// to skip the line-LCS work for the common rerun case).
func TestCompare_IdenticalRuns(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start,
		StageRecord{ID: "script", DurationMS: 1000, Status: "ok"},
		StageRecord{ID: "render", DurationMS: 2000, Status: "ok"},
	)
	files := map[string]string{
		"stages/script.md": "hello world\n",
		"stages/render.md": "rendered: hello world\n",
	}
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, twoStageYAML, `{"topic":"cats"}`, files)
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, twoStageYAML, `{"topic":"cats"}`, files)
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.PipelineYAMLDiff != "" {
		t.Errorf("PipelineYAMLDiff = %q, want empty", rep.PipelineYAMLDiff)
	}
	for _, in := range rep.Inputs {
		if !in.Equal() {
			t.Errorf("input %s changed: %q -> %q", in.Key, in.A, in.B)
		}
	}
	if len(rep.Stages) != 2 {
		t.Fatalf("len(Stages) = %d, want 2", len(rep.Stages))
	}
	for _, s := range rep.Stages {
		if s.Presence != "both" {
			t.Errorf("stage %s presence = %q, want both", s.ID, s.Presence)
		}
		if s.PromptDiff != "" {
			t.Errorf("stage %s PromptDiff = %q, want empty", s.ID, s.PromptDiff)
		}
		if s.OutputDiff != "" {
			t.Errorf("stage %s OutputDiff = %q, want empty", s.ID, s.OutputDiff)
		}
		if !s.Status.Equal() {
			t.Errorf("stage %s status differs: %q vs %q", s.ID, s.Status.A, s.Status.B)
		}
	}
}

// TestCompare_StagePromptChanged: the second run's input differs, so
// the rendered prompt of the script stage differs between runs. Verify
// the differ surfaces both prompt_diff (rendered template) AND
// output_diff (text content of the output file).
func TestCompare_StagePromptChanged(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start,
		StageRecord{ID: "script", DurationMS: 1000, Status: "ok"},
		StageRecord{ID: "render", DurationMS: 2000, Status: "ok"},
	)
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, twoStageYAML, `{"topic":"rainy alley"}`, map[string]string{
		"stages/script.md": "The rain falls softly on the cobblestones.\n",
		"stages/render.md": "rendered: rainy alley\n",
	})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, twoStageYAML, `{"topic":"neon city"}`, map[string]string{
		"stages/script.md": "Neon signs flicker against the dark.\n",
		"stages/render.md": "rendered: neon city\n",
	})
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	// Find the script stage.
	var script *StageDiff
	for i := range rep.Stages {
		if rep.Stages[i].ID == "script" {
			script = &rep.Stages[i]
		}
	}
	if script == nil {
		t.Fatal("script stage not in report")
	}
	if script.PromptDiff == "" {
		t.Errorf("PromptDiff is empty; want non-empty (rendered prompt differs)")
	}
	// Rendered prompt for script must mention both topics — verify by
	// checking the diff contains both phrases.
	if !strings.Contains(script.PromptDiff, "rainy alley") {
		t.Errorf("PromptDiff missing A topic: %s", script.PromptDiff)
	}
	if !strings.Contains(script.PromptDiff, "neon city") {
		t.Errorf("PromptDiff missing B topic: %s", script.PromptDiff)
	}
	if script.OutputDiff == "" {
		t.Errorf("OutputDiff is empty; want non-empty (output text differs)")
	}
	if !strings.Contains(script.OutputDiff, "The rain falls softly") {
		t.Errorf("OutputDiff missing A content: %s", script.OutputDiff)
	}
	if !strings.Contains(script.OutputDiff, "Neon signs flicker") {
		t.Errorf("OutputDiff missing B content: %s", script.OutputDiff)
	}
	// Inputs diff should record topic changing.
	foundTopic := false
	for _, in := range rep.Inputs {
		if in.Key == "topic" {
			foundTopic = true
			if in.A != "rainy alley" || in.B != "neon city" {
				t.Errorf("topic diff = (%q, %q), want (rainy alley, neon city)", in.A, in.B)
			}
		}
	}
	if !foundTopic {
		t.Errorf("topic input not surfaced in report")
	}
}

// TestCompare_StageOnlyInOneRun: run A has an extra stage. Verify
// the differ marks it with presence "only_a" and that the report
// contains every stage from both runs.
func TestCompare_StageOnlyInOneRun(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	yamlA := `name: demo
stages:
  - id: shared
    type: text
    capability: writing
    prompt: hi
    output: shared.md
  - id: extra
    type: text
    capability: writing
    prompt: extra
    output: extra.md
`
	yamlB := `name: demo
stages:
  - id: shared
    type: text
    capability: writing
    prompt: hi
    output: shared.md
`
	recA := basicRec("demo", start,
		StageRecord{ID: "shared", DurationMS: 100, Status: "ok"},
		StageRecord{ID: "extra", DurationMS: 200, Status: "ok"},
	)
	recB := basicRec("demo", start,
		StageRecord{ID: "shared", DurationMS: 100, Status: "ok"},
	)
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", recA, yamlA, `{}`, map[string]string{
		"shared.md": "hi\n",
		"extra.md":  "extra\n",
	})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", recB, yamlB, `{}`, map[string]string{
		"shared.md": "hi\n",
	})
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Stages) != 2 {
		t.Fatalf("len(Stages) = %d, want 2", len(rep.Stages))
	}
	want := map[string]string{"shared": "both", "extra": "only_a"}
	for _, s := range rep.Stages {
		if got := s.Presence; got != want[s.ID] {
			t.Errorf("stage %s presence = %q, want %q", s.ID, got, want[s.ID])
		}
	}
}

// TestCompare_BinaryOutput: the script stage emits a PNG (NUL-byte
// containing payload). Verify the report classifies the output as
// binary on both sides, populates Size + SHA256 for each, and does
// NOT populate OutputDiff (binary content isn't text-diffed).
func TestCompare_BinaryOutput(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	yaml := `name: demo
stages:
  - id: render
    type: comfyui
    capability: image
    workflow: wf.json
    output: render.png
`
	rec := basicRec("demo", start, StageRecord{ID: "render", DurationMS: 5000, Status: "ok"})
	// Two distinct PNG-shaped payloads (NUL byte ensures binary
	// classification regardless of the extension check).
	pngA := string([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 1, 0xAA})
	pngB := string([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 1, 0xBB, 0xCC})
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, yaml, `{}`, map[string]string{"render.png": pngA})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, yaml, `{}`, map[string]string{"render.png": pngB})
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Stages) != 1 {
		t.Fatalf("len(Stages) = %d, want 1", len(rep.Stages))
	}
	s := rep.Stages[0]
	if !s.OutputA.Binary || !s.OutputB.Binary {
		t.Errorf("binary not detected: A=%+v B=%+v", s.OutputA, s.OutputB)
	}
	if s.OutputA.Size == 0 || s.OutputB.Size == 0 {
		t.Errorf("size missing: A=%d B=%d", s.OutputA.Size, s.OutputB.Size)
	}
	if s.OutputA.SHA256 == "" || s.OutputB.SHA256 == "" {
		t.Errorf("sha256 missing: A=%q B=%q", s.OutputA.SHA256, s.OutputB.SHA256)
	}
	if s.OutputA.SHA256 == s.OutputB.SHA256 {
		t.Errorf("sha256 unexpectedly equal for distinct payloads")
	}
	if s.OutputDiff != "" {
		t.Errorf("OutputDiff should be empty for binary outputs, got %q", s.OutputDiff)
	}
	// Markdown render: must mention both sizes (smoke test that the
	// binary-output block reaches the formatter).
	var buf bytes.Buffer
	if err := rep.Markdown(&buf, false); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	mdOut := buf.String()
	if !strings.Contains(mdOut, "(binary)") {
		t.Errorf("Markdown output missing '(binary)' marker:\n%s", mdOut)
	}
}

// TestCompare_JSON: emit JSON, parse it back, and verify the
// top-level shape matches the documented schema. We don't pin exact
// values for nested diff strings (LCS implementation detail) but we
// require the structural keys exist.
func TestCompare_JSON(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "script", DurationMS: 1000, Status: "ok"})
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, twoStageYAML, `{"topic":"x"}`, map[string]string{
		"stages/script.md": "hello a\n",
		"stages/render.md": "render a\n",
	})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, twoStageYAML, `{"topic":"y"}`, map[string]string{
		"stages/script.md": "hello b\n",
		"stages/render.md": "render b\n",
	})
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var buf bytes.Buffer
	if err := rep.JSON(&buf); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, buf.String())
	}
	// Top-level keys.
	for _, key := range []string{"run_a", "run_b", "inputs", "stages", "total_duration_ms"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing top-level key %q in JSON: %s", key, buf.String())
		}
	}
	// run_a / run_b shape.
	runA, ok := got["run_a"].(map[string]any)
	if !ok {
		t.Fatalf("run_a is not an object: %v", got["run_a"])
	}
	if _, ok := runA["id"]; !ok {
		t.Errorf("run_a missing 'id'")
	}
	if _, ok := runA["path"]; !ok {
		t.Errorf("run_a missing 'path'")
	}
	// stages array.
	stages, ok := got["stages"].([]any)
	if !ok || len(stages) == 0 {
		t.Fatalf("stages is empty or wrong type: %v", got["stages"])
	}
}

// TestCompare_NoContent: when NoContent is set, output text content
// must NOT be embedded in the report. Verify both that Content is
// empty and that no OutputDiff was computed even for stages with
// differing text outputs.
func TestCompare_NoContent(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "script", DurationMS: 1, Status: "ok"})
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, twoStageYAML, `{"topic":"x"}`, map[string]string{
		"stages/script.md": "differ\n",
		"stages/render.md": "x\n",
	})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, twoStageYAML, `{"topic":"x"}`, map[string]string{
		"stages/script.md": "different\n",
		"stages/render.md": "x\n",
	})
	rep, err := Compare(a, b, CompareOpts{NoContent: true})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	for _, s := range rep.Stages {
		if s.OutputA.Content != "" || s.OutputB.Content != "" {
			t.Errorf("stage %s: content present under NoContent", s.ID)
		}
		if s.OutputDiff != "" {
			t.Errorf("stage %s: OutputDiff present under NoContent: %s", s.ID, s.OutputDiff)
		}
	}
}

// TestCompare_StageFilter: --stage X restricts the per-stage list to
// just the matching stage id.
func TestCompare_StageFilter(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start,
		StageRecord{ID: "script", DurationMS: 1, Status: "ok"},
		StageRecord{ID: "render", DurationMS: 2, Status: "ok"},
	)
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, twoStageYAML, `{"topic":"x"}`, map[string]string{
		"stages/script.md": "a\n",
		"stages/render.md": "a\n",
	})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, twoStageYAML, `{"topic":"y"}`, map[string]string{
		"stages/script.md": "b\n",
		"stages/render.md": "b\n",
	})
	rep, err := Compare(a, b, CompareOpts{StageFilter: "script"})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Stages) != 1 || rep.Stages[0].ID != "script" {
		t.Errorf("StageFilter ignored: got %v", stageIDs(rep.Stages))
	}
}

// TestCompare_StageFilterNoMatchIsAnError pins that a typo'd --stage
// fails instead of printing a clean, empty, exit-0 report that reads
// like "these runs agree about that stage".
func TestCompare_StageFilterNoMatchIsAnError(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start,
		StageRecord{ID: "script", DurationMS: 1, Status: "ok"},
		StageRecord{ID: "render", DurationMS: 2, Status: "ok"},
	)
	a := writeDiffRun(t, root, "2026-05-15T12-00-00_a", rec, twoStageYAML, `{"topic":"x"}`, map[string]string{
		"stages/script.md": "a\n", "stages/render.md": "a\n",
	})
	b := writeDiffRun(t, root, "2026-05-15T13-00-00_b", rec, twoStageYAML, `{"topic":"y"}`, map[string]string{
		"stages/script.md": "b\n", "stages/render.md": "b\n",
	})
	_, err := Compare(a, b, CompareOpts{StageFilter: "scrpt"})
	if err == nil {
		t.Fatal("expected an error for a stage id present in neither run")
	}
	// The message must name what IS available, or the operator has to
	// go read the yaml to find their typo.
	if !strings.Contains(err.Error(), "script") || !strings.Contains(err.Error(), "render") {
		t.Errorf("message should list the known stage ids, got %v", err)
	}
}

// TestCompare_RefusesOutputPathEscapingTheRunDir pins the containment
// rule on the differ, which was the one of four consumers of a rendered
// `output:` template that did not apply it. Executor.snapshot writes
// pipeline.yaml.snapshot verbatim BEFORE any stage runs, so a template
// the executor would refuse still reaches this code — and the differ
// reads the file and embeds textual content whole into its report.
func TestCompare_RefusesOutputPathEscapingTheRunDir(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	if err := writeFile(secret, "TOP-SECRET-CANARY\n"); err != nil {
		t.Fatal(err)
	}
	// The run dirs live at <root>/<base>, so "../secret.txt" from
	// inside a run dir resolves to the file above.
	escapeYAML := "name: demo\nstages:\n  - id: x\n    type: text\n    capability: w\n" +
		"    prompt: hi\n    output: ../secret.txt\n"
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "x", DurationMS: 1, Status: "ok"})
	a := writeDiffRun(t, root, "a", rec, escapeYAML, `{}`, nil)
	b := writeDiffRun(t, root, "b", rec, escapeYAML, `{}`, nil)

	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "TOP-SECRET-CANARY") {
		t.Errorf("differ read a file outside the run dir into its report:\n%s", blob)
	}
	for _, s := range rep.Stages {
		if s.OutputA.Path == "../secret.txt" && !s.OutputA.Missing {
			t.Errorf("escaping output path resolved: %+v", s.OutputA)
		}
	}
	// The OTHER rung. stagePriorOutputs reads each stage's output into
	// the prior state used to render DOWNSTREAM templates, so an
	// escaping `output:` on stage one lands in stage two's rendered
	// prompt — a different leak, out of a different function, that the
	// stat-side guard above cannot prevent. The two runs must name
	// different files or the rendered prompts match and no diff is
	// emitted at all.
	if err := writeFile(filepath.Join(root, "secretA.txt"), "CANARY-A\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(root, "secretB.txt"), "CANARY-B\n"); err != nil {
		t.Fatal(err)
	}
	chain := func(target string) string {
		return "name: demo\nstages:\n" +
			"  - id: leak\n    type: text\n    capability: w\n    prompt: hi\n    output: ../" + target + "\n" +
			"  - id: consumer\n    type: text\n    capability: w\n    inputs: [leak]\n" +
			"    prompt: 'saw {{.stages.leak.output}}'\n    output: consumer.md\n"
	}
	chainRec := basicRec("demo", start,
		StageRecord{ID: "leak", DurationMS: 1, Status: "ok"},
		StageRecord{ID: "consumer", DurationMS: 1, Status: "ok"},
	)
	e := writeDiffRun(t, root, "e", chainRec, chain("secretA.txt"), `{}`, map[string]string{"consumer.md": "1\n"})
	f := writeDiffRun(t, root, "f", chainRec, chain("secretB.txt"), `{}`, map[string]string{"consumer.md": "2\n"})
	repChain, err := Compare(e, f, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	chainBlob, err := json.Marshal(repChain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chainBlob), "CANARY-A") || strings.Contains(string(chainBlob), "CANARY-B") {
		t.Errorf("prior-state read leaked an out-of-run-dir file into a downstream prompt:\n%s", chainBlob)
	}

	// The same fixture with an in-run-dir output must still resolve, so
	// the guard is not just "the differ stopped reading anything".
	okYAML := "name: demo\nstages:\n  - id: x\n    type: text\n    capability: w\n" +
		"    prompt: hi\n    output: x.md\n"
	c := writeDiffRun(t, root, "c", rec, okYAML, `{}`, map[string]string{"x.md": "VISIBLE\n"})
	d := writeDiffRun(t, root, "d", rec, okYAML, `{}`, map[string]string{"x.md": "VISIBLE\n"})
	repOK, err := Compare(c, d, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(repOK.Stages) != 1 || repOK.Stages[0].OutputA.Missing {
		t.Errorf("legitimate output path refused: %+v", repOK.Stages)
	}
}

// TestUnifiedDiff_TrailingNewlineIsNotReportedAsIdentical closes the
// second empty-return in unifiedDiff. splitLinesKeepEmpty strips exactly
// one trailing newline, so "x\n" and "x" produce identical line slices
// and zero hunks — and the "" that came back is the callers' sentinel
// for "no change", which the renderer prints as "(identical)". A
// missing final newline is one of the commonest real diffs there is.
func TestUnifiedDiff_TrailingNewlineIsNotReportedAsIdentical(t *testing.T) {
	got := unifiedDiff("x\n", "x", "a/out", "b/out")
	if got == "" {
		t.Fatal(`unifiedDiff("x\n", "x") = "" — a real difference reported as its own absence`)
	}
	if !strings.Contains(got, "No newline at end of file") {
		t.Errorf("difference should be legible to a human, got:\n%s", got)
	}
	if !strings.Contains(got, "a/out") || !strings.Contains(got, "b/out") {
		t.Errorf("labels missing from:\n%s", got)
	}
	// Reversed, and the equal case is still the sentinel.
	if unifiedDiff("x", "x\n", "a", "b") == "" {
		t.Error("reversed direction still reported as identical")
	}
	if unifiedDiff("x\n", "x\n", "a", "b") != "" {
		t.Error(`equal inputs must still return "" — it is the no-change sentinel`)
	}
}

// TestCompare_TrailingNewlineChangeSurfacesInTheReport carries the same
// defect through the public surface: two runs whose output differs only
// in its final newline must not render as identical.
func TestCompare_TrailingNewlineChangeSurfacesInTheReport(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "x", DurationMS: 1, Status: "ok"})
	yaml := "name: demo\nstages:\n  - id: x\n    type: text\n    capability: w\n    prompt: hi\n    output: x.md\n"
	a := writeDiffRun(t, root, "a", rec, yaml, `{}`, map[string]string{"x.md": "hello\n"})
	b := writeDiffRun(t, root, "b", rec, yaml, `{}`, map[string]string{"x.md": "hello"})
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Stages) != 1 {
		t.Fatalf("want 1 stage, got %v", stageIDs(rep.Stages))
	}
	if rep.Stages[0].OutputDiff == "" {
		t.Error("output changed but OutputDiff is empty — renderer will print (identical)")
	}
	var buf bytes.Buffer
	if err := rep.Markdown(&buf, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "output: (identical)") {
		t.Errorf("human view claims the outputs are identical:\n%s", buf.String())
	}
}

// TestCompare_RenderStageUnreachablePromptFileStillDiffs closes the
// guard that the text branch had and the render branch did not. When a
// prompt_file can't be read from the run dir — which the code's own
// note calls the normal case — the render branch left raw as "", both
// sides rendered to "", and compareStages concluded the prompt was
// unchanged. A render stage that switched prompt files between two runs
// showed nothing at all.
func TestCompare_RenderStageUnreachablePromptFileStillDiffs(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "r", DurationMS: 1, Status: "ok"})
	mk := func(promptFile string) string {
		return "name: demo\nstages:\n  - id: r\n    type: render\n" +
			"    prompt_file: " + promptFile + "\n    output: r.md\n"
	}
	a := writeDiffRun(t, root, "a", rec, mk("prompts/one.md"), `{}`, map[string]string{"r.md": "x\n"})
	b := writeDiffRun(t, root, "b", rec, mk("prompts/two.md"), `{}`, map[string]string{"r.md": "x\n"})
	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Stages) != 1 {
		t.Fatalf("want 1 stage, got %v", stageIDs(rep.Stages))
	}
	if rep.Stages[0].PromptDiff == "" {
		t.Fatal("render stage changed prompt_file but the diff is empty")
	}
	if !strings.Contains(rep.Stages[0].PromptDiff, "one.md") ||
		!strings.Contains(rep.Stages[0].PromptDiff, "two.md") {
		t.Errorf("diff should name both prompt files, got:\n%s", rep.Stages[0].PromptDiff)
	}
}

func stageIDs(ss []StageDiff) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// TestUnifiedDiff_LineByLine: spot-checks the unified diff format
// (the implementation detail we promised the parent agent). One
// changed line in the middle of two paragraphs should produce a single
// hunk with @@ headers pointing at the right line numbers.
func TestUnifiedDiff_LineByLine(t *testing.T) {
	a := "alpha\nbeta\ngamma\n"
	b := "alpha\nbravo\ngamma\n"
	got := unifiedDiff(a, b, "a", "b")
	if got == "" {
		t.Fatal("unifiedDiff returned empty for differing inputs")
	}
	if !strings.Contains(got, "---") || !strings.Contains(got, "+++") {
		t.Errorf("missing diff headers: %q", got)
	}
	if !strings.Contains(got, "-beta") || !strings.Contains(got, "+bravo") {
		t.Errorf("missing change lines: %q", got)
	}
	if strings.Contains(got, "-alpha") || strings.Contains(got, "-gamma") {
		t.Errorf("unchanged lines should not appear: %q", got)
	}
}

// TestUnifiedDiff_Empty: identical inputs produce an empty string so
// callers can use that as a "no change" sentinel.
func TestUnifiedDiff_Empty(t *testing.T) {
	if got := unifiedDiff("same", "same", "a", "b"); got != "" {
		t.Errorf("expected empty diff for equal inputs, got %q", got)
	}
}

// TestCompare_PathArg: verify Compare accepts absolute paths (not
// just basenames). Run dirs may live outside vamp.RunsDir() in tests
// and on machines where the user passed --run-dir.
func TestCompare_PathArg(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "x", DurationMS: 1, Status: "ok"})
	yaml := "name: demo\nstages:\n  - id: x\n    type: text\n    capability: w\n    prompt: hi\n    output: x.md\n"
	a := writeDiffRun(t, root, "a", rec, yaml, `{}`, map[string]string{"x.md": "a\n"})
	b := writeDiffRun(t, root, "b", rec, yaml, `{}`, map[string]string{"x.md": "a\n"})
	rep, err := Compare(filepath.Clean(a), filepath.Clean(b), CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.RunA.Path == "" || rep.RunB.Path == "" {
		t.Errorf("paths not populated: %+v / %+v", rep.RunA, rep.RunB)
	}
}
