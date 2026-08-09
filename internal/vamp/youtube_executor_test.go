package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordedHTTPCall captures one outbound HTTP request for assertion. Body is
// the full request body bytes; for form posts the test uses url.ParseQuery to
// decode. We deep-copy header maps so concurrent assertion can't race the
// transport rewriting headers between Do calls.
type recordedHTTPCall struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// recordingDoer is a test httpDoer that records every request and replies
// with the pre-programmed response for the URL pattern it matches. Patterns
// match by substring against METHOD + space + URL so tests can distinguish
// a POST init from a PUT bytes upload to the same /videos endpoint. A
// request that matches no pattern returns a 500 with a clear message so the
// test fails loudly instead of silently returning io.EOF.
type recordingDoer struct {
	mu        sync.Mutex
	calls     []recordedHTTPCall
	responses []doerResponse
}

type doerResponse struct {
	// match is the substring matched against "<METHOD> <URL>". The first
	// matching response wins, so callers list more specific patterns first.
	match string
	// status is the HTTP response status code.
	status int
	// header is added to the response.
	header http.Header
	// body is the response body bytes.
	body []byte
	// err, when non-nil, is returned from Do instead of a response.
	err error
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	if req.Body != nil {
		_ = req.Body.Close()
	}
	hdr := http.Header{}
	for k, v := range req.Header {
		hdr[k] = append([]string(nil), v...)
	}
	d.mu.Lock()
	d.calls = append(d.calls, recordedHTTPCall{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: hdr,
		Body:   body,
	})
	target := req.Method + " " + req.URL.String()
	for _, r := range d.responses {
		if strings.Contains(target, r.match) {
			d.mu.Unlock()
			if r.err != nil {
				return nil, r.err
			}
			resp := &http.Response{
				Status:     fmt.Sprintf("%d", r.status),
				StatusCode: r.status,
				Header:     r.header,
				Body:       io.NopCloser(bytes.NewReader(r.body)),
			}
			if resp.Header == nil {
				resp.Header = http.Header{}
			}
			return resp, nil
		}
	}
	d.mu.Unlock()
	// Unmatched: 500 so the test fails clearly.
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Status:     "500 Internal Server Error",
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("unmatched request: " + target)),
	}, nil
}

func (d *recordingDoer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

func (d *recordingDoer) findCall(urlSubstr string) (recordedHTTPCall, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.calls {
		if strings.Contains(c.URL, urlSubstr) {
			return c, true
		}
	}
	return recordedHTTPCall{}, false
}

// writeVideoFile creates a stub MP4 file at the given path. Used by tests so
// the executor's os.ReadFile succeeds without needing a real video.
func writeVideoFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stubCreds is the test credential loader: returns the supplied credentials
// without touching disk. Tests use this rather than writing a JSON file per
// case so they're trivially parallelisable.
func stubCreds() func(string) (youtubeCredentials, error) {
	return func(string) (youtubeCredentials, error) {
		return youtubeCredentials{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			RefreshToken: "test-refresh-token",
		}, nil
	}
}

// clearYouTubeEnv unsets the three credential env vars for the duration of a
// test so a developer's real values don't override the stub creds.
func clearYouTubeEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envYouTubeClientID, "")
	t.Setenv(envYouTubeClientSecret, "")
	t.Setenv(envYouTubeRefreshToken, "")
}

