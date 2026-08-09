package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// recordedWebhookRequest captures one webhook POST for assertions. We snapshot
// method/URL/header/body up front because httptest.Server reuses the same
// *http.Request across handlers if multiple tests run in parallel against
// the same server — copying lets the assertions remain race-free.
type recordedWebhookRequest struct {
	Method      string
	URL         string
	ContentType string
	Header      http.Header
	Body        []byte
}

// webhookTestServer wraps httptest.Server with a thread-safe recorder for
// the most recent request and a programmable response. Tests configure
// status/body/headers per case; the handler echoes them out. We deliberately
// favour httptest.Server over a fully-stubbed httpDoer here because it
// exercises the real net/http client path the executor uses in production,
// which is what we want covered for a stage whose entire purpose is "do an
// HTTP call".
type webhookTestServer struct {
	mu       sync.Mutex
	requests []recordedWebhookRequest
	status   int
	respBody string
	srv      *httptest.Server
}

func newWebhookTestServer(t *testing.T) *webhookTestServer {
	t.Helper()
	w := &webhookTestServer{status: 200, respBody: "ok"}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		hdr := http.Header{}
		for k, v := range r.Header {
			hdr[k] = append([]string(nil), v...)
		}
		w.mu.Lock()
		w.requests = append(w.requests, recordedWebhookRequest{
			Method:      r.Method,
			URL:         r.URL.String(),
			ContentType: r.Header.Get("Content-Type"),
			Header:      hdr,
			Body:        body,
		})
		status, respBody := w.status, w.respBody
		w.mu.Unlock()
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(respBody))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *webhookTestServer) URL() string { return w.srv.URL }

func (w *webhookTestServer) lastRequest() (recordedWebhookRequest, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) == 0 {
		return recordedWebhookRequest{}, false
	}
	return w.requests[len(w.requests)-1], true
}

// TestWebhookExecutor_PostsRenderedBody is the happy-path covering the
// canonical Slack-incoming-webhook shape: POST, application/json,
// rendered body, response captured as stage output. This is the request
// shape every wider test below builds on.
func TestWebhookExecutor_PostsRenderedBody(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.respBody = `{"ok":true}`

	stage := &Stage{
		ID:     "notify",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Inputs: []string{"publish"},
		Body: map[string]any{
			"text": "Pipeline {{ .pipeline_name }} done. Video: {{ .stages.publish.output }}",
		},
		Output: "webhook_response.txt",
	}
	in := StageInput{
		Stage:        stage,
		PipelineName: "test_pipeline",
		Prior: map[string]*stageResult{
			"publish": {Output: "https://youtube.com/watch?v=abc"},
		},
		RunDir: t.TempDir(),
	}

	exec := &webhookExecutor{}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Text != `{"ok":true}` {
		t.Errorf("out.Text = %q, want response body", out.Text)
	}

	req, ok := srv.lastRequest()
	if !ok {
		t.Fatal("no request recorded")
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.ContentType)
	}
	var parsed map[string]string
	if err := json.Unmarshal(req.Body, &parsed); err != nil {
		t.Fatalf("body JSON parse: %v (body=%q)", err, string(req.Body))
	}
	wantText := "Pipeline test_pipeline done. Video: https://youtube.com/watch?v=abc"
	if parsed["text"] != wantText {
		t.Errorf("rendered text = %q, want %q", parsed["text"], wantText)
	}
}

// TestWebhookExecutor_HeadersAndCustomMethod checks that the method override
// (PUT) and templated Authorization header both reach the server. A custom
// Authorization header is the canonical "I need to call a non-Slack webhook
// that requires auth" use case so this is the field most users will exercise
// in real pipelines.
func TestWebhookExecutor_HeadersAndCustomMethod(t *testing.T) {
	srv := newWebhookTestServer(t)
	t.Setenv("WEBHOOK_TOKEN", "secret-token-123")

	stage := &Stage{
		ID:     "notify",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Method: "PUT",
		Body: map[string]any{
			"msg": "hello",
		},
		Headers: map[string]string{
			"Authorization":  `Bearer {{ env "WEBHOOK_TOKEN" }}`,
			"X-Pipeline":     "{{ .pipeline_name }}",
			"X-Custom-Plain": "static-value",
		},
		Output: "resp.txt",
	}
	in := StageInput{
		Stage:        stage,
		PipelineName: "ph",
		RunDir:       t.TempDir(),
	}

	exec := &webhookExecutor{}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req, ok := srv.lastRequest()
	if !ok {
		t.Fatal("no request recorded")
	}
	if req.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", req.Method)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer secret-token-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret-token-123")
	}
	if got := req.Header.Get("X-Pipeline"); got != "ph" {
		t.Errorf("X-Pipeline = %q, want ph", got)
	}
	if got := req.Header.Get("X-Custom-Plain"); got != "static-value" {
		t.Errorf("X-Custom-Plain = %q, want static-value", got)
	}
}

// TestWebhookExecutor_EnvFunc isolates the {{ .env "VAR" }} template
// function. Slack webhook URLs are the documented motivating case — users
// keep them in env vars to avoid committing secrets to the pipeline YAML.
func TestWebhookExecutor_EnvFunc(t *testing.T) {
	srv := newWebhookTestServer(t)
	t.Setenv("VAMP_SLACK_WEBHOOK", srv.URL())

	stage := &Stage{
		ID:   "notify",
		Type: StageTypeWebhook,
		URL:  `{{ env "VAMP_SLACK_WEBHOOK" }}`,
		Body: map[string]any{
			"text": "ping",
		},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}

	exec := &webhookExecutor{}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, ok := srv.lastRequest(); !ok {
		t.Fatal("expected request to env-resolved URL, got none")
	}
}

