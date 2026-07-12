package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sseChunk writes one OpenAI-style SSE data line and flushes.
func sseChunk(w http.ResponseWriter, payload string) {
	fmt.Fprintf(w, "data: %s\n\n", payload)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func startSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// TestWarmModel_ErrorMapping is the typed-error mapping table: a fake router
// emits each of llama-swap's observed failure shapes and the classifier must
// land on the right RouterErrorCode, matchable via errors.Is and errors.As.
func TestWarmModel_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		wantCode   RouterErrorCode
		wantDetail string // substring of RouterError.Detail; "" skips the check
	}{
		{
			// llama-swap's in-stream failure: 200 + loading chunks, then the
			// error payload as the final SSE data line.
			name: "in-stream start failure after 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				startSSE(w)
				sseChunk(w, `{"choices":[{"delta":{"reasoning_content":"loading model..."}}]}`)
				sseChunk(w, `{"error":"unspecific error: upstream command exited prematurely","src":"llama-swap"}`)
			},
			wantCode:   RouterStartFailed,
			wantDetail: "exited prematurely",
		},
		{
			// Same failure surfaced as a plain JSON body (non-streaming path).
			name: "start failure as JSON body after 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintln(w, `{"error":"unspecific error: upstream command exited prematurely","src":"llama-swap"}`)
			},
			wantCode:   RouterStartFailed,
			wantDetail: "exited prematurely",
		},
		{
			name: "404 model not in catalog",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintln(w, `{"error":{"message":"model not found","type":"not_found_error"}}`)
			},
			wantCode: RouterNotFound,
		},
		{
			// Engines that report unknown models with a 400: the body text
			// wins over the status.
			name: "400 unknown model",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintln(w, `{"error":{"message":"could not find model \"nope\""}}`)
			},
			wantCode: RouterNotFound,
		},
		{
			name: "429 concurrency shed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintln(w, `{"error":{"message":"too many requests"}}`)
			},
			wantCode: RouterCapacity,
		},
		{
			name: "500 with OOM-flavored body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintln(w, `{"error":"CUDA out of memory: failed to allocate 4.2GiB"}`)
			},
			wantCode:   RouterCapacity,
			wantDetail: "out of memory",
		},
		{
			name: "in-stream OOM after 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				startSSE(w)
				sseChunk(w, `{"error":"upstream error: CUDA out of memory","src":"llama-swap"}`)
			},
			wantCode: RouterCapacity,
		},
		{
			name: "generic 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintln(w, "internal error")
			},
			wantCode: RouterStartFailed,
		},
		{
			// Gateway statuses = "backend not serving yet" (vibe's own proxy
			// during a load, llama-server's 503 while loading) — must stay
			// retryable unavailability, not an authoritative failed start.
			name: "503 while backend loads",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintln(w, `{"error":{"message":"Loading model"}}`)
			},
			wantCode: RouterUpstreamDown,
		},
		{
			name: "stream dies mid-load without error payload",
			handler: func(w http.ResponseWriter, r *http.Request) {
				startSSE(w)
				sseChunk(w, `{"choices":[{"delta":{"reasoning_content":"loading..."}}]}`)
				// Handler returns: clean EOF, no completion, no error line.
			},
			wantCode: RouterStartFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			err := WarmModel(context.Background(), srv.Client(), srv.URL, "m")
			if err == nil {
				t.Fatal("expected error")
			}
			var re *RouterError
			if !errors.As(err, &re) {
				t.Fatalf("errors.As(*RouterError) failed for %v", err)
			}
			if re.Code != tc.wantCode {
				t.Errorf("code = %s, want %s (err: %v)", re.Code, tc.wantCode, err)
			}
			if !errors.Is(err, &RouterError{Code: tc.wantCode}) {
				t.Errorf("errors.Is against code sentinel failed for %v", err)
			}
			if re.Model != "m" {
				t.Errorf("model = %q, want %q", re.Model, "m")
			}
			if tc.wantDetail != "" && !strings.Contains(re.Detail, tc.wantDetail) {
				t.Errorf("detail %q missing %q", re.Detail, tc.wantDetail)
			}
		})
	}
}