// TestYouTubeExecutor_HappyPath drives the executor through all three required
// endpoints (token exchange, resumable init, bytes upload), asserts the
// metadata POST body carries the rendered title/description/tags, and verifies
// the output URL ends up in the stage's output file.
func TestYouTubeExecutor_HappyPath(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	videoPath := filepath.Join(runDir, "final.mp4")
	writeVideoFile(t, videoPath, "fake mp4 bytes")

	doer := &recordingDoer{
		responses: []doerResponse{
			{
				match:  "POST https://oauth2.googleapis.com/token",
				status: 200,
				body:   []byte(`{"access_token":"AT-test","token_type":"Bearer","expires_in":3600}`),
			},
			{
				match:  "POST https://www.googleapis.com/upload/youtube/v3/videos",
				status: 200,
				header: http.Header{"Location": []string{"https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&upload_id=abc123"}},
				body:   []byte(``),
			},
			{
				match:  "PUT https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&upload_id=abc123",
				status: 200,
				body:   []byte(`{"id":"vid_xyz","kind":"youtube#video"}`),
			},
		},
	}

	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Inputs:      []string{"assemble", "script"},
		Video:       "{{ .stages.assemble.output }}",
		Title:       "Test: {{ .inputs.title }}",
		Description: "{{ .stages.script.output }}",
		Tags:        []string{"ai", "demo", "vibe"},
		Privacy:     "unlisted",
		Output:      "youtube_url.txt",
	}
	in := StageInput{
		Stage:  stage,
		Inputs: map[string]string{"title": "Hello World"},
		Prior: map[string]*stageResult{
			"assemble": {Output: "final.mp4"},
			"script":   {Output: "A short narration."},
		},
		RunDir: runDir,
	}

	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	wantURL := "https://youtube.com/watch?v=vid_xyz"
	if out.Text != wantURL {
		t.Errorf("out.Text = %q, want %q", out.Text, wantURL)
	}

	if doer.callCount() != 3 {
		t.Errorf("expected 3 HTTP calls (token + init + put), got %d", doer.callCount())
	}

	// Verify token exchange request shape.
	tokenCall, ok := doer.findCall("oauth2.googleapis.com/token")
	if !ok {
		t.Fatal("no token exchange call recorded")
	}
	if tokenCall.Method != http.MethodPost {
		t.Errorf("token call method = %s, want POST", tokenCall.Method)
	}
	if ct := tokenCall.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("token Content-Type = %q", ct)
	}
	form, err := url.ParseQuery(string(tokenCall.Body))
	if err != nil {
		t.Fatalf("token form decode: %v", err)
	}
	if form.Get("grant_type") != "refresh_token" {
		t.Errorf("token grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("refresh_token") != "test-refresh-token" {
		t.Errorf("token refresh_token = %q", form.Get("refresh_token"))
	}
	if form.Get("client_id") != "test-client-id" {
		t.Errorf("token client_id = %q", form.Get("client_id"))
	}

	// Verify the resumable-init POST carries the rendered metadata.
	initCall, ok := doer.findCall("/upload/youtube/v3/videos")
	if !ok {
		t.Fatal("no upload init call recorded")
	}
	if got := initCall.Header.Get("Authorization"); got != "Bearer AT-test" {
		t.Errorf("init Authorization = %q", got)
	}
	if !strings.Contains(initCall.URL, "uploadType=resumable") {
		t.Errorf("init URL missing uploadType=resumable: %s", initCall.URL)
	}
	if !strings.Contains(initCall.URL, "part=snippet") || !strings.Contains(initCall.URL, "status") {
		t.Errorf("init URL missing part=snippet,status: %s", initCall.URL)
	}

	var meta struct {
		Snippet struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			CategoryID  string   `json:"categoryId"`
		} `json:"snippet"`
		Status struct {
			PrivacyStatus string `json:"privacyStatus"`
		} `json:"status"`
	}
	if err := json.Unmarshal(initCall.Body, &meta); err != nil {
		t.Fatalf("init body parse: %v (body=%q)", err, string(initCall.Body))
	}
	if meta.Snippet.Title != "Test: Hello World" {
		t.Errorf("rendered title = %q, want %q", meta.Snippet.Title, "Test: Hello World")
	}
	if meta.Snippet.Description != "A short narration." {
		t.Errorf("rendered description = %q", meta.Snippet.Description)
	}
	if strings.Join(meta.Snippet.Tags, ",") != "ai,demo,vibe" {
		t.Errorf("tags = %v, want [ai demo vibe]", meta.Snippet.Tags)
	}
	if meta.Snippet.CategoryID != "22" {
		t.Errorf("categoryId = %q, want 22 (default)", meta.Snippet.CategoryID)
	}
	if meta.Status.PrivacyStatus != "unlisted" {
		t.Errorf("privacyStatus = %q, want unlisted", meta.Status.PrivacyStatus)
	}

	// Verify bytes upload PUTs to the Location URL we returned.
	putCall, ok := doer.findCall("upload_id=abc123")
	if !ok {
		t.Fatal("no upload PUT call recorded")
	}
	if putCall.Method != http.MethodPut {
		t.Errorf("upload PUT method = %s", putCall.Method)
	}
	if string(putCall.Body) != "fake mp4 bytes" {
		t.Errorf("upload body = %q, want %q", string(putCall.Body), "fake mp4 bytes")
	}

	// And the runner writes the URL to <RunDir>/<output>. The runner
	// (executeStage) owns that write; we replicate the call here through
	// the public StageOutput to verify the executor produced the right
	// payload. The actual file-write is exercised by the runner's tests.
}

