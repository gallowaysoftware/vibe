package vamp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
