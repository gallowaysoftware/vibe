package vamp

import (
	"fmt"
	"io"
	"strings"
)

// writeFailureSummary renders an end-of-run failure block when the
// pipeline returned an error. Unlike the timing table (which always
// runs), the summary fires only on failure and aims to compress
// "what went wrong + what to try" into a few scannable lines.
//
// Error-pattern matching is deliberately shallow — substrings rather
// than typed-error trees — because the errors arrive from very
// different layers (HTTP clients, file I/O, validation, model
// servers) and we want a useful summary even for errors the layer
// boundary didn't pre-classify. False positives are OK: the hint is
// always presented as a SUGGESTION the user can ignore.
//
// Format:
//
//	FAILED — pipeline "NAME" returned <N> stage error(s)
//	  stage:  <first-failed-stage-id>
//	  reason: <first 200 chars of err, joined newlines collapsed>
//	  hint:   <pattern-matched suggestion>
//	  ...
//	  more in pipeline_timing.json (status="error" entries)
func writeFailureSummary(w io.Writer, pipelineName string, runErr error, e *Executor) {
	if w == nil || runErr == nil {
		return
	}
	// errors.Join produces a multi-line error; split into per-stage
	// chunks so we can report the count + lead with the first.
	chains := splitJoinedError(runErr)
	if len(chains) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "FAILED — pipeline %q returned %d stage error(s)\n", pipelineName, len(chains))
	for i, msg := range chains {
		stageID, reason := parseStageError(msg)
		fmt.Fprintf(w, "  stage:  %s\n", stageID)
		fmt.Fprintf(w, "  reason: %s\n", truncateForSummary(reason, 200))
		if hint := failureHint(stageID, reason, e); hint != "" {
			fmt.Fprintf(w, "  hint:   %s\n", hint)
		}
		// Only show the first failure in detail; if more, count and
		// point to the timing JSON for the rest.
		if i == 0 && len(chains) > 1 {
			fmt.Fprintf(w, "  ... +%d more stage error(s); full details in pipeline_timing.json\n",
				len(chains)-1)
			break
		}
	}
}

// splitJoinedError unwraps an errors.Join chain into one string per
// joined error. Falls back to a single-element slice for non-joined
// errors. Joined errors stringify with newline separators; we use
// the structural Unwrap interface when present to avoid relying on
// the textual format.
func splitJoinedError(err error) []string {
	// errors.Join produces an error whose Unwrap returns []error.
	type unwrappable interface{ Unwrap() []error }
	if u, ok := err.(unwrappable); ok {
		parts := u.Unwrap()
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p != nil {
				out = append(out, p.Error())
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Single error — return as a 1-element slice.
	if msg := err.Error(); msg != "" {
		return []string{msg}
	}
	return nil
}

// parseStageError pulls the stage id + remaining reason out of an
// error string shaped "stage <id>: <reason>" (the format the
// runner's errors.Join wrapping uses; see exec.go line 562).
// Unrecognised shapes return ("?", entireMessage).
func parseStageError(s string) (stageID, reason string) {
	const prefix = "stage "
	if !strings.HasPrefix(s, prefix) {
		return "?", s
	}
	rest := s[len(prefix):]
	colon := strings.Index(rest, ": ")
	if colon < 0 {
		return "?", s
	}
	return rest[:colon], strings.TrimSpace(rest[colon+2:])
}

// truncateForSummary collapses internal newlines + trims very long
// errors so the summary stays scannable. The full error remains in
// the joined runErr the caller already returned (and in
// pipeline_timing.json's status field).
func truncateForSummary(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		return s[:maxLen-1] + "…"
	}
	return s
}

// failureHint returns a one-line suggestion based on the reason
// string. Returns "" when no pattern matches — in that case the
// summary just shows stage + reason and leaves the user to dig.
//
// Add patterns here as new failure modes are observed in real
// pipelines; each is small + cheap + independent.
func failureHint(stageID, reason string, e *Executor) string {
	low := strings.ToLower(reason)
	switch {
	case strings.Contains(low, "connection refused"),
		strings.Contains(low, "no such host"):
		return "Is the backend running? Try `<pipeline> doctor` to see which declared service is unreachable, then `<pipeline> activate` to bring everything up."
	case strings.Contains(low, "context deadline exceeded"),
		strings.Contains(low, "i/o timeout"):
		return "The backend accepted the request but didn't respond in time. Check vibe ps for a hung profile; consider raising the stage's Retry MaxBackoff."
	case strings.Contains(low, "401"), strings.Contains(low, "unauthorized"):
		return "Backend requires auth. Most likely missing a bearer token; check the stage's setup_hint or the model server's API-key config."
	case strings.Contains(low, "429"), strings.Contains(low, "rate limit"):
		return "Backend rate-limited the call. The stage's retry policy should already handle this — confirm it has RetryOn: transient."
	case strings.Contains(low, "invalid_output"), strings.Contains(low, "unmarshal"):
		return "The LLM returned text the stage couldn't parse (often: missing/truncated JSON). Raise max_tokens or tighten the prompt's output contract."
	case strings.Contains(low, "no such file"), strings.Contains(low, "does not exist"):
		return "An expected file is missing. If this is a foreach iteration over outputs from another stage, the upstream stage may have produced nothing."
	case strings.Contains(low, "supervisor: not running"),
		strings.Contains(low, "no profile"):
		return "vibe daemon has no active profile. Run `<pipeline> activate` (auto-registered subcommand) to bring up the LLM + sidecars this pipeline needs."
	case strings.Contains(low, "already running"):
		return "A different profile holds the active slot. Stop it (`vibe stop`) before starting another active-mode profile."
	}
	_ = stageID
	_ = e
	return ""
}