// TestWebhookExecutor_HTTPError verifies a non-2xx response surfaces a
// descriptive error containing both the status code and a preview of the
// response body. The body preview is what makes a quota / auth failure
// diagnosable in a one-line CI failure message.
func TestWebhookExecutor_HTTPError(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 500
	srv.respBody = "internal server error: queue full"

	stage := &Stage{
		ID:     "notify",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Body:   map[string]any{"text": "ping"},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}

	exec := &webhookExecutor{}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected HTTP 500 to surface an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500, got: %v", err)
	}
	if !strings.Contains(err.Error(), "queue full") {
		t.Errorf("error should include response body preview, got: %v", err)
	}
}

// TestWebhookExecutor_HTTPErrorBodyTruncation verifies that the 1 KiB cap
// kicks in when the upstream returns a huge error body, so we don't dump a
// multi-MB HTML page into a CI log on a misconfigured URL.
func TestWebhookExecutor_HTTPErrorBodyTruncation(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 503
	srv.respBody = strings.Repeat("A", maxWebhookErrorBody+500)

	stage := &Stage{
		ID:     "notify",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Body:   map[string]any{"text": "x"},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}

	exec := &webhookExecutor{}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected 503 error")
	}
	// The error message includes "<status code>: <body>", so length should
	// be capped near maxWebhookErrorBody plus the small prefix.
	if len(err.Error()) > maxWebhookErrorBody+200 {
		t.Errorf("error length %d exceeds expected cap (~%d)", len(err.Error()), maxWebhookErrorBody+200)
	}
}

// TestWebhookExecutor_RejectsBothBodyAndBodyFile verifies validation rejects
// a webhook stage that supplies both body shapes at the same time. This is
// the only realistic operator error in this surface: typoing one but
// leaving the other shouldn't silently send a half-rendered payload.
func TestWebhookExecutor_RejectsBothBodyAndBodyFile(t *testing.T) {
	yaml := `
name: x
stages:
  - id: notify
    type: webhook
    url: https://example.com/hook
    body:
      text: hello
    body_template_file: ./body.json.tmpl
    output: resp.txt
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for both body and body_template_file")
	}
	if !strings.Contains(err.Error(), "exactly one of body or body_template_file") {
		t.Errorf("error should reference both-set conflict, got: %v", err)
	}
}

// TestWebhookExecutor_RejectsNeitherBodyNorBodyFile catches the reverse
// missing-body misconfiguration. A webhook with no body would be a no-op
// notification — usually a user mistake, never the user's intent.
func TestWebhookExecutor_RejectsNeitherBodyNorBodyFile(t *testing.T) {
	yaml := `
name: x
stages:
  - id: notify
    type: webhook
    url: https://example.com/hook
    output: resp.txt
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for missing body")
	}
	if !strings.Contains(err.Error(), "exactly one of body or body_template_file") {
		t.Errorf("error should reference missing-body, got: %v", err)
	}
}

// TestWebhookExecutor_BodyTemplateFile verifies the body_template_file path:
// the executor reads the file relative to PipelineDir, renders it as one
// template against the standard binding, and ships the result verbatim
// (no JSON re-marshalling, so users can craft any payload shape they want).
func TestWebhookExecutor_BodyTemplateFile(t *testing.T) {
	pipelineDir := t.TempDir()
	tmplPath := filepath.Join(pipelineDir, "body.json.tmpl")
	tmplBody := `{"channel":"#vamp","text":"Pipeline {{ .pipeline_name }} finished"}`
	if err := os.WriteFile(tmplPath, []byte(tmplBody), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newWebhookTestServer(t)
	stage := &Stage{
		ID:               "notify",
		Type:             StageTypeWebhook,
		URL:              srv.URL(),
		BodyTemplateFile: "body.json.tmpl",
		Output:           "resp.txt",
	}
	in := StageInput{
		Stage:        stage,
		PipelineName: "demo_pipe",
		PipelineDir:  pipelineDir,
		RunDir:       t.TempDir(),
	}
	exec := &webhookExecutor{}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req, ok := srv.lastRequest()
	if !ok {
		t.Fatal("no request recorded")
	}
	want := `{"channel":"#vamp","text":"Pipeline demo_pipe finished"}`
	if string(req.Body) != want {
		t.Errorf("body = %q, want %q", string(req.Body), want)
	}
	if req.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.ContentType)
	}
}

// TestWebhookExecutor_EmptyURL covers the post-rendering empty-url case:
// the YAML is syntactically valid (url is non-empty) but the template
// resolves to whitespace. We surface this as a clear executor error rather
// than letting net/http return a confusing "unsupported protocol scheme"
// failure.
func TestWebhookExecutor_EmptyURL(t *testing.T) {
	stage := &Stage{
		ID:     "notify",
		Type:   StageTypeWebhook,
		URL:    `{{ env "WEBHOOK_NOT_SET" }}`,
		Body:   map[string]any{"text": "x"},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}
	exec := &webhookExecutor{}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected empty-url error")
	}
	if !strings.Contains(err.Error(), "url is empty") {
		t.Errorf("error should mention empty url, got: %v", err)
	}
}

