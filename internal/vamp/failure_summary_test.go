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
