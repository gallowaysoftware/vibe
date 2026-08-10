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
// the payload has no error field — e.g. an ordinary completion chunk — or
// when the field carries no text.
//
// The empty-string case is ok=false rather than ("", true) because this
// helper has two callers and only one of them guarded it.
// classifyHTTPFailure checks `msg != ""` before building its detail;
// readWarmStream hands the message straight to classifyFailureMessage, so
// `{"error":""}` mid-stream produced `router: START_FAILED: model "qwen3"`
// — and WaitForWarm treats START_FAILED as authoritative, so the whole
// capability died with an error containing no reason at all. Reporting "no
// message here" instead lets the HTTP path fall back to the raw body and the
// stream path keep reading.
func routerFailureMessage(raw []byte) (string, bool) {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Error) == 0 || string(envelope.Error) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(envelope.Error, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "", false
		}
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

// capacityPhrases are the substrings that make a failure message an
// allocation failure — the model or request does not fit RIGHT NOW, so
// WaitForWarm should wait 3s and ask again.
//
// The list is a family rather than the six exact spellings it started as.
// `"failed to allocate"` alone missed "unable to allocate CUDA0 buffer",
// "cannot allocate memory" and every other verb, and the miss is expensive
// in one direction only: an allocation failure that falls through to
// START_FAILED is treated by WaitForWarm as an AUTHORITATIVE verdict and
// permanently fails the capability, so a transient VRAM squeeze — another
// cell's model still resident, the normal state of a two-GPU fleet — kills
// a run that would have succeeded three seconds later.
//
// `exit status 137` and `signal: killed` are here for the same reason:
// 128+SIGKILL is the Linux OOM-killer's signature, and it is not
// engine-specific.
var capacityPhrases = []string{
	"out of memory", "outofmemory", "insufficient memory", "not enough memory",
	"insufficient vram", "allocate", "allocation failed",
	"exit status 137", "signal: killed",
}

// notFoundPhrases are the substrings that mean the model id is not in the
// catalog. Asking again will not grow it, so WaitForWarm fails fast.
var notFoundPhrases = []string{
	"model not found", "unknown model", "no such model", "could not find model",
	"model_not_found",
}

// classifyFailureMessage maps a router/engine failure message to a typed
// error. Message-text matching is the only signal available for the two
// cases HTTP status can't discriminate: an in-stream failure after a 200
// (START_FAILED vs CAPACITY) and engines that report unknown models with a
// 400 instead of a 404.
//
// The OOM arm is tested first, which is why its tokens must be exact. `"oom"`
// used to be matched as a bare substring, and it is a substring of bloom,
// bloomz, zoom, doom, room and bedroom — real GGUF model families and
// ordinary path segments. A 404 saying `model not found: bloomz-7b1` was
// therefore CAPACITY (retryable) while the same 404 for `qwen3-30b` was
// NOT_FOUND (fail fast): the operator who typo'd a catalog entry hammered a
// 404 every 3s for the full 10-minute warm budget and was then told the model
// did not fit in VRAM. `oom` is now matched only as a whole token, so
// "cuda oom" and "oom-kill" still hit and "bloomz-7b1" does not.
func classifyFailureMessage(model, msg string) *RouterError {
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower, capacityPhrases...) || containsToken(lower, "oom"):
		return &RouterError{Code: RouterCapacity, Model: model, Detail: msg}
	case containsAny(lower, notFoundPhrases...):
		return &RouterError{Code: RouterNotFound, Model: model, Detail: msg}
	default:
		return &RouterError{Code: RouterStartFailed, Model: model, Detail: msg}
	}
}

// containsToken reports whether tok appears in s bounded by non-alphanumeric
// characters on both sides — the word-boundary test strings.Contains is not.
// s and tok are both expected lowercase.
func containsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if s[i:i+len(tok)] != tok {
			continue
		}
		if i > 0 && isTokenByte(s[i-1]) {
			continue
		}
		if j := i + len(tok); j < len(s) && isTokenByte(s[j]) {
			continue
		}
		return true
	}
	return false
}

// isTokenByte reports whether c continues a word. Underscore counts: a model
// id like "bloom_7b" is one token, not two.
func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// maxRouterDetail bounds the far-side text this package will carry in an
// error string. The 8192-byte io.LimitReader that used to be the only bound
// lives in WarmModel — the CALLER — so the classifier was a guard in one of
// N call paths with N=1, and any future caller that forgets the LimitReader
// (a cloud_api backend on the warm path, which the fleet design plans) hands
// this an unbounded body that goes verbatim into a run log. A 7KB HTML proxy
// page is not a diagnostic; the first 512 bytes of it are.
const maxRouterDetail = 512

// classifyHTTPFailure maps a non-2xx router response to a typed error.
func classifyHTTPFailure(model string, status int, body []byte) *RouterError {
	msg, ok := routerFailureMessage(body)
	if !ok {
		msg = strings.TrimSpace(string(body))
	}
	msg = boundDetail(msg)
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

// boundDetail clips far-side text to maxRouterDetail bytes on a rune
// boundary, marking the cut so the reader knows there was more.
func boundDetail(s string) string {
	if len(s) <= maxRouterDetail {
		return s
	}
	cut := maxRouterDetail
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "… [truncated]"
}

// isConnectFailure reports whether err means nothing took the request at the
// connection level. EPIPE, ECONNABORTED and ETIMEDOUT are here alongside the
// dial-time errnos because vamp/errors.go publishes
// errors.Is(err, ErrUpstreamDown) as the documented way for an external
// caller to ask "is anything listening on the router port" — and a router
// killed mid-request answers with OpError{Op:"write", Err: EPIPE}, which the
// dial-only Op check never saw.
func isConnectFailure(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ETIMEDOUT) {
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