// TestWebhookExecutor_BodyPreservesNonStringTypes verifies that bools and
// numbers in the inline body map pass through to the JSON wire without
// being stringified. Some webhook receivers (Mattermost block kit, Discord
// embed colors) require numeric / boolean leaves, and rendering them via
// the template would coerce to strings.
func TestWebhookExecutor_BodyPreservesNonStringTypes(t *testing.T) {
	srv := newWebhookTestServer(t)
	stage := &Stage{
		ID:   "notify",
		Type: StageTypeWebhook,
		URL:  srv.URL(),
		Body: map[string]any{
			"text":   "msg",
			"silent": true,
			"color":  int(15158332),
		},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}
	exec := &webhookExecutor{}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req, ok := srv.lastRequest()
	if !ok {
		t.Fatal("no request recorded")
	}
	var parsed map[string]any
	if err := json.Unmarshal(req.Body, &parsed); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if parsed["silent"] != true {
		t.Errorf("silent = %v (%T), want bool true", parsed["silent"], parsed["silent"])
	}
	// JSON numbers come back as float64.
	if got, ok := parsed["color"].(float64); !ok || got != 15158332 {
		t.Errorf("color = %v (%T), want float64 15158332", parsed["color"], parsed["color"])
	}
}

// ─── Assertion path (smoke-test webhook stages) ──────────────────────────────

// TestWebhookExecutor_AssertStatusCode_Match: assert.status_code=200 should
// accept exactly that and return the body, even though we otherwise default
// to "any 2xx".
func TestWebhookExecutor_AssertStatusCode_Match(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 200
	srv.respBody = "ok"
	stage := &Stage{
		ID:     "probe",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Method: "GET",
		Assert: &AssertSpec{StatusCode: 200},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, PipelineName: "p", RunDir: t.TempDir()}
	out, err := (&webhookExecutor{}).Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Text != "ok" {
		t.Errorf("Text = %q", out.Text)
	}
}

// TestWebhookExecutor_AssertStatusCode_AcceptsNon2xx: a status_code: 401
// assertion should treat a 401 response as success — useful for testing
// that auth IS required on an endpoint.
func TestWebhookExecutor_AssertStatusCode_AcceptsNon2xx(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 401
	srv.respBody = `{"detail":"Not authenticated"}`
	stage := &Stage{
		ID:     "probe",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Method: "GET",
		Assert: &AssertSpec{StatusCode: 401},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, PipelineName: "p", RunDir: t.TempDir()}
	if _, err := (&webhookExecutor{}).Execute(context.Background(), in); err != nil {
		t.Fatalf("expected 401 to be accepted with assert.status_code=401, got: %v", err)
	}
}

// TestWebhookExecutor_AssertStatusCode_Mismatch reports both expected and
// actual in the error so the user can debug without re-running.
func TestWebhookExecutor_AssertStatusCode_Mismatch(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 500
	srv.respBody = "kaboom"
	stage := &Stage{
		ID:     "probe",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Method: "GET",
		Assert: &AssertSpec{StatusCode: 200},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, PipelineName: "p", RunDir: t.TempDir()}
	_, err := (&webhookExecutor{}).Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for status mismatch")
	}
	msg := err.Error()
	for _, want := range []string{"assert", "expected HTTP 200", "got HTTP 500", "kaboom"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestWebhookExecutor_AssertBodyChecks exercises every body-check shape in
// one go: body_contains (pass + fail), body_not_contains, min_body_length.
// The interesting property is that ALL failures appear in a single error so
// the user gets the full picture without re-running.
func TestWebhookExecutor_AssertBodyChecks_AllFailures(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 200
	srv.respBody = "short"
	stage := &Stage{
		ID:     "probe",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Method: "GET",
		Assert: &AssertSpec{
			BodyContains:    []string{"missing-token"},
			BodyNotContains: []string{"short"}, // present, should fail
			MinBodyLength:   100,
		},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, PipelineName: "p", RunDir: t.TempDir()}
	_, err := (&webhookExecutor{}).Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected assertion failures")
	}
	msg := err.Error()
	for _, want := range []string{
		`body_contains: missing "missing-token"`,
		`body_not_contains: found "short"`,
		`body length 5 < min_body_length 100`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q in %q", want, msg)
		}
	}
}