func TestWarmModel_UpstreamDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listening: dial must refuse
	err := WarmModel(context.Background(), nil, url, "m")
	if !errors.Is(err, ErrUpstreamDown) {
		t.Fatalf("err = %v, want UPSTREAM_DOWN", err)
	}
}

// TestWarmModel_StreamingWarm verifies the probe asks for a streamed
// single token and tolerates llama-swap's loading-state chunks
// (delta.reasoning_content) before the real content arrives.
func TestWarmModel_StreamingWarm(t *testing.T) {
	var gotStream atomic.Bool
	var gotMaxTokens atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream    bool   `json:"stream"`
			MaxTokens int64  `json:"max_tokens"`
			Model     string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		gotStream.Store(body.Stream)
		gotMaxTokens.Store(body.MaxTokens)
		startSSE(w)
		sseChunk(w, `{"choices":[{"delta":{"role":"assistant"}}]}`)
		sseChunk(w, `{"choices":[{"delta":{"reasoning_content":"still loading model qwen..."}}]}`)
		sseChunk(w, `{"choices":[{"delta":{"reasoning_content":"almost there..."}}]}`)
		sseChunk(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
		sseChunk(w, `{"choices":[{"delta":{},"finish_reason":"length"}]}`)
		sseChunk(w, "[DONE]")
	}))
	defer srv.Close()
	if err := WarmModel(context.Background(), srv.Client(), srv.URL, "m"); err != nil {
		t.Fatalf("WarmModel: %v", err)
	}
	if !gotStream.Load() {
		t.Error("probe did not request stream: true")
	}
	if gotMaxTokens.Load() != 1 {
		t.Errorf("probe max_tokens = %d, want 1", gotMaxTokens.Load())
	}
}

// TestWarmModel_FinishReasonWithoutContent covers thinking models whose
// single budgeted token is reasoning: the stream may end via finish_reason
// with no content delta and must still count as warm.
func TestWarmModel_FinishReasonWithoutContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startSSE(w)
		sseChunk(w, `{"choices":[{"delta":{"reasoning_content":"hm"}}]}`)
		sseChunk(w, `{"choices":[{"delta":{},"finish_reason":"length"}]}`)
		// No [DONE]: EOF after a completed choice is still success.
	}))
	defer srv.Close()
	if err := WarmModel(context.Background(), srv.Client(), srv.URL, "m"); err != nil {
		t.Fatalf("WarmModel: %v", err)
	}
}

func TestWaitForWarm_NotFoundAborts(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(w, `{"error":{"message":"model not found"}}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := WaitForWarm(ctx, srv.Client(), srv.URL, "m", time.Second, 10*time.Millisecond)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (NOT_FOUND must not be retried)", calls.Load())
	}
}

func TestWaitForWarm_StartFailedAborts(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		startSSE(w)
		sseChunk(w, `{"error":"unspecific error: upstream command exited prematurely","src":"llama-swap"}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := WaitForWarm(ctx, srv.Client(), srv.URL, "m", time.Second, 10*time.Millisecond)
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("err = %v, want START_FAILED", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (START_FAILED must not be retried)", calls.Load())
	}
}

// TestWaitForWarm_RetriesTransientThenSucceeds: a 503 (backend still
// loading) is retryable; the second attempt streams a completion.
func TestWaitForWarm_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"error":{"message":"Loading model"}}`)
			return
		}
		startSSE(w)
		sseChunk(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
		sseChunk(w, "[DONE]")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := WaitForWarm(ctx, srv.Client(), srv.URL, "m", time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForWarm: %v", err)
	}
	if calls.Load() < 2 {
		t.Errorf("calls = %d, want >= 2", calls.Load())
	}
}
