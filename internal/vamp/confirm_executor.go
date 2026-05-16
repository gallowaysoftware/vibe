package vamp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// confirmExecutor implements StageExecutor for `type: confirm` stages — the
// human-in-the-loop primitive. The stage prints (or writes to a marker
// file) a rendered approval prompt and blocks until the operator accepts
// or rejects, an optional timeout fires, or the parent context is
// canceled.
//
// Two channels share the same executor:
//
//   - Foreground TTY: when stdin is a character device, the executor prints
//     `Message + " [y/N] "` and reads a line from stdin. `y\n` (case-
//     insensitive) accepts; everything else rejects. This is the path users
//     get when they invoke `vamp run pipeline.yaml` from an interactive
//     shell.
//
//   - External command: when stdin is NOT a TTY (the typical `--detach`
//     case), the executor writes `<run-dir>/<stage-id>.pending` with the
//     rendered message body and polls every 500ms for `<run-dir>/<stage-
//     id>.response`. The operator clears the gate with
//     `vamp confirm <run-dir> <stage-id> [--reject]`, which writes
//     "accepted" or "rejected" into the response file. On accept the stage
//     returns ok; on reject the stage returns errStageRejected which the
//     wave loop translates into a marked-error status (downstream success
//     stages skip, downstream failure/always stages fire).
//
// Caching: confirm stages are deliberately NOT cacheable — re-running a
// pipeline should re-ask, never silently inherit a prior decision. The
// stage-type predicate stageIsCacheable() (in cache.go, sibling agent) is
// expected to special-case StageTypeConfirm.
//
// The stdin override (StdinForTest) and the TTY-detection hook
// (isStdinTTYForTest) exist solely for unit tests; production callers
// leave both unset so the executor reads os.Stdin and probes its mode bits.
type confirmExecutor struct {
	// pollInterval governs how often the external-command path checks for a
	// response file. 500ms is the spec-mandated default; tests override it
	// to drive the goroutine race faster.
	pollInterval time.Duration
	// StdinForTest, when non-nil, replaces os.Stdin for the foreground
	// prompt read. Letting tests pin a strings.Reader avoids spinning up
	// real ptys in test runs.
	StdinForTest io.Reader
	// isStdinTTYForTest, when non-nil, overrides the os.Stdin.Stat()
	// character-device probe used to pick between the foreground and the
	// external-command paths. Tests set this to a closure that returns
	// true (foreground) or false (detach) to exercise both halves of the
	// executor without juggling actual TTY plumbing.
	isStdinTTYForTest func() bool
}

// Compile-time guarantee that confirmExecutor satisfies StageExecutor.
var _ StageExecutor = (*confirmExecutor)(nil)

// newConfirmExecutor builds a confirmExecutor with the spec-mandated 500ms
// poll interval. Tests construct one directly so they can shorten the
// polling cadence.
func newConfirmExecutor() *confirmExecutor {
	return &confirmExecutor{pollInterval: 500 * time.Millisecond}
}

// errStageRejected is the sentinel a confirm stage returns when the
// operator rejected the prompt (or the timeout fired with no response).
// The wave loop's markStageStatus path treats it the same way as any
// other stage error — downstream "success" stages skip, "failure"/"always"
// stages fire — but the marker error type lets external callers
// (`errors.Is`) distinguish a user-initiated rejection from a real bug.
var errStageRejected = errors.New("confirm stage rejected")

// IsConfirmRejection reports whether err originated from a confirm stage
// rejecting (operator said no or timed out). Exported so the CLI layer
// can render a friendlier message than the raw aggregated pipeline error.
func IsConfirmRejection(err error) bool { return errors.Is(err, errStageRejected) }

// confirmAcceptedBody / confirmRejectedBody are the canonical strings
// written to <stage-id>.response by `vamp confirm` and back into the
// stage's Output file. They're plain ASCII so an operator can edit the
// marker file with vi if `vamp confirm` is unavailable on the host.
const (
	confirmAcceptedBody = "accepted"
	confirmRejectedBody = "rejected"
)

// PendingFileSuffix / ResponseFileSuffix name the per-stage marker files
// the external-command path writes / polls. Exported so the `vamp
// confirm` command can construct the same paths without a copy-paste of
// the string literal.
const (
	PendingFileSuffix  = ".pending"
	ResponseFileSuffix = ".response"
)

