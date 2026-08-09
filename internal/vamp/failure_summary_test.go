package vamp

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFailureSummary_SingleConnectionRefused(t *testing.T) {
	err := fmt.Errorf("stage write_script: HTTP request: dial tcp 127.0.0.1:9000: connect: connection refused")
	var buf bytes.Buffer
	writeFailureSummary(&buf, "episodic-run", err, nil)
	out := buf.String()
	for _, want := range []string{
		`FAILED — pipeline "episodic-run" returned 1 stage error`,
		`stage:  write_script`,
		`reason:`,
		`connection refused`,
		"Is the backend running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestFailureSummary_JoinedMultipleErrors(t *testing.T) {
	joined := errors.Join(
		fmt.Errorf("stage write_script: HTTP 500"),
		fmt.Errorf("stage showrunner: unmarshal: invalid character"),
	)
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", joined, nil)
	out := buf.String()
	if !strings.Contains(out, "returned 2 stage error") {
		t.Errorf("expected 2-error count, got:\n%s", out)
	}
	// Should show the FIRST error in detail.
	if !strings.Contains(out, "stage:  write_script") {
		t.Errorf("expected first stage shown, got:\n%s", out)
	}
	// Should mention there's another one.
	if !strings.Contains(out, "+1 more") {
		t.Errorf("expected +N more line, got:\n%s", out)
	}
}

func TestFailureSummary_NoOpOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", nil, nil)
	if buf.Len() != 0 {
		t.Errorf("expected no output on nil err, got:\n%s", buf.String())
	}
}

func TestFailureSummary_InvalidOutputHint(t *testing.T) {
	err := fmt.Errorf("stage extract_units: invalid_output: validateJSON failed")
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", err, nil)
	if !strings.Contains(buf.String(), "max_tokens or tighten the prompt") {
		t.Errorf("expected invalid_output hint, got:\n%s", buf.String())
	}
}

func TestFailureSummary_UnknownPattern(t *testing.T) {
	// No known pattern → no hint line.
	err := fmt.Errorf("stage x: something nobody has seen before")
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", err, nil)
	out := buf.String()
	if !strings.Contains(out, "stage:  x") {
		t.Errorf("stage line missing")
	}
	if strings.Contains(out, "hint:") {
		t.Errorf("unexpected hint line for unknown pattern: %s", out)
	}
}

// TestFailureSummary_PrintsTheResumeCommand is the highest
// value-per-character item in the review: --resume is fully built (per-
// item foreach granularity, pipeline-drift detection, JSON revalidation
// of resumed outputs) and no user-facing output mentioned it anywhere.
// A ten-stage pipeline that died at stage nine printed a run dir, an
// error, and no way to know that re-running the whole thing was not the
// only option.
func TestFailureSummary_PrintsTheResumeCommand(t *testing.T) {
	runDir := t.TempDir()
	e := &Executor{RunDir: runDir}
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", fmt.Errorf("stage nine: boom"), e)
	out := buf.String()
	if !strings.Contains(out, "resume:") {
		t.Fatalf("failure summary never mentions --resume:\n%s", out)
	}
	// The whole command, not just the flag: a path the operator has to
	// reassemble by hand is a path they get wrong.
	if !strings.Contains(out, "--resume "+runDir) {
		t.Errorf("resume line must carry the run dir verbatim:\n%s", out)
	}
	if !strings.Contains(out, " run ") {
		t.Errorf("resume line must be a runnable command:\n%s", out)
	}
}

// TestPipelineArg covers the argv shapes the resume line has to survive.
// The one that motivated it is `vamp run --input k=v pipeline.yaml`: a
// positional read of argv[2] finds the flag, and the printed command
// then has no pipeline in it — which cobra rejects, leaving the operator
// to repair by hand the line that exists so they would not have to.
func TestPipelineArg(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"canonical", []string{"vamp", "run", "pipeline.yaml"}, "pipeline.yaml"},
		{"flags first", []string{"vamp", "run", "--input", "k=v", "ep.yaml"}, "ep.yaml"},
		{"flags after", []string{"vamp", "run", "ep.yml", "--detach"}, "ep.yml"},
		{"uppercase ext", []string{"vamp", "run", "Ep.YAML"}, "Ep.YAML"},
		{"path", []string{"vamp", "run", "/a/b/ep.yaml"}, "/a/b/ep.yaml"},
		// A mounted pipeline binary: `run` takes no positional at all.
		{"mounted", []string{"my-pipeline", "run"}, ""},
		{"mounted detached", []string{"my-pipeline", "run", "--internal-run-job"}, ""},
		// Not a run invocation, and the degenerate argv a test binary has.
		{"other subcommand", []string{"vamp", "validate", "ep.yaml"}, ""},
		{"short argv", []string{"vamp"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pipelineArg(c.argv); got != c.want {
				t.Errorf("pipelineArg(%q) = %q, want %q", c.argv, got, c.want)
			}
		})
	}
}

// TestFailureSummary_NoResumeLineWhenAlreadyResuming: the command is
// still correct on a resumed run, but the operator has already found
// it, and repeating it costs a line of the thing they are reading to
// find out what broke.
func TestFailureSummary_NoResumeLineWhenAlreadyResuming(t *testing.T) {
	runDir := t.TempDir()
	e := &Executor{RunDir: runDir, ResumeDir: runDir}
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", fmt.Errorf("stage nine: boom"), e)
	if strings.Contains(buf.String(), "resume:") {
		t.Errorf("a resumed run should not re-advertise --resume:\n%s", buf.String())
	}
}

// TestFailureSummary_NoRunDirNoResumeLine: a dry run or an internal-API
// caller has no directory to resume from, and a resume command naming
// an empty path is worse than none.
func TestFailureSummary_NoRunDirNoResumeLine(t *testing.T) {
	var buf bytes.Buffer
	writeFailureSummary(&buf, "p", fmt.Errorf("stage nine: boom"), &Executor{})
	if strings.Contains(buf.String(), "resume:") {
		t.Errorf("no run dir means no resume line:\n%s", buf.String())
	}
	buf.Reset()
	writeFailureSummary(&buf, "p", fmt.Errorf("stage nine: boom"), nil)
	if strings.Contains(buf.String(), "resume:") {
		t.Errorf("nil executor means no resume line:\n%s", buf.String())
	}
}