// TestWebhookExecutor_AssertBodyChecks_Pass: the happy path with body checks.
func TestWebhookExecutor_AssertBodyChecks_Pass(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = 200
	srv.respBody = `{"results":[{"url":"https://en.wikipedia.org/wiki/Iran","title":"Iran"}]}`
	stage := &Stage{
		ID:     "probe",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Method: "GET",
		Assert: &AssertSpec{
			BodyContains:  []string{`"results"`, "wikipedia.org"},
			MinBodyLength: 20,
		},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, PipelineName: "p", RunDir: t.TempDir()}
	if _, err := (&webhookExecutor{}).Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestWebhookExecutor_GETNoBody confirms the validator allows GET stages
// with no body. The smoke-test pattern probes a URL and asserts on the
// response — having to invent a body just to satisfy the validator was
// the prior friction.
func TestWebhookExecutor_GETNoBody(t *testing.T) {
	yaml := `
name: probe
stages:
  - id: ping
    type: webhook
    url: https://example.com/health
    method: GET
    assert:
      status_code: 200
    output: resp.txt
`
	if _, err := LoadPipeline(writePipeline(t, yaml)); err != nil {
		t.Fatalf("GET with no body should validate, got: %v", err)
	}
}

// TestWebhookExecutor_AssertRejectedOnNonWebhook keeps assert: scoped to
// webhook stages. Putting it on a text stage would silently no-op today;
// surfacing it as a validation error is friendlier.
func TestWebhookExecutor_AssertRejectedOnNonWebhook(t *testing.T) {
	yaml := `
name: x
stages:
  - id: greet
    type: text
    capability: chat
    prompt: hi
    assert:
      status_code: 200
    output: out.txt
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for assert on text stage")
	}
	if !strings.Contains(err.Error(), "assert is only valid on type: webhook stages") {
		t.Errorf("error: %v", err)
	}
}

// TestWebhookExecutor_Returns429AsTransient verifies an HTTP 429 response
// surfaces as a typed transientHTTPError so the retry loop classifies it as
// transient (and not a permanent 4xx). The error string still embeds the
// digits "429" so the legacy substring-based classifier matches even when
// future wrapping strips the typed form.
func TestWebhookExecutor_Returns429AsTransient(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.status = http.StatusTooManyRequests
	srv.respBody = `{"detail":"rate limited"}`

	stage := &Stage{
		ID:     "search",
		Type:   StageTypeWebhook,
		URL:    srv.URL(),
		Body:   map[string]any{"q": "foo"},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}

	exec := &webhookExecutor{}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected 429 to surface an error")
	}
	th := asTransientHTTPError(err)
	if th == nil {
		t.Fatalf("expected *transientHTTPError, got %T: %v", err, err)
	}
	if th.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", th.StatusCode)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error string should contain 429, got: %v", err)
	}
	// And the runtime classifier must agree it's retryable under the
	// "transient" mode (default for webhook stages).
	policy := &RetryPolicy{RetryOn: []string{retryOnTransient}}
	if !isRetryable(err, policy) {
		t.Errorf("isRetryable(429, transient) = false, want true")
	}
}

// TestWebhookExecutor_429ParsesRetryAfterSeconds verifies the integer-seconds
// form of the Retry-After header is parsed into transientHTTPError.RetryAfter
// so the retry loop can honor the server's hint instead of using its own
// exponential backoff.
func TestWebhookExecutor_429ParsesRetryAfterSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	stage := &Stage{
		ID:     "search",
		Type:   StageTypeWebhook,
		URL:    srv.URL,
		Body:   map[string]any{"q": "foo"},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}

	_, err := (&webhookExecutor{}).Execute(context.Background(), in)
	th := asTransientHTTPError(err)
	if th == nil {
		t.Fatalf("expected transientHTTPError, got: %v", err)
	}
	if th.RetryAfter != 12*time.Second {
		t.Errorf("RetryAfter = %s, want 12s", th.RetryAfter)
	}
}

// TestWebhookExecutor_429ParsesRetryAfterHTTPDate verifies the RFC-7231
// HTTP-date form of Retry-After is parsed. We feed in a date a few seconds
// in the future and assert RetryAfter rounds to roughly that gap.
func TestWebhookExecutor_429ParsesRetryAfterHTTPDate(t *testing.T) {
	// Time the response advertises as "come back at". Six seconds ahead of
	// "now" is enough to keep the assertion stable across CI clock jitter.
	const ahead = 6 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		future := time.Now().Add(ahead).UTC().Format(http.TimeFormat)
		w.Header().Set("Retry-After", future)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("come back later"))
	}))
	defer srv.Close()

	stage := &Stage{
		ID:     "search",
		Type:   StageTypeWebhook,
		URL:    srv.URL,
		Body:   map[string]any{"q": "x"},
		Output: "resp.txt",
	}
	in := StageInput{Stage: stage, RunDir: t.TempDir()}

	_, err := (&webhookExecutor{}).Execute(context.Background(), in)
	th := asTransientHTTPError(err)
	if th == nil {
		t.Fatalf("expected transientHTTPError, got: %v", err)
	}
	// HTTP-date has 1-second resolution, so we accept any value within
	// [0, ahead+1s]. The bound rejects "didn't parse at all" (zero) and
	// guards against a parser that double-counts.
	if th.RetryAfter <= 0 || th.RetryAfter > ahead+2*time.Second {
		t.Errorf("RetryAfter = %s, want roughly %s", th.RetryAfter, ahead)
	}
}

// TestParseRetryAfter_Forms unit-tests parseRetryAfter directly with every
// form the spec defines plus a few garbage inputs. Keeps the parser
// independent of the HTTP plumbing — failures here point at the parser
// rather than the executor.
func TestParseRetryAfter_Forms(t *testing.T) {
	// Anchor "now" so HTTP-date math is deterministic.
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		input   string
		want    time.Duration
		wantOK  bool
		approx  bool // when true, allow ±1s slack (HTTP-date precision)
		zeroVal bool
	}{
		{name: "empty", input: "", wantOK: false, zeroVal: true},
		{name: "integer-seconds", input: "30", want: 30 * time.Second, wantOK: true},
		{name: "zero-seconds", input: "0", want: 0, wantOK: true},
		{name: "negative-rejected", input: "-5", wantOK: false, zeroVal: true},
		{name: "http-date-future", input: now.Add(45 * time.Second).Format(http.TimeFormat), want: 45 * time.Second, wantOK: true, approx: true},
		{name: "http-date-past", input: now.Add(-10 * time.Second).Format(http.TimeFormat), wantOK: false, zeroVal: true},
		{name: "garbage", input: "nope", wantOK: false, zeroVal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.input, now)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (got=%s)", ok, tc.wantOK, got)
			}
			if tc.zeroVal && got != 0 {
				t.Errorf("expected zero duration on parse failure, got %s", got)
			}
			if tc.wantOK && !tc.zeroVal {
				diff := got - tc.want
				if diff < 0 {
					diff = -diff
				}
				if tc.approx {
					if diff > time.Second {
						t.Errorf("got = %s, want approximately %s", got, tc.want)
					}
				} else if got != tc.want {
					t.Errorf("got = %s, want %s", got, tc.want)
				}
			}
		})
	}
}

// TestWebhookExecutor_429ThenSuccess_RetriesOnce is the integration-shape
// test the original ask explicitly called for: spin up an httptest.Server
// that returns 429 on the first request and 200 on the second, run the
// stage through the full retry loop, and assert exactly two attempts
// landed at the server. Mirrors the real "rate-limited search API"
// shape that motivated the change.
func TestWebhookExecutor_429ThenSuccess_RetriesOnce(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0") // ask for an immediate retry to keep the test fast
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("burst"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	yaml := fmt.Sprintf(`
name: rl
stages:
  - id: search
    type: webhook
    url: %q
    body:
      q: hello
    output: resp.txt
`, srv.URL)
	p, err := LoadPipeline(writePipeline(t, yaml))
	if err != nil {
		t.Fatalf("LoadPipeline: %v", err)
	}
	// Force tiny backoff so the retry isn't gated on the default 1s sleep.
	p.Stages[0].Retry.InitialBackoff = time.Millisecond
	p.Stages[0].Retry.MaxBackoff = 10 * time.Millisecond

	runDir := t.TempDir()
	ex := &Executor{
		Pipeline:     p,
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{}},
		Vibe:         vibeclient.NewWithHTTPClient("http://127.0.0.1:1", http.DefaultClient, ""),
		RunDir:       runDir,
	}
	if err := ex.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v (expected the second attempt to succeed)", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hits = %d, want 2 (one 429 + one 200)", got)
	}
}

// TestWebhookExecutor_AssertStatusCodeRange validates the schema accepts
// real HTTP codes and rejects garbage.
func TestWebhookExecutor_AssertStatusCodeRange(t *testing.T) {
	yaml := `
name: x
stages:
  - id: probe
    type: webhook
    url: https://example.com/h
    method: GET
    assert:
      status_code: 99
    output: r.txt
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for status_code=99")
	}
	if !strings.Contains(err.Error(), "not a valid HTTP status") {
		t.Errorf("error: %v", err)
	}
}

