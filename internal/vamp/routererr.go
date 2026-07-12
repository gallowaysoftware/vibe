package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
)

// RouterErrorCode partitions model-router failures (llama-swap or vibe's own
// proxy on :9000) into the four classes a caller can act on differently:
// retry-later, fix-the-name, shed-load, or check-the-host.
type RouterErrorCode string

const (
	// RouterStartFailed: the router accepted the request but the model
	// failed to come up — llama-swap commits to a 200 + SSE before the
	// upstream is healthy, so start failures arrive as an in-stream error
	// payload (or a 5xx with start-failure text on non-streaming paths).
	RouterStartFailed RouterErrorCode = "START_FAILED"
	// RouterNotFound: the model id is not in the router's catalog (404, or
	// an "unknown model" error body).
	RouterNotFound RouterErrorCode = "NOT_FOUND"
	// RouterCapacity: concurrency shedding (429) or an OOM-flavored
	// router/upstream failure — the model or request doesn't fit right now.
	RouterCapacity RouterErrorCode = "CAPACITY"
	// RouterUpstreamDown: nothing answered — connection refused, host
	// unreachable, DNS failure. Distinct from START_FAILED: the router
	// itself never took the request.
	RouterUpstreamDown RouterErrorCode = "UPSTREAM_DOWN"
)

// RouterError is the typed error for router-plane failures. errors.Is
// matches on Code (compare against the Err* sentinels), errors.As extracts
// the full value for Model/Detail.
type RouterError struct {
	Code   RouterErrorCode
	Model  string // model id the request named; "" when unknown
	Detail string // upstream-provided failure text, verbatim
	cause  error
}

// Sentinels for errors.Is checks against each code.
var (
	ErrStartFailed  = &RouterError{Code: RouterStartFailed}
	ErrNotFound     = &RouterError{Code: RouterNotFound}
	ErrCapacity     = &RouterError{Code: RouterCapacity}
	ErrUpstreamDown = &RouterError{Code: RouterUpstreamDown}
)

func (e *RouterError) Error() string {
	var b strings.Builder
	b.WriteString("router: ")
	b.WriteString(string(e.Code))
	if e.Model != "" {
		fmt.Fprintf(&b, ": model %q", e.Model)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Detail == "" && e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

func (e *RouterError) Unwrap() error { return e.cause }

// Is matches any *RouterError with the same Code, so
// errors.Is(err, ErrStartFailed) works without the caller holding the exact
// instance.
func (e *RouterError) Is(target error) bool {
	t, ok := target.(*RouterError)
	return ok && t.Code == e.Code
}

// routerFailureMessage extracts the failure text from a router/engine error
// body. Two shapes exist in the wild: llama-swap's
// {"error":"<string>","src":"llama-swap"} (also its final in-stream SSE data
// line) and the OpenAI-style {"error":{"message":...}}. Returns ok=false when
// the payload has no error field — e.g. an ordinary completion chunk.
func routerFailureMessage(raw []byte) (string, bool) {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Error) == 0 || string(envelope.Error) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(envelope.Error, &s); err == nil {
		return s, true
	}
	var obj struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(envelope.Error, &obj); err == nil && (obj.Message != "" || obj.Type != "") {
		if obj.Message != "" {
			return obj.Message, true
		}
		return obj.Type, true
	}
	return strings.TrimSpace(string(envelope.Error)), true
}

// classifyFailureMessage maps a router/engine failure message to a typed
// error. Message-text matching is the only signal available for the two
// cases HTTP status can't discriminate: an in-stream failure after a 200
// (START_FAILED vs CAPACITY) and engines that report unknown models with a
// 400 instead of a 404.
func classifyFailureMessage(model, msg string) *RouterError {
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower,
		"out of memory", "oom", "insufficient memory", "not enough memory",
		"insufficient vram", "failed to allocate"):
		return &RouterError{Code: RouterCapacity, Model: model, Detail: msg}
	case containsAny(lower,
		"model not found", "unknown model", "no such model", "could not find model",
		"model_not_found"):
		return &RouterError{Code: RouterNotFound, Model: model, Detail: msg}
	default:
		return &RouterError{Code: RouterStartFailed, Model: model, Detail: msg}
	}
}

// classifyHTTPFailure maps a non-2xx router response to a typed error.
func classifyHTTPFailure(model string, status int, body []byte) *RouterError {
	msg, ok := routerFailureMessage(body)
	if !ok {
		msg = strings.TrimSpace(string(body))
	}
	detail := fmt.Sprintf("status %d", status)
	if msg != "" {
		detail = fmt.Sprintf("status %d: %s", status, msg)
	}
	// Body text is more specific than the status code: an OOM behind any
	// status is CAPACITY, an "unknown model" behind a 400 is NOT_FOUND.
	if re := classifyFailureMessage(model, msg); re.Code != RouterStartFailed {
		re.Detail = detail
		return re
	}
	switch status {
	case 404:
		return &RouterError{Code: RouterNotFound, Model: model, Detail: detail}
	case 429:
		return &RouterError{Code: RouterCapacity, Model: model, Detail: detail}
	case 502, 503, 504:
		// Gateway statuses mean "nothing serving behind the proxy right
		// now" — what vibe's own proxy returns while a backend is still
		// loading — so they must classify as retryable unavailability, not
		// as an authoritative failed start.
		return &RouterError{Code: RouterUpstreamDown, Model: model, Detail: detail}
	default:
		return &RouterError{Code: RouterStartFailed, Model: model, Detail: detail}
	}
}

// classifyTransportError wraps connection-level failures as UPSTREAM_DOWN.
// Context cancellation/expiry passes through untyped so retry loops and
// callers can keep telling "we gave up waiting" apart from "the host is
// down".
func classifyTransportError(model string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if isConnectFailure(err) {
		return &RouterError{Code: RouterUpstreamDown, Model: model, cause: err}
	}
	return err
}

func isConnectFailure(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