// PendingFilePath returns the absolute path of the marker file the
// confirm executor writes for stageID inside runDir. Exposed so the
// `vamp confirm` command and any future tooling agree on the layout.
func PendingFilePath(runDir, stageID string) string {
	return filepath.Join(runDir, stageID+PendingFileSuffix)
}

// ResponseFilePath returns the absolute path of the response file the
// operator (or `vamp confirm`) writes to clear the gate. Mirror of
// PendingFilePath; same rationale.
func ResponseFilePath(runDir, stageID string) string {
	return filepath.Join(runDir, stageID+ResponseFileSuffix)
}

// Execute renders the message template, picks the foreground vs detach
// path, and blocks until the gate clears or the timeout fires. The
// returned StageOutput carries "accepted" or "rejected" verbatim;
// executeStage writes it to <RunDir>/<rendered output> the same way any
// other text-producing stage's content is persisted.
func (c *confirmExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("confirm: missing stage")
	}

	// Render the message against the standard template binding. Foreach
	// item bindings are honored so a confirm-in-a-foreach can reference
	// the current item (though typically you'd put a confirm OUTSIDE the
	// fan-out).
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}
	prior := in.Prior
	// renderTemplate accepts a nil extra; promote to a fresh map so we
	// can layer the failure/status bindings the rest of the executors
	// expose to confirm messages too.
	if extra == nil {
		extra = map[string]any{}
	}
	extra["pipeline_status"] = in.PipelineStatus
	extra["failure_summary"] = in.FailureSummary
	message, err := renderTemplate(st.ID+":message", st.Message, st.Inputs, in.Inputs, prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render message: %w", st.ID, err)
	}

	tty := c.detectTTY()
	if tty {
		return c.executeForeground(ctx, st, in, message)
	}
	return c.executeExternal(ctx, st, in, message)
}

// detectTTY decides between the foreground (stdin-driven) and the
// external-command (marker-file-driven) paths. The spec mandates the
// stdlib portable form `(os.Stdin.Stat().Mode() & os.ModeCharDevice) != 0`
// — true on a real terminal, false on a redirected stdin (`<`, pipes,
// /dev/null, the Setsid'd detached worker). Tests override this via
// isStdinTTYForTest.
func (c *confirmExecutor) detectTTY() bool {
	if c.isStdinTTYForTest != nil {
		return c.isStdinTTYForTest()
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		// On stat failure assume we're not on a TTY; falling through to
		// the marker-file path is the safer default for a daemonized
		// worker that has redirected stdin away from a terminal.
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// executeForeground prints the rendered prompt to in.Log (the executor's
// log sink — same place runtime status lines land), reads one line from
// stdin, and accepts only on "y" / "Y" / "yes" (whitespace-trimmed,
// lowercased). Anything else rejects.
func (c *confirmExecutor) executeForeground(ctx context.Context, st *Stage, in StageInput, message string) (*StageOutput, error) {
	stdin := io.Reader(os.Stdin)
	if c.StdinForTest != nil {
		stdin = c.StdinForTest
	}
	w := in.Log
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "=== confirm: %s ===\n", st.ID)
	fmt.Fprintln(w, message)
	fmt.Fprint(w, "approve? [y/N] ")

	// Read in a goroutine so a cancellation / timeout can preempt a
	// blocked stdin read. We deliberately don't try to close stdin from
	// here — the parent process owns it — so a canceled run leaks the
	// goroutine until the process exits, which is acceptable because the
	// process is on its way out anyway when ctx is canceled.
	type readResult struct {
		line string
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 0, 64)
		tmp := make([]byte, 1)
		for {
			n, err := stdin.Read(tmp)
			if n == 0 && err != nil {
				resultCh <- readResult{line: string(buf), err: err}
				return
			}
			if n == 0 {
				continue
			}
			if tmp[0] == '\n' {
				resultCh <- readResult{line: string(buf)}
				return
			}
			if tmp[0] != '\r' {
				buf = append(buf, tmp[0])
			}
		}
	}()

	timeoutCh, cancelTimer := c.timeoutChan(st.Timeout)
	defer cancelTimer()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeoutCh:
		fmt.Fprintf(w, "\n(timed out after %s — rejecting)\n", st.Timeout)
		return nil, fmt.Errorf("stage %s: confirm timed out after %s: %w", st.ID, st.Timeout, errStageRejected)
	case res := <-resultCh:
		if res.err != nil && res.err != io.EOF {
			return nil, fmt.Errorf("stage %s: read stdin: %w", st.ID, res.err)
		}
		ans := strings.ToLower(strings.TrimSpace(res.line))
		if ans == "y" || ans == "yes" {
			fmt.Fprintln(w, "accepted")
			return &StageOutput{Text: confirmAcceptedBody}, nil
		}
		fmt.Fprintln(w, "rejected")
		return nil, fmt.Errorf("stage %s: operator rejected: %w", st.ID, errStageRejected)
	}
}