// TestYouTubeExecutor_TokenExchangeFails verifies that a 401 from the OAuth
// endpoint surfaces a clear error including Google's response body so users
// can distinguish a revoked refresh token from a network failure.
func TestYouTubeExecutor_TokenExchangeFails(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	videoPath := filepath.Join(runDir, "final.mp4")
	writeVideoFile(t, videoPath, "fake mp4")

	doer := &recordingDoer{
		responses: []doerResponse{
			{
				match:  "POST https://oauth2.googleapis.com/token",
				status: 401,
				body:   []byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`),
			},
		},
	}
	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected token-exchange error")
	}
	if !strings.Contains(err.Error(), "oauth") {
		t.Errorf("error should mention oauth, got: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should surface Google's error body, got: %v", err)
	}
	if doer.callCount() != 1 {
		t.Errorf("expected exactly 1 HTTP call before failure, got %d", doer.callCount())
	}
}

// TestYouTubeExecutor_UploadFails verifies a 500 from the resumable-init
// endpoint produces an error that identifies which step failed (so the user
// can tell upload from auth from byte-PUT failures).
func TestYouTubeExecutor_UploadFails(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	videoPath := filepath.Join(runDir, "final.mp4")
	writeVideoFile(t, videoPath, "fake mp4")

	doer := &recordingDoer{
		responses: []doerResponse{
			{
				match:  "POST https://oauth2.googleapis.com/token",
				status: 200,
				body:   []byte(`{"access_token":"AT","token_type":"Bearer"}`),
			},
			{
				match:  "POST https://www.googleapis.com/upload/youtube/v3/videos",
				status: 500,
				body:   []byte(`{"error":{"code":500,"message":"Internal Server Error"}}`),
			},
		},
	}
	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected upload-init error")
	}
	if !strings.Contains(err.Error(), "init upload") {
		t.Errorf("error should identify the init-upload step, got: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should surface status code, got: %v", err)
	}
}

// TestYouTubeExecutor_WithThumbnail verifies that when a thumbnail path is
// configured the executor fires the extra POST to thumbnails/set with the
// image bytes after a successful video upload.
func TestYouTubeExecutor_WithThumbnail(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "fake mp4")
	writeVideoFile(t, filepath.Join(runDir, "cover.png"), "fake png bytes")

	doer := &recordingDoer{
		responses: []doerResponse{
			{
				match:  "POST https://oauth2.googleapis.com/token",
				status: 200,
				body:   []byte(`{"access_token":"AT","token_type":"Bearer"}`),
			},
			{
				match:  "POST https://www.googleapis.com/upload/youtube/v3/videos",
				status: 200,
				header: http.Header{"Location": []string{"https://example.com/upload?id=u1"}},
			},
			{
				match:  "PUT https://example.com/upload?id=u1",
				status: 200,
				body:   []byte(`{"id":"vid_thumb"}`),
			},
			{
				match:  "POST https://www.googleapis.com/upload/youtube/v3/thumbnails/set",
				status: 200,
				body:   []byte(`{"items":[{"default":{"url":"..."}}]}`),
			},
		},
	}
	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Thumbnail:   "cover.png",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Text, "vid_thumb") {
		t.Errorf("out URL = %q, want substring vid_thumb", out.Text)
	}

	thumbCall, ok := doer.findCall("thumbnails/set")
	if !ok {
		t.Fatal("no thumbnail POST recorded")
	}
	if thumbCall.Method != http.MethodPost {
		t.Errorf("thumbnail method = %s", thumbCall.Method)
	}
	if !strings.Contains(thumbCall.URL, "videoId=vid_thumb") {
		t.Errorf("thumbnail URL missing videoId: %s", thumbCall.URL)
	}
	if got := thumbCall.Header.Get("Authorization"); got != "Bearer AT" {
		t.Errorf("thumbnail Authorization = %q", got)
	}
	if string(thumbCall.Body) != "fake png bytes" {
		t.Errorf("thumbnail body = %q, want %q", string(thumbCall.Body), "fake png bytes")
	}
}

// TestYouTubeExecutor_PrivacyValidation verifies that an invalid privacy value
// is rejected at pipeline-validation time, before any HTTP call is attempted.
// The check lives in pipeline.go but the failure surface is what users see.
func TestYouTubeExecutor_PrivacyValidation(t *testing.T) {
	yaml := `
name: x
stages:
  - id: publish
    type: youtube
    video: final.mp4
    title: T
    description: D
    privacy: secret
    output: url.txt
`
	_, err := LoadPipeline(writePipeline(t, yaml))
	if err == nil {
		t.Fatal("expected validation error for invalid privacy")
	}
	if !strings.Contains(err.Error(), "privacy") {
		t.Errorf("error should mention privacy, got: %v", err)
	}
	if !strings.Contains(err.Error(), "private") || !strings.Contains(err.Error(), "unlisted") || !strings.Contains(err.Error(), "public") {
		t.Errorf("error should list allowed values, got: %v", err)
	}
}

// TestYouTubeExecutor_DefaultPrivacyIsPrivate verifies the omitted-privacy
// path defaults to "private" — the safest default for an automated upload
// pipeline so a misconfigured run doesn't accidentally publish.
func TestYouTubeExecutor_DefaultPrivacyIsPrivate(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "x")

	doer := &recordingDoer{
		responses: []doerResponse{
			{match: "POST https://oauth2.googleapis.com/token", status: 200, body: []byte(`{"access_token":"AT"}`)},
			{match: "POST https://www.googleapis.com/upload/youtube/v3/videos", status: 200, header: http.Header{"Location": []string{"https://example.com/u?id=x"}}},
			{match: "PUT https://example.com/u?id=x", status: 200, body: []byte(`{"id":"v"}`)},
		},
	}
	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	initCall, ok := doer.findCall("/upload/youtube/v3/videos")
	if !ok {
		t.Fatal("init call missing")
	}
	if !strings.Contains(string(initCall.Body), `"privacyStatus":"private"`) {
		t.Errorf("expected default privacyStatus=private, body=%q", string(initCall.Body))
	}
}

// TestYouTubeExecutor_EnvCredentialsOverride verifies that the env vars override
// (or stand in for) the credentials file. This is the documented escape hatch
// for CI / secret-store-backed deploys that don't want a JSON on disk.
func TestYouTubeExecutor_EnvCredentialsOverride(t *testing.T) {
	t.Setenv(envYouTubeClientID, "env-client-id")
	t.Setenv(envYouTubeClientSecret, "env-client-secret")
	t.Setenv(envYouTubeRefreshToken, "env-refresh-token")
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "x")

	doer := &recordingDoer{
		responses: []doerResponse{
			{match: "POST https://oauth2.googleapis.com/token", status: 200, body: []byte(`{"access_token":"AT"}`)},
			{match: "POST https://www.googleapis.com/upload/youtube/v3/videos", status: 200, header: http.Header{"Location": []string{"https://example.com/up?id=e"}}},
			{match: "PUT https://example.com/up?id=e", status: 200, body: []byte(`{"id":"vEnv"}`)},
		},
	}
	// credsLoader that always reports file-not-found so we know env vars
	// alone drive the success path.
	exec := &youtubeExecutor{
		doer: doer,
		credsLoader: func(string) (youtubeCredentials, error) {
			return youtubeCredentials{}, os.ErrNotExist
		},
	}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	tokenCall, ok := doer.findCall("oauth2.googleapis.com/token")
	if !ok {
		t.Fatal("no token call recorded")
	}
	form, err := url.ParseQuery(string(tokenCall.Body))
	if err != nil {
		t.Fatalf("token form: %v", err)
	}
	if form.Get("client_id") != "env-client-id" {
		t.Errorf("client_id = %q, want env-client-id", form.Get("client_id"))
	}
	if form.Get("refresh_token") != "env-refresh-token" {
		t.Errorf("refresh_token = %q, want env-refresh-token", form.Get("refresh_token"))
	}
}

// TestYouTubeExecutor_MissingCredentials surfaces a clear error when neither
// the credentials file nor the env vars supply complete credentials. Users
// running into this should see exactly what to set, not a 401 from Google.
func TestYouTubeExecutor_MissingCredentials(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "x")

	exec := &youtubeExecutor{
		credsLoader: func(string) (youtubeCredentials, error) {
			return youtubeCredentials{}, os.ErrNotExist
		},
	}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected missing-credentials error")
	}
	if !strings.Contains(err.Error(), "client_id") || !strings.Contains(err.Error(), "client_secret") || !strings.Contains(err.Error(), "refresh_token") {
		t.Errorf("error should list missing fields, got: %v", err)
	}
	if !strings.Contains(err.Error(), envYouTubeClientID) {
		t.Errorf("error should mention env-var name, got: %v", err)
	}
}

// TestYouTubeExecutor_NoLocationHeader verifies a malformed resumable-init
// response (missing Location) surfaces an explicit error rather than a
// downstream "empty url" failure on the PUT.
func TestYouTubeExecutor_NoLocationHeader(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "x")

	doer := &recordingDoer{
		responses: []doerResponse{
			{match: "POST https://oauth2.googleapis.com/token", status: 200, body: []byte(`{"access_token":"AT"}`)},
			{match: "POST https://www.googleapis.com/upload/youtube/v3/videos", status: 200, body: []byte(``)}, // missing Location
		},
	}
	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected error when Location header is missing")
	}
	if !strings.Contains(err.Error(), "Location") {
		t.Errorf("error should mention Location, got: %v", err)
	}
}

// TestYouTubeExecutor_PUTResume308 verifies the Phase-1 behaviour for a 308
// Resume Incomplete: surfaced as a clear error so users know to retry rather
// than hang. (Real chunked resume is Phase 2.)
func TestYouTubeExecutor_PUTResume308(t *testing.T) {
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "x")

	doer := &recordingDoer{
		responses: []doerResponse{
			{match: "POST https://oauth2.googleapis.com/token", status: 200, body: []byte(`{"access_token":"AT"}`)},
			{match: "POST https://www.googleapis.com/upload/youtube/v3/videos", status: 200, header: http.Header{"Location": []string{"https://example.com/u?id=z"}}},
			{match: "PUT https://example.com/u?id=z", status: 308, body: []byte(``)},
		},
	}
	exec := &youtubeExecutor{doer: doer, credsLoader: stubCreds()}
	stage := &Stage{
		ID:          "publish",
		Type:        StageTypeYouTube,
		Video:       "final.mp4",
		Title:       "T",
		Description: "D",
		Output:      "url.txt",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for 308 Resume Incomplete")
	}
	if !strings.Contains(err.Error(), "308") {
		t.Errorf("error should reference 308, got: %v", err)
	}
}

// TestYouTubeExecutor_ReadCredentialsFromDisk verifies the production
// credsLoader parses an on-disk JSON file with the expected schema. This is
// the single bit of file-IO logic in the executor not exercised by the stub
// loader above.
func TestYouTubeExecutor_ReadCredentialsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	body := `{"client_id":"cid","client_secret":"csec","refresh_token":"rtok"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readCredentialsFromDisk(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ClientID != "cid" || got.ClientSecret != "csec" || got.RefreshToken != "rtok" {
		t.Errorf("creds = %+v", got)
	}

	// Missing file path surfaces os.ErrNotExist so loadCredentials can treat
	// it as "env vars must provide" rather than fatal.
	_, err = readCredentialsFromDisk(filepath.Join(dir, "nope.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file err = %v, want os.ErrNotExist", err)
	}
}

// ── the resumable-upload session URI is a credential ──────────────────

// secretUploadID is the authorisation half of a resumable-upload session
// URI. Google carries it in the QUERY and documents the URI itself as a
// value that must be kept secret: whoever holds it can append to,
// complete or abort THAT upload without the access token that created it.
const secretUploadID = "AEnB2Uo-SECRET-SESSION-TOKEN-9f3c0a1b"

// sessionURI is the full session URI the tests below must never see leak.
const sessionURI = "https://www.googleapis.com/upload/youtube/v3/videos?uploadType=resumable&upload_id=" + secretUploadID

// youtubeSessionDoer answers the token exchange and the resumable init
// normally, then fails (or answers) the bytes PUT as instructed. That is
// the shape of every failure on the upload: the session URI has already
// been handed out, and the error is what carries it back.
//
// The "PUT " pattern is listed BEFORE the init POST because recordingDoer
// matches on "<METHOD> <URL>" substrings, first match wins, and the two
// share a URL.
func youtubeSessionDoer(put doerResponse) *recordingDoer {
	put.match = "PUT "
	return &recordingDoer{
		responses: []doerResponse{
			{
				match:  "POST https://oauth2.googleapis.com/token",
				status: 200,
				body:   []byte(`{"access_token":"AT-test","token_type":"Bearer","expires_in":3600}`),
			},
			put,
			{
				match:  "POST https://www.googleapis.com/upload/youtube/v3/videos",
				status: 200,
				header: http.Header{"Location": []string{sessionURI}},
			},
		},
	}
}

// runYouTubeUpload drives one upload through doer and returns the stage
// error plus whatever reached the run log.
func runYouTubeUpload(t *testing.T, doer *recordingDoer) (string, error) {
	t.Helper()
	clearYouTubeEnv(t)
	runDir := t.TempDir()
	writeVideoFile(t, filepath.Join(runDir, "final.mp4"), "fake mp4 bytes")
	var log bytes.Buffer
	_, err := (&youtubeExecutor{doer: doer, credsLoader: stubCreds()}).Execute(
		context.Background(),
		StageInput{
			Stage: &Stage{
				ID:          "publish",
				Type:        StageTypeYouTube,
				Video:       "final.mp4",
				Title:       "T",
				Description: "D",
				Output:      "url.txt",
			},
			RunDir: runDir,
			Log:    &log,
		})
	return log.String(), err
}

// TestYouTubeExecutor_TransportErrorDoesNotCarryTheSessionURI is the leak
// PR #71 left open after closing the identical one for webhook URLs.
//
// *url.Error embeds the request URL STRUCTURALLY, so every transport
// failure on the bytes PUT produced the session URI verbatim — and a
// stage error becomes Executor.FailureSummary, which six executors hand
// to templates as {{ .failure_summary }}. A `run_when: failure` webhook
// then POSTS the credential into a chat channel, which is the path that
// made the webhook version serious.
func TestYouTubeExecutor_TransportErrorDoesNotCarryTheSessionURI(t *testing.T) {
	log, err := runYouTubeUpload(t, youtubeSessionDoer(doerResponse{err: &url.Error{
		Op:  "Put",
		URL: sessionURI,
		Err: errors.New("read tcp 10.0.0.9:443: connection reset by peer"),
	}}))
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if strings.Contains(err.Error(), secretUploadID) {
		t.Errorf("the stage error carries the session URI, and FailureSummary hands it to {{ .failure_summary }}: %v", err)
	}
	if strings.Contains(log, secretUploadID) {
		t.Errorf("the run log carries the session URI; under --detach that log is a FILE kept after the run:\n%s", log)
	}
	// The diagnosis must survive the redaction, or the fix trades a leak
	// for an unusable error.
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("scrubbing removed the transport's own message: %v", err)
	}
}

// TestYouTubeExecutor_SessionURIQuotedInAMessageIsScrubbed covers the
// half unwrapping alone does not reach: a transport (a proxy, a
// middlebox, an SDK) that quotes the URL inside its OWN prose rather than
// in *url.Error's structural field. Unwrapping drops the wrapper and
// leaves that text untouched; ScrubURL is what removes it.
func TestYouTubeExecutor_SessionURIQuotedInAMessageIsScrubbed(t *testing.T) {
	_, err := runYouTubeUpload(t, youtubeSessionDoer(doerResponse{
		err: errors.New("proxy rejected CONNECT for " + sessionURI + ": policy denied"),
	}))
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if strings.Contains(err.Error(), secretUploadID) {
		t.Errorf("a transport that quoted the URI in its own message leaked it: %v", err)
	}
	if !strings.Contains(err.Error(), "policy denied") {
		t.Errorf("scrubbing removed the transport's own message: %v", err)
	}
}

// TestYouTubeExecutor_RedirectedSessionURIIsDropped pins the UNWRAP half
// specifically, which no string scrubber can do.
//
// The two halves are indistinguishable on a direct failure — both remove
// the same bytes — and come apart the moment the URL inside the
// *url.Error is not the one we passed. http.Client follows redirects and
// names the hop it actually failed on, and Google's upload endpoints do
// redirect to regional hosts. A scrub keyed on the session URI we hold
// cannot match that string; dropping the wrapper removes it structurally.
func TestYouTubeExecutor_RedirectedSessionURIIsDropped(t *testing.T) {
	redirected := "https://upload-eu-west1.googleusercontent.invalid/resume/OTHER-SESSION-4d2f"
	_, err := runYouTubeUpload(t, youtubeSessionDoer(doerResponse{err: &url.Error{
		Op:  "Put",
		URL: redirected,
		Err: errors.New("dial tcp 10.0.0.9:443: i/o timeout"),
	}}))
	if err == nil {
		t.Fatal("expected the upload to fail")
	}
	if strings.Contains(err.Error(), redirected) {
		t.Errorf("the error carries a redirect target no scrubber could know about: %v", err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("unwrapping lost the cause: %v", err)
	}
}

// TestYouTubeExecutor_ScrubbedUploadErrorKeepsTheChain is the half a
// string assertion cannot see. Redaction that flattened the cause to text
// would break errors.Is on the wrapped error, and the runner depends on
// it twice: runWithRetryInner's context.Canceled early-out must never
// retry a ctrl-C, and isRetryable's net.Error check must still classify a
// dial timeout as transient.
func TestYouTubeExecutor_ScrubbedUploadErrorKeepsTheChain(t *testing.T) {
	_, err := runYouTubeUpload(t, youtubeSessionDoer(doerResponse{err: &url.Error{
		Op:  "Put",
		URL: sessionURI,
		Err: context.Canceled,
	}}))
	if err == nil {
		t.Fatal("expected the cancelled upload to fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation stopped being reachable through the chain; a ctrl-C would be retried: %v", err)
	}
	if strings.Contains(err.Error(), secretUploadID) {
		t.Errorf("keeping the chain reintroduced the leak: %v", err)
	}
}

// TestYouTubeExecutor_ErrorBodyQuotingTheSessionURIIsScrubbed covers the
// non-2xx path, where the leak arrives in the RESPONSE rather than in a
// transport error. Google's quota and redirect responses echo the request
// in some shapes, and that body is pasted straight into the stage error.
func TestYouTubeExecutor_ErrorBodyQuotingTheSessionURIIsScrubbed(t *testing.T) {
	_, err := runYouTubeUpload(t, youtubeSessionDoer(doerResponse{
		status: 403,
		body:   []byte(`{"error":{"message":"quota exceeded for ` + sessionURI + `"}}`),
	}))
	if err == nil {
		t.Fatal("expected a 403 to fail the stage")
	}
	if strings.Contains(err.Error(), secretUploadID) {
		t.Errorf("a response body quoting the session URI leaked it into the stage error: %v", err)
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("scrubbing removed the server's own diagnosis: %v", err)
	}
}

// TestYouTubeExecutor_LogRecordsARedactedSessionURI is the positive half
// of the log rule. The run log still has to distinguish two uploads — a
// foreach publishing twelve episodes — so the answer is not silence but
// fleetnotify.Redact: scheme, host, and a stable 8-hex id, with the path
// and query (where the session's bearer lives) dropped.
func TestYouTubeExecutor_LogRecordsARedactedSessionURI(t *testing.T) {
	log, err := runYouTubeUpload(t, youtubeSessionDoer(doerResponse{
		status: 200,
		body:   []byte(`{"id":"vid_xyz"}`),
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(log, "upload_id=") || strings.Contains(log, secretUploadID) {
		t.Errorf("the run log carries the session URI's query:\n%s", log)
	}
	if !strings.Contains(log, "www.googleapis.com") {
		t.Errorf("redaction removed the host, leaving nothing to debug with:\n%s", log)
	}
	if !strings.Contains(log, "(id ") {
		t.Errorf("redaction dropped the stable id that tells two sessions apart:\n%s", log)
	}
}
