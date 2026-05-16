package vamp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConfirm_ExternalAccept exercises the marker-file path: the executor
// is forced into "not a TTY" mode, writes <stage-id>.pending, and a
// helper goroutine writes "accepted" into <stage-id>.response. The stage
// returns successfully and emits the canonical "accepted" body.
func TestConfirm_ExternalAccept(t *testing.T) {
	runDir := t.TempDir()
	st := &Stage{ID: "approve", Type: StageTypeConfirm, Message: "ok?", Output: "approve.txt"}

	c := &confirmExecutor{
		pollInterval:      10 * time.Millisecond,
		isStdinTTYForTest: func() bool { return false },
	}

	// Race a goroutine that writes the response file once the executor
	// has written the pending marker. We poll for the marker so the
	// helper doesn't write the response before the executor is ready
	// to read it (the executor `os.Remove`s a stale response on entry).
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(PendingFilePath(runDir, st.ID)); err == nil {
				_ = os.WriteFile(ResponseFilePath(runDir, st.ID), []byte("accepted\n"), 0o644)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("pending marker never appeared")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := StageInput{Stage: st, RunDir: runDir}
	out, err := c.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil || out.Text != confirmAcceptedBody {
		t.Errorf("Execute output = %+v, want %q", out, confirmAcceptedBody)
	}
	// Pending marker should be cleaned up on the way out.
	if _, err := os.Stat(PendingFilePath(runDir, st.ID)); !os.IsNotExist(err) {
		t.Errorf("pending marker still present (err=%v)", err)
	}
}

// TestConfirm_ExternalReject mirrors TestConfirm_ExternalAccept but writes
// "rejected" into the response file. The stage must return an error that
// satisfies IsConfirmRejection (so the caller can distinguish a user-
// initiated rejection from an unexpected failure).
func TestConfirm_ExternalReject(t *testing.T) {
	runDir := t.TempDir()
	st := &Stage{ID: "approve", Type: StageTypeConfirm, Message: "ok?", Output: "approve.txt"}

	c := &confirmExecutor{
		pollInterval:      10 * time.Millisecond,
		isStdinTTYForTest: func() bool { return false },
	}

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(PendingFilePath(runDir, st.ID)); err == nil {
				_ = os.WriteFile(ResponseFilePath(runDir, st.ID), []byte("rejected\n"), 0o644)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("pending marker never appeared")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := StageInput{Stage: st, RunDir: runDir}
	_, err := c.Execute(ctx, in)
	if err == nil {
		t.Fatal("Execute: expected error, got nil")
	}
	if !IsConfirmRejection(err) {
		t.Errorf("Execute error not a confirm rejection: %v", err)
	}
	if !errors.Is(err, errStageRejected) {
		t.Errorf("Execute error not wrapping errStageRejected: %v", err)
	}
}

// TestConfirm_Timeout verifies that a stage configured with a short
// Timeout rejects on its own when no response file ever arrives.
func TestConfirm_Timeout(t *testing.T) {
	runDir := t.TempDir()
	st := &Stage{ID: "approve", Type: StageTypeConfirm, Message: "ok?", Output: "approve.txt", Timeout: 50 * time.Millisecond}

	c := &confirmExecutor{
		pollInterval:      10 * time.Millisecond,
		isStdinTTYForTest: func() bool { return false },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	in := StageInput{Stage: st, RunDir: runDir}
	_, err := c.Execute(ctx, in)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Execute: expected timeout error, got nil")
	}
	if !IsConfirmRejection(err) {
		t.Errorf("timeout should be reported as rejection: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("timeout took %s, expected ~50ms", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %v should mention timeout", err)
	}
}

// TestConfirm_StdinAccept drives the foreground (TTY) path by injecting a
// strings.Reader for stdin and asserting the stage returns ok.
func TestConfirm_StdinAccept(t *testing.T) {
	runDir := t.TempDir()
	st := &Stage{ID: "approve", Type: StageTypeConfirm, Message: "ok?", Output: "approve.txt"}

	var logBuf bytes.Buffer
	c := &confirmExecutor{
		pollInterval:      10 * time.Millisecond,
		isStdinTTYForTest: func() bool { return true },
		StdinForTest:      strings.NewReader("y\n"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := StageInput{Stage: st, RunDir: runDir, Log: &logBuf}
	out, err := c.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == nil || out.Text != confirmAcceptedBody {
		t.Errorf("Execute output = %+v, want %q", out, confirmAcceptedBody)
	}
	if !strings.Contains(logBuf.String(), "approve?") {
		t.Errorf("log should contain prompt; got %q", logBuf.String())
	}
}

// TestConfirm_StdinReject verifies the foreground path rejects on any
// non-y input. Kept as a sibling to TestConfirm_StdinAccept so the two
// branches of the readLine path are covered by tests that don't depend
// on the marker-file machinery.
func TestConfirm_StdinReject(t *testing.T) {
	runDir := t.TempDir()
	st := &Stage{ID: "approve", Type: StageTypeConfirm, Message: "ok?", Output: "approve.txt"}

	c := &confirmExecutor{
		pollInterval:      10 * time.Millisecond,
		isStdinTTYForTest: func() bool { return true },
		StdinForTest:      strings.NewReader("n\n"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	in := StageInput{Stage: st, RunDir: runDir}
	_, err := c.Execute(ctx, in)
	if err == nil {
		t.Fatal("Execute: expected rejection error")
	}
	if !IsConfirmRejection(err) {
		t.Errorf("Execute error not a confirm rejection: %v", err)
	}
}

// TestConfirmCmd_WritesResponseFile exercises the ResponseFilePath helper
// the same way vamp confirm would: write "accepted" via the same path the
// stage polls. Sanity check that the constants are in lock-step between
// the executor and the CLI helper.
func TestConfirmCmd_WritesResponseFile(t *testing.T) {
	runDir := t.TempDir()
	path := ResponseFilePath(runDir, "stageA")
	if err := os.WriteFile(path, []byte("accepted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := filepath.Base(path)
	want := "stageA" + ResponseFileSuffix
	if got != want {
		t.Errorf("response file basename = %q, want %q", got, want)
	}
}
