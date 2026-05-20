package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRenderTestPipeline drops a minimal two-stage pipeline at <dir>/p.yaml
// and returns its absolute path. Stage `a` has an inline prompt that
// references a pipeline input; stage `b` depends on `a` and renders
// `.stages.a.output`. That shape is the smallest one that exercises both
// the placeholder path (no --run-dir) and the on-disk path (--run-dir set).
func writeRenderTestPipeline(t *testing.T, dir string) string {
	t.Helper()
	p := `name: render-test
inputs:
  who:
    default: world
stages:
  - id: a
    capability: stub
    prompt: "hello {{ .inputs.who }}"
    output: a.txt
  - id: b
    capability: stub
    inputs:
      - a
    prompt: "got: {{ .stages.a.output }}"
    output: b.txt
`
	path := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(path, []byte(p), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRenderCmd_NoRunDir confirms that rendering a downstream stage
// without a --run-dir substitutes the documented placeholder for the
// upstream output. The exact placeholder format is part of the CLI
// contract (users grep for "-output-placeholder" when reading rendered
// prompts), so we assert on the literal string.
func TestRenderCmd_NoRunDir(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := writeRenderTestPipeline(t, dir)

	cmd := renderCmd()
	// Inject the root so cobra's --help / completion plumbing has a parent.
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{pipelinePath, "b"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("render b: %v\noutput: %s", err, out.String())
	}
	got := out.String()
	if want := "got: a-output-placeholder"; got != want {
		t.Errorf("render b = %q, want %q", got, want)
	}
}

// TestRenderCmd_WithRunDir seeds a fake run dir with the prior stage's
// output on disk and confirms render reads it back into the downstream
// template. This exercises the dependency-path resolution path (we have
// to render the dep's Output template to find the file) plus the
// readFile-and-bind path.
func TestRenderCmd_WithRunDir(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := writeRenderTestPipeline(t, dir)
	runDir := filepath.Join(dir, "run1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "a.txt"), []byte("hello there"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := renderCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{pipelinePath, "b", "--run-dir", runDir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("render b --run-dir: %v\noutput: %s", err, out.String())
	}
	if want := "got: hello there"; out.String() != want {
		t.Errorf("render b = %q, want %q", out.String(), want)
	}
}

// TestRenderCmd_InputOverride confirms that --input overrides the
// default for an input the rendered stage references — same flag shape
// as `vamp run`. The first stage references {{ .inputs.who }} so this
// is the natural cross-check.
func TestRenderCmd_InputOverride(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := writeRenderTestPipeline(t, dir)

	cmd := renderCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{pipelinePath, "a", "--input", "who=universe"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("render a: %v\noutput: %s", err, out.String())
	}
	if want := "hello universe"; out.String() != want {
		t.Errorf("render a = %q, want %q", out.String(), want)
	}
}

// TestRenderCmd_UnknownStage confirms a typo in the stage id surfaces a
// helpful error listing the valid ids.
func TestRenderCmd_UnknownStage(t *testing.T) {
	dir := t.TempDir()
	pipelinePath := writeRenderTestPipeline(t, dir)

	cmd := renderCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{pipelinePath, "nope"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown stage id")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nope") || !strings.Contains(msg, "a") || !strings.Contains(msg, "b") {
		t.Errorf("error %q should mention the typo and the available ids", msg)
	}
}
