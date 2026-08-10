package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderCmd_RunDirDependencyCannotEscapeTheRunDir drives the
// containment guard through the SHIPPED COMMAND, not through the Go
// function, because the command is where the consequence lives:
// buildRenderPrior takes the rendered dependency path, joins it onto the
// run dir, os.ReadFile's it, and binds the bytes to
// `.stages.<id>.output`, which pipeline_ops.go then prints to stdout.
//
// Before the guard, this exited 0 and printed the contents of a file one
// directory ABOVE the run dir.
//
// The pipeline here is what a fetched pipeline plus a `--input` looks
// like — `output: "{{ .inputs.name }}"` — which is the honest reach of
// this path: the pipeline YAML and --input, NOT a sampled LLM string
// (buildRenderPrior binds every dependency's own prior output to "").
// It is fixed regardless, because `vamp run` and `vamp dry-run` both
// refuse this path and a `render` that resolves what `run` refuses is a
// plan that lies.
func TestRenderCmd_RunDirDependencyCannotEscapeTheRunDir(t *testing.T) {
	base := t.TempDir()
	runDir := filepath.Join(base, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET-OUTSIDE-THE-RUN-DIR"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The legitimate file, so the same pipeline can show the command
	// still works when the path stays inside.
	if err := os.WriteFile(filepath.Join(runDir, "inside.txt"), []byte("INSIDE-THE-RUN-DIR"), 0o644); err != nil {
		t.Fatal(err)
	}

	pipeline := `name: escape-test
inputs:
  name:
    type: string
    required: true
stages:
  - id: dep
    capability: stub
    prompt: "x"
    output: "{{ .inputs.name }}"
  - id: target
    capability: stub
    inputs: [dep]
    prompt: "PRIOR>>{{ .stages.dep.output }}<<"
    output: target.txt
`
	pipelinePath := filepath.Join(base, "p.yaml")
	if err := os.WriteFile(pipelinePath, []byte(pipeline), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, name string) (string, error) {
		t.Helper()
		cmd := renderCmd()
		out := &bytes.Buffer{}
		cmd.SetOut(out)
		cmd.SetErr(out)
		cmd.SetArgs([]string{pipelinePath, "target", "--run-dir", runDir, "--input", "name=" + name})
		err := cmd.Execute()
		return out.String(), err
	}

	t.Run("escaping", func(t *testing.T) {
		got, err := run(t, "../outside/secret.txt")
		if err == nil {
			t.Fatalf("vamp render exited 0 on an escaping dependency path; printed: %q", got)
		}
		if strings.Contains(got, "SECRET-OUTSIDE-THE-RUN-DIR") {
			t.Errorf("the out-of-tree file reached stdout: %q", got)
		}
		if !strings.Contains(err.Error(), "escapes the run dir") {
			t.Errorf("want a run-dir escape refusal, got %v", err)
		}
	})

	t.Run("legitimate", func(t *testing.T) {
		got, err := run(t, "inside.txt")
		if err != nil {
			t.Fatalf("a legitimate dependency path was refused: %v (%s)", err, got)
		}
		if want := "PRIOR>>INSIDE-THE-RUN-DIR<<"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