// secretWebhookPath is the shape every incoming-webhook dialect shares:
// the credential is a PATH segment, so a message that keeps the path has
// leaked the bearer whether or not it kept the host.
const secretWebhookPath = "/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"

// TestWebhookExecutor_LogDoesNotCarryTheURL pins the run log. Under
// --detach that log is a file on disk, so a URL printed here is a
// credential persisted for the life of the run dir. The log must still
// identify the endpoint well enough to debug with — host and a stable
// id — which is why this asserts on what IS there as well as what is not.
func TestWebhookExecutor_LogDoesNotCarryTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	secretURL := srv.URL + secretWebhookPath

	var log strings.Builder
	exec := &webhookExecutor{}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
		Log:    &log,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := log.String()
	if strings.Contains(got, secretURL) {
		t.Errorf("run log contains the full webhook URL:\n%s", got)
	}
	if strings.Contains(got, secretWebhookPath) {
		t.Errorf("run log contains the webhook's secret path segment:\n%s", got)
	}
	// Still useful: the host survives, so an operator can tell which
	// endpoint a stage talked to.
	if !strings.Contains(got, "webhook: POST ") {
		t.Errorf("run log lost the webhook line entirely:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("run log should still name the host it called:\n%s", got)
	}
}

// TestWebhookExecutor_TransportErrorDoesNotCarryTheURL pins the error
// path, which is the worse of the two: a stage error becomes
// Executor.FailureSummary, and {{ .failure_summary }} is what a
// run_when: failure webhook posts into a chat channel. A leak here
// republishes the credential to everyone in the room.
func TestWebhookExecutor_TransportErrorDoesNotCarryTheURL(t *testing.T) {
	// A server closed before the call: Do fails with a *url.Error, whose
	// Error() embeds the full URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()
	secretURL := base + secretWebhookPath

	var log strings.Builder
	exec := &webhookExecutor{}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
		Log:    &log,
	})
	if err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
	if strings.Contains(err.Error(), secretURL) {
		t.Errorf("stage error contains the full webhook URL: %v", err)
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("stage error contains the webhook's secret path segment: %v", err)
	}
	// The error must still say what happened, or scrubbing has traded a
	// leak for an unusable failure.
	if !strings.Contains(err.Error(), "stage notify: request:") {
		t.Errorf("stage error lost its context: %v", err)
	}
	if strings.Contains(log.String(), secretWebhookPath) {
		t.Errorf("run log contains the webhook's secret path segment:\n%s", log.String())
	}
}

