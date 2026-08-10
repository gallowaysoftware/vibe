package vamp

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
)

// TestClassifyFailureMessage_OOMIsMatchedAsAWord pins the false-POSITIVE
// half of the classifier's one axis.
//
// "oom" was matched as a bare substring, and it is a substring of bloom,
// bloomz, zoom, doom, room and bedroom — BigScience's BLOOM/bloomz are real
// widely-mirrored GGUF families, and the rest are ordinary path segments.
// The OOM arm is tested BEFORE the NOT_FOUND arm, so it won: a 404 saying
// `model not found: bloomz-7b1` classified as CAPACITY, which WaitForWarm
// treats as retryable, so the operator who typo'd one catalog entry hammered
// a 404 every 3s for the whole 10-minute warm budget and was then told the
// model did not fit in VRAM. The same 404 for qwen3-30b failed in
// milliseconds.
func TestClassifyFailureMessage_OOMIsMatchedAsAWord(t *testing.T) {
	notCapacity := map[string]RouterErrorCode{
		"model not found: bloomz-7b1":                                         RouterNotFound,
		"unknown model: bloom-560m":                                           RouterNotFound,
		"could not find model /srv/models/doom-writer-q4.gguf":                RouterNotFound,
		"failed to start /srv/models/bedroom-coder/model.gguf: exit status 1": RouterStartFailed,
		"llama-server: error while loading shared libraries in /opt/zoom/lib": RouterStartFailed,
		"upload to /srv/mushroom/models timed out":                            RouterStartFailed,
	}
	for msg, want := range notCapacity {
		if got := classifyFailureMessage("m", msg).Code; got != want {
			t.Errorf("%q\n got:  %s\n want: %s", msg, got, want)
		}
	}
	// The token itself still classifies, in every spelling that is really
	// a word — dropping "oom" outright would have been the other way to be
	// wrong.
	stillCapacity := []string{
		"CUDA OOM while loading weights",
		"oom-kill triggered by cgroup",
		"process hit an OOM",
		"oom",
		"(oom)",
	}
	for _, msg := range stillCapacity {
		if got := classifyFailureMessage("m", msg).Code; got != RouterCapacity {
			t.Errorf("%q got %s, want CAPACITY", msg, got)
		}
	}
}

// TestClassifyFailureMessage_AllocationFailuresAreRetryable pins the
// false-NEGATIVE half — the same three lines, the opposite direction, and
// the more expensive one.
//
// The list required the exact phrase "failed to allocate", so every other
// spelling fell through to START_FAILED, which WaitForWarm treats as an
// AUTHORITATIVE verdict and does not retry. A transient VRAM squeeze —
// another cell's model still resident, the normal state of a two-GPU fleet —
// therefore permanently failed the capability instead of succeeding three
// seconds later.
//
// Whether llama.cpp on any given box emits these exact strings is inference,
// not measurement; `exit status 137` is not engine-specific.
func TestClassifyFailureMessage_AllocationFailuresAreRetryable(t *testing.T) {
	for _, msg := range []string{
		"llama_model_load: error loading model: unable to allocate CUDA0 buffer",
		"ggml_backend_cuda_buffer_type_alloc_buffer: cannot allocate 8192.00 MiB",
		"failed to allocate compute buffers",
		"cudaMalloc failed: allocation failed",
		"exit status 137",
		"process killed (signal: killed)",
		"torch.cuda.OutOfMemoryError: CUDA out of memory",
		"OutOfMemoryError",
	} {
		re := classifyFailureMessage("m", msg)
		if re.Code != RouterCapacity {
			t.Errorf("%q got %s, want CAPACITY (START_FAILED is non-retryable in WaitForWarm)", msg, re.Code)
		}
		// The retryability policy this is aimed at lives in WaitForWarm:
		// NOT_FOUND and START_FAILED fail fast, everything else retries.
		if errors.Is(re, ErrStartFailed) || errors.Is(re, ErrNotFound) {
			t.Errorf("%q lands in a non-retryable bucket", msg)
		}
	}
}

// TestClassifyHTTPFailure_NameErrorAndCapacityDoNotSwap is the end-to-end
// shape of the finding: two 404s that differed only in the model NAME took
// opposite paths through the warm loop.
func TestClassifyHTTPFailure_NameErrorAndCapacityDoNotSwap(t *testing.T) {
	for _, model := range []string{"bloomz-7b1", "qwen3-30b", "zoom-tts", "doom-7b"} {
		body := fmt.Sprintf(`{"error":"model not found: %s"}`, model)
		re := classifyHTTPFailure(model, 404, []byte(body))
		if re.Code != RouterNotFound {
			t.Errorf("404 for %q got %s, want NOT_FOUND", model, re.Code)
		}
	}
	// A 404 whose body really is an OOM is still CAPACITY — the body is
	// more specific than the status, which is the arm's whole point.
	if got := classifyHTTPFailure("m", 404, []byte(`{"error":"CUDA out of memory"}`)).Code; got != RouterCapacity {
		t.Errorf("got %s, want CAPACITY", got)
	}
}