// executeExternal writes the pending marker, polls for the response, and
// returns once one arrives. The marker contains the rendered message body
// followed by a brief usage hint so an operator can `cat` the file and
// see exactly what command to run.
//
// Concurrency: there's only ever one writer to the response file (`vamp
// confirm`), and the executor is the only reader. We tolerate (and
// ignore) a transiently-empty file because some editors stat the path
// before the body is written; readNonEmpty already enforces "size > 0".
func (c *confirmExecutor) executeExternal(ctx context.Context, st *Stage, in StageInput, message string) (*StageOutput, error) {
	pending := PendingFilePath(in.RunDir, st.ID)
	response := ResponseFilePath(in.RunDir, st.ID)
	// Clear any stale response from a prior run; resume semantics for
	// confirm are "always re-ask", so the response file from a previous
	// invocation must not be picked up here.
	_ = os.Remove(response)
	body := fmt.Sprintf("%s\n\nWaiting for `vamp confirm %s %s` (or --reject).\n", message, in.RunDir, st.ID)
	if err := writeFile(pending, body); err != nil {
		return nil, fmt.Errorf("stage %s: write pending marker: %w", st.ID, err)
	}
	// Best-effort cleanup of the marker file on the way out so a
	// successful run doesn't litter pending files into the run dir. We
	// keep the response file in place because it's part of the stage's
	// audit trail (and the actual Output file is the canonical record
	// either way).
	defer func() { _ = os.Remove(pending) }()

	if w := in.Log; w != nil {
		fmt.Fprintf(w, "  -> stage %q: awaiting `vamp confirm %s %s` (or --reject)\n", st.ID, in.RunDir, st.ID)
	}

	interval := c.pollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timeoutCh, cancelTimer := c.timeoutChan(st.Timeout)
	defer cancelTimer()

	for {
		// Read the response on every tick. The file is small (one
		// keyword) so the per-poll cost is negligible. A first-tick
		// check before Select would let us return faster when the
		// response is already in place, but at 500ms the difference is
		// imperceptible and adding it doubles the read path.
		body, ok, err := readNonEmpty(response)
		if err != nil {
			return nil, fmt.Errorf("stage %s: read response: %w", st.ID, err)
		}
		if ok {
			v := strings.ToLower(strings.TrimSpace(string(body)))
			switch v {
			case confirmAcceptedBody:
				return &StageOutput{Text: confirmAcceptedBody}, nil
			case confirmRejectedBody:
				return nil, fmt.Errorf("stage %s: operator rejected: %w", st.ID, errStageRejected)
			default:
				return nil, fmt.Errorf("stage %s: response file %q must contain %q or %q, got %q", st.ID, response, confirmAcceptedBody, confirmRejectedBody, v)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeoutCh:
			return nil, fmt.Errorf("stage %s: confirm timed out after %s: %w", st.ID, st.Timeout, errStageRejected)
		case <-ticker.C:
			// loop and poll again
		}
	}
}

// timeoutChan returns a channel that fires after d (or never, when d
// is zero), plus a cleanup func the caller must defer. The "never"
// case returns a nil channel; selecting on nil blocks forever, which
// is exactly what we want for "no timeout configured".
func (c *confirmExecutor) timeoutChan(d time.Duration) (<-chan time.Time, func()) {
	if d <= 0 {
		return nil, func() {}
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}