// errDoer is an httpDoer that fails the way net/http does: a *url.Error
// wrapping the real cause, with the full request URL in the wrapper.
type errDoer struct{ err error }

func (d errDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

// TestWebhookExecutor_ErrorTextQuotingTheURLIsScrubbed pins the SECOND
// of the two redactions on the error path, which the *url.Error test
// cannot see: unwrapping the wrapper removes the URL only when the URL
// is in the wrapper. A transport that quotes the destination inside its
// own message — a proxy refusing CONNECT, a redirect chain, a custom
// RoundTripper — leaks it past the unwrap. Neither half may be deleted
// on the grounds that the other one covers it.
func TestWebhookExecutor_ErrorTextQuotingTheURLIsScrubbed(t *testing.T) {
	secretURL := "https://hooks.example.invalid" + secretWebhookPath
	exec := &webhookExecutor{doer: errDoer{
		err: fmt.Errorf("proxy refused CONNECT for %s", secretURL),
	}}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the transport error to fail the stage")
	}
	if strings.Contains(err.Error(), secretURL) {
		t.Errorf("stage error contains the full webhook URL: %v", err)
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("stage error contains the webhook's secret path segment: %v", err)
	}
	if !strings.Contains(err.Error(), "proxy refused CONNECT") {
		t.Errorf("scrubbing ate the diagnosis as well as the secret: %v", err)
	}
}

// TestScrubURLError_DropsAURLTheScrubberCannotMatch pins the unwrap
// half, which the string-scrub tests cannot reach because for a direct
// transport failure both halves remove the same bytes.
//
// They come apart the moment the URL in the *url.Error is not the URL
// the stage rendered. http.Client follows redirects and reports the hop
// it actually failed on, so a webhook endpoint that 302s somewhere else
// produces an error naming a URL no scrubber keyed on the stage's own
// string can match. Unwrapping is what removes it — structurally,
// without needing the two spellings to agree.
func TestScrubURLError_DropsAURLTheScrubberCannotMatch(t *testing.T) {
	stageURL := "https://hooks.example.invalid" + secretWebhookPath
	redirected := "https://relay.internal.invalid/forward/9f3c0a1b-session-token"
	err := scrubURLError(stageURL, &url.Error{
		Op:  "Post",
		URL: redirected,
		Err: errors.New("dial tcp 10.0.0.9:443: i/o timeout"),
	})
	if strings.Contains(err.Error(), redirected) {
		t.Errorf("error carries the redirect target the scrubber cannot know about: %v", err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("unwrapping lost the cause: %v", err)
	}
}

// TestWebhookExecutor_ScrubbedErrorKeepsTheChain is the half a string
// assertion cannot see. Redaction that flattened the cause to text would
// break errors.Is on the wrapped error, and the retry loop depends on it
// twice: a ctrl-C must never be retried (runWithRetryInner's
// context.Canceled early-out) and a dial timeout must still classify as
// one (isRetryable's net.Error check).
func TestWebhookExecutor_ScrubbedErrorKeepsTheChain(t *testing.T) {
	secretURL := "https://hooks.example.invalid" + secretWebhookPath
	exec := &webhookExecutor{doer: errDoer{err: &url.Error{
		Op:  "Post",
		URL: secretURL,
		Err: context.Canceled,
	}}}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the cancelled request to fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; the retry loop would retry a ctrl-C: %v", err)
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("stage error still carries the webhook URL: %v", err)
	}
}

// ── the response body is a credential channel too ─────────────────────
//
// The two redactions above cover errors the TRANSPORT produced. They do
// not cover the one the SERVER produced, and that is the leak with the
// shortest path to a chat channel: for Slack, Discord and ntfy the URL
// path IS the bearer, and a 404 body routinely quotes the request line
// back. The secret then arrives inside far-side prose rather than inside
// a *url.Error, so unwrapping finds nothing to unwrap.

// secretWebhookQuery is the OTHER dialect: the bearer in the query
// string rather than the path. Pinned separately because a scrubber that
// only decomposes the path cannot reach it — exactly the gap a sibling
// PR closed in fleetnotify.ScrubURL, and this asserts vamp's call sites
// benefit from the fixed version.
const secretWebhookQuery = "auth=tk_0000000000000000000000000000"

// echoingServer is the far side that turns its own error page into a
// credential channel: it quotes r.RequestURI (path+query, no scheme, no
// host) back into the body, which is what nginx, Cloudflare and most
// API gateways actually do on a 404.
func echoingServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, "<html><title>error</title><body>no such webhook: %s</body></html>", r.RequestURI)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestWebhookExecutor_ErrorBodyQuotingTheURLIsScrubbed is the headline
// reproduction. A non-2xx body preview goes into the stage error, the
// stage error becomes Executor.FailureSummary, and six executors hand
// that to templates as {{ .failure_summary }} — so a run_when: failure
// webhook posts whatever is in here into the room.
func TestWebhookExecutor_ErrorBodyQuotingTheURLIsScrubbed(t *testing.T) {
	srv := echoingServer(t, http.StatusNotFound)
	secretURL := srv.URL + secretWebhookPath

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the 404 to fail the stage")
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("the response body carried the webhook's secret path into the stage error: %v", err)
	}
	if strings.Contains(err.Error(), secretURL) {
		t.Errorf("stage error contains the full webhook URL: %v", err)
	}
	// Scrubbing that ate the diagnosis has traded one bug for another:
	// the operator still has to be able to tell a 404 from a 500 and to
	// see what the server said.
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("stage error lost the status: %v", err)
	}
	if !strings.Contains(err.Error(), "no such webhook") {
		t.Errorf("stage error lost the server's own message: %v", err)
	}
}