// TestClassifyHTTPFailure_DetailIsBounded pins a cap that lived only in the
// CALLER. WarmModel wraps the body in an 8192-byte io.LimitReader; the
// classifier had no bound of its own, so it was a guard in one of N call
// paths with N=1, and the next caller — a cloud_api backend on the warm
// path, which the fleet design plans — hands it an unbounded body that goes
// verbatim into the run log. A 7KB HTML proxy page is not a diagnostic.
func TestClassifyHTTPFailure_DetailIsBounded(t *testing.T) {
	body := []byte(strings.Repeat("<html>padding</html>", 4000))
	re := classifyHTTPFailure("m", 502, body)
	if len(re.Error()) > 4096 {
		t.Errorf("Error() is %d bytes for a %d-byte body; the classifier carries no bound of its own", len(re.Error()), len(body))
	}
	if !strings.Contains(re.Error(), "truncated") {
		t.Errorf("the cut is not marked, so the reader cannot tell there was more: %q", re.Error())
	}
	// A short body still arrives whole — the bound must not cost the
	// ordinary diagnostic.
	re = classifyHTTPFailure("m", 500, []byte(`{"error":"model failed to start: no such file"}`))
	if !strings.Contains(re.Error(), "no such file") {
		t.Errorf("a short body was clipped: %q", re.Error())
	}
}

// TestRouterFailureMessage_AnEmptyErrorFieldIsNotAMessage pins the shape
// that produced a failure with no reason in it. routerFailureMessage has two
// callers and only classifyHTTPFailure guarded the empty case; readWarmStream
// handed the message straight through, so `{"error":""}` mid-stream became
// `router: START_FAILED: model "qwen3"` — and START_FAILED is
// non-retryable, so the whole capability died with an error string
// containing nothing to act on.
func TestRouterFailureMessage_AnEmptyErrorFieldIsNotAMessage(t *testing.T) {
	for _, body := range []string{`{"error":""}`, `{"error":"   "}`, `{"error":null}`, `{"id":"chunk","choices":[]}`} {
		if msg, ok := routerFailureMessage([]byte(body)); ok {
			t.Errorf("%s reported a failure message %q; an error field with no text is not a reason", body, msg)
		}
	}
	// Shapes that DO carry text keep working, including the contentless-but-
	// present ones, which at least say something.
	for body, want := range map[string]string{
		`{"error":"boom","src":"llama-swap"}`:    "boom",
		`{"error":{"message":"no such model"}}`:  "no such model",
		`{"error":{"type":"invalid_request"}}`:   "invalid_request",
		`{"error":{"code":404,"detail":"gone"}}`: `{"code":404,"detail":"gone"}`,
		`{"error":0}`:                            "0",
	} {
		msg, ok := routerFailureMessage([]byte(body))
		if !ok || msg != want {
			t.Errorf("%s -> (%q, %v), want (%q, true)", body, msg, ok, want)
		}
	}
}

// TestIsConnectFailure_ConnectionLevelErrnos pins the errnos that answer
// "nothing is listening on the router port". vamp/errors.go publishes
// errors.Is(err, ErrUpstreamDown) as the documented way for an external
// caller to ask that, and a router killed mid-request answers with
// OpError{Op:"write", Err: EPIPE} — which the dial-only Op check never saw.
func TestIsConnectFailure_ConnectionLevelErrnos(t *testing.T) {
	down := map[string]error{
		"ECONNREFUSED":  syscall.ECONNREFUSED,
		"ECONNRESET":    syscall.ECONNRESET,
		"EHOSTUNREACH":  syscall.EHOSTUNREACH,
		"ENETUNREACH":   syscall.ENETUNREACH,
		"EPIPE":         syscall.EPIPE,
		"ECONNABORTED":  syscall.ECONNABORTED,
		"ETIMEDOUT":     syscall.ETIMEDOUT,
		"write + EPIPE": &net.OpError{Op: "write", Err: syscall.EPIPE},
		"dial":          &net.OpError{Op: "dial", Err: errors.New("whatever")},
		"DNS":           &net.DNSError{Err: "no such host", Name: "nope.invalid"},
	}
	for name, err := range down {
		t.Run(name, func(t *testing.T) {
			if !isConnectFailure(err) {
				t.Errorf("%v is not typed as a connect failure", err)
			}
			got := classifyTransportError("m", err)
			if !errors.Is(got, ErrUpstreamDown) {
				t.Errorf("classifyTransportError(%v) = %v, want ErrUpstreamDown", err, got)
			}
		})
	}
	// Not every error is the host being down; an ordinary failure must
	// stay untyped so the caller can tell the two apart.
	if isConnectFailure(errors.New("unexpected EOF")) {
		t.Error("a plain error was typed as a connect failure")
	}
}

// TestRouterError_Error covers the string every one of these findings
// eventually reaches the operator through — 0.0% statement coverage before
// this. The cause is only appended when there is no Detail, so a classified
// failure does not print its reason twice.
func TestRouterError_Error(t *testing.T) {
	cases := []struct {
		name string
		err  *RouterError
		want string
	}{
		{"code only", &RouterError{Code: RouterCapacity}, "router: CAPACITY"},
		{"code + model", &RouterError{Code: RouterNotFound, Model: "qwen3"}, `router: NOT_FOUND: model "qwen3"`},
		{"code + model + detail", &RouterError{Code: RouterStartFailed, Model: "q", Detail: "status 500"}, `router: START_FAILED: model "q": status 500`},
		{"cause stands in for an absent detail", &RouterError{Code: RouterUpstreamDown, Model: "q", cause: errors.New("connection refused")}, `router: UPSTREAM_DOWN: model "q": connection refused`},
		{"detail wins over cause", &RouterError{Code: RouterUpstreamDown, Detail: "status 502", cause: errors.New("connection refused")}, "router: UPSTREAM_DOWN: status 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("\n got:  %q\n want: %q", got, tc.want)
			}
		})
	}
	// Unwrap keeps the cause reachable for errors.Is on the transport error
	// underneath the classification.
	wrapped := &RouterError{Code: RouterUpstreamDown, cause: syscall.ECONNREFUSED}
	if !errors.Is(wrapped, syscall.ECONNREFUSED) {
		t.Error("the transport cause is not reachable through Unwrap")
	}
}