// TestWebhookExecutor_ErrorBodyQuotingTheQueryCredentialIsScrubbed pins
// the `?auth=<token>` dialect. It is the case a path-only scrubber
// cannot reach at all: with a bare "/" path there is no path part to
// match, and an echoed RequestURI contains neither scheme nor host, so
// the whole-URL match does not fire either.
func TestWebhookExecutor_ErrorBodyQuotingTheQueryCredentialIsScrubbed(t *testing.T) {
	srv := echoingServer(t, http.StatusForbidden)
	secretURL := srv.URL + "/?" + secretWebhookQuery

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the 403 to fail the stage")
	}
	if strings.Contains(err.Error(), secretWebhookQuery) {
		t.Errorf("the response body carried the webhook's query credential into the stage error: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("stage error lost the status: %v", err)
	}
}

// TestWebhookExecutor_AssertStatusMismatchBodyIsScrubbed pins the
// sibling site. assert.status_code takes a different branch to the
// "2xx required" default and formats its own preview, so a fix applied
// to one branch and not the other leaves the leak reachable by any
// pipeline that declares an assert.
func TestWebhookExecutor_AssertStatusMismatchBodyIsScrubbed(t *testing.T) {
	srv := echoingServer(t, http.StatusUnauthorized)
	secretURL := srv.URL + secretWebhookPath

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "smoke",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Assert: &AssertSpec{StatusCode: http.StatusOK},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the status mismatch to fail the stage")
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("the assert mismatch text carried the webhook's secret path: %v", err)
	}
	if !strings.Contains(err.Error(), "expected HTTP 200, got HTTP 401") {
		t.Errorf("stage error lost the mismatch it exists to report: %v", err)
	}
}

// TestWebhookExecutor_TransientErrorBodyIsScrubbed pins the typed-error
// branch. A 5xx/429 does not return a plain error — it returns a
// *transientHTTPError whose Underlying string is what the retry loop
// LOGS on every attempt ("attempt %d failed; retrying in %s: %v"), and
// under --detach that log is a file. The scrub must not cost the retry
// machinery its classification or its Retry-After hint.
func TestWebhookExecutor_TransientErrorBodyIsScrubbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "upstream unavailable for %s", r.RequestURI)
	}))
	defer srv.Close()
	secretURL := srv.URL + secretWebhookPath

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the 503 to fail the stage")
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("the 503 body carried the webhook's secret path into the stage error: %v", err)
	}
	th := asTransientHTTPError(err)
	if th == nil {
		t.Fatal("scrubbing broke the transient classification; a 503 would no longer retry")
	}
	if th.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %v, want 2s; the server's own hint was lost", th.RetryAfter)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("stage error lost the status the substring classifier also keys on: %v", err)
	}
}

// nilRequestDoer answers with a hand-built response, the way anything
// that is not an *http.Client does: http.Response.Request is documented
// as "only populated for Client requests".
type nilRequestDoer struct {
	status int
	body   string
}

func (d nilRequestDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: d.status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(d.body)),
	}, nil
}

// TestWebhookExecutor_BodyIsScrubbedWhenTheResponseCarriesNoRequest is
// the half the tests above cannot see. For an *http.Client the URL the
// stage rendered and the URL on resp.Request are the same string, so
// either scrub removes it and the two guards look interchangeable.
//
// They are not. httpDoer is an interface precisely so the transport can
// be swapped, and a response built by hand has a nil Request — at which
// point the scrub keyed on the stage's own URL is the only one left. A
// redaction that works for one implementation of the transport is a
// redaction with a hole in it.
func TestWebhookExecutor_BodyIsScrubbedWhenTheResponseCarriesNoRequest(t *testing.T) {
	secretURL := "https://hooks.example.invalid" + secretWebhookPath
	exec := &webhookExecutor{doer: nilRequestDoer{
		status: http.StatusNotFound,
		body:   "no such webhook: " + secretWebhookPath,
	}}
	_, err := exec.Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the 404 to fail the stage")
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("the scrub depended on the transport populating resp.Request: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("stage error lost the status: %v", err)
	}
}

// TestWebhookExecutor_RedirectTargetQuotedInABodyIsScrubbed covers the
// URL the stage never rendered. http.Client follows redirects, so the
// body that comes back is written by a host we did not address, about a
// URL we did not build — and a scrubber keyed only on the stage's own
// string cannot match it. The URL actually requested is on
// resp.Request, which is where this has to read it from.
func TestWebhookExecutor_RedirectTargetQuotedInABodyIsScrubbed(t *testing.T) {
	const redirectedSecretPath = "/forwarded/9f3c0a1b-session-token-0000"
	final := echoingServer(t, http.StatusNotFound)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+redirectedSecretPath, http.StatusTemporaryRedirect)
	}))
	defer front.Close()

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "notify",
			Type:   StageTypeWebhook,
			URL:    front.URL + secretWebhookPath,
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the redirected 404 to fail the stage")
	}
	if strings.Contains(err.Error(), redirectedSecretPath) {
		t.Errorf("stage error carries the redirect target the stage never rendered: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("stage error lost the status: %v", err)
	}
}

// TestWebhookExecutor_EchoedCredentialHeaderIsScrubbed covers the other
// half of "we sent it, they said it back". The `env` template helper
// exists precisely so a webhook's bearer travels in a header instead of
// the YAML, and debug/gateway endpoints echo request headers into their
// error bodies. The scrub is keyed on the header NAME, so this asserts
// both halves: the credential-named value goes, an ordinary one stays.
func TestWebhookExecutor_EchoedCredentialHeaderIsScrubbed(t *testing.T) {
	const secretToken = "Bearer xoxb-0000-0000-fake-not-a-real-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "bad credentials; received Authorization=%q X-Vamp-Pipeline=%q",
			r.Header.Get("Authorization"), r.Header.Get("X-Vamp-Pipeline"))
	}))
	defer srv.Close()

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:   "notify",
			Type: StageTypeWebhook,
			URL:  srv.URL + "/hook",
			Headers: map[string]string{
				"Authorization":   secretToken,
				"X-Vamp-Pipeline": "demo-pipeline",
			},
			Body:   map[string]any{"text": "done"},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the 401 to fail the stage")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("the echoed Authorization header carried the bearer into the stage error: %v", err)
	}
	if !strings.Contains(err.Error(), "bad credentials") {
		t.Errorf("stage error lost the server's own message: %v", err)
	}
	// Targeted, not blanket: scrubbing every value we sent would erase
	// the ordinary headers an operator debugs with.
	if !strings.Contains(err.Error(), "demo-pipeline") {
		t.Errorf("scrubbing took a non-credential header with it: %v", err)
	}
}

// TestWebhookExecutor_AssertFailureTextIsScrubbed covers the last
// string this executor can return. The body checks echo the user's own
// literals, and a pipeline asserting that an endpoint echoes its URL
// back puts the credential in one — a narrower leak than the body
// preview, included because the rule that stops this class recurring is
// "no string leaves this executor unscrubbed", not "no string we
// currently believe carries a secret".
func TestWebhookExecutor_AssertFailureTextIsScrubbed(t *testing.T) {
	srv := newWebhookTestServer(t) // defaults: 200, body "ok"
	secretURL := srv.URL() + secretWebhookPath

	_, err := (&webhookExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:     "smoke",
			Type:   StageTypeWebhook,
			URL:    secretURL,
			Body:   map[string]any{"text": "done"},
			Assert: &AssertSpec{BodyContains: []string{secretWebhookPath}},
			Output: "out.txt",
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected the body_contains check to fail")
	}
	if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("the assert failure text quoted the webhook's secret path: %v", err)
	}
	if !strings.Contains(err.Error(), "body_contains") {
		t.Errorf("stage error lost the name of the check that failed: %v", err)
	}
}

// TestWebhookExecutor_FailureSummaryDoesNotPostTheCredential is the
// whole chain, driven through the real Executor rather than asserted on
// an error string: a webhook stage fails against a server that quotes
// the request line, and a second `run_when: failure` webhook stage
// posts {{ .failure_summary }} — which is what a real pipeline does —
// to a channel. The assertion is on the bytes that left the process.
func TestWebhookExecutor_FailureSummaryDoesNotPostTheCredential(t *testing.T) {
	upstream := echoingServer(t, http.StatusNotFound)
	secretURL := upstream.URL + secretWebhookPath

	alert := newWebhookTestServer(t)

	var logMu sync.Mutex
	log := &lockedBuffer{mu: &logMu}
	exec := &Executor{
		Pipeline: &Pipeline{
			Name: "leak_chain",
			Stages: []Stage{
				{ID: "post", Type: StageTypeWebhook, URL: secretURL,
					Body: map[string]any{"text": "done"}, Output: "post.txt"},
				{ID: "alert", Type: StageTypeWebhook, URL: alert.URL(),
					Body:   map[string]any{"text": "run failed: {{ .failure_summary }}"},
					Inputs: []string{"post"}, RunWhen: "failure", Output: "alert.txt"},
			},
		},
		RunDir: t.TempDir(),
		Log:    log,
	}
	if err := exec.Run(context.Background()); err == nil {
		t.Fatal("expected the pipeline to fail on the 404")
	} else if strings.Contains(err.Error(), secretWebhookPath) {
		t.Errorf("the aggregated run error carries the webhook's secret path: %v", err)
	}

	req, ok := alert.lastRequest()
	if !ok {
		t.Fatal("the run_when: failure stage never fired; this test proves nothing without it")
	}
	posted := string(req.Body)
	if strings.Contains(posted, secretWebhookPath) {
		t.Errorf("the credential was POSTed into the notification channel:\n%s", posted)
	}
	// Without this the test would pass on a failure_summary that had
	// been emptied rather than scrubbed.
	if !strings.Contains(posted, "HTTP 404") {
		t.Errorf("the notification lost the failure it exists to report:\n%s", posted)
	}
	logMu.Lock()
	logged := log.buf.String()
	logMu.Unlock()
	if strings.Contains(logged, secretWebhookPath) {
		t.Errorf("the run log — a file on disk under --detach — carries the secret path:\n%s", logged)
	}
}

// lockedBuffer is a mutex-guarded io.Writer for tests that hand
// Executor.Log a buffer: the runner writes to it from stage goroutines
// and the deferred summary, and -race objects to an unguarded one.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
