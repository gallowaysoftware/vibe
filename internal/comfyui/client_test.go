package comfyui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer wires an httptest server to an HTTP handler and returns a
// Client pointing at it. The server is automatically closed at test end.
func newTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, New(srv.URL, srv.Client())
}

func TestClient_UploadImage_OK(t *testing.T) {
	var gotName string
	var gotOverwrite, gotType string
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/upload/image" {
			t.Errorf("got %s %s, want POST /upload/image", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		_, hdr, err := r.FormFile("image")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		gotName = hdr.Filename
		gotOverwrite = r.FormValue("overwrite")
		gotType = r.FormValue("type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"scene_001.png","subfolder":"","type":"input"}`))
	})
	defer srv.Close()

	ui, err := c.UploadImage(context.Background(), "scene_001.png", strings.NewReader("PNGBYTES"))
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if gotName != "scene_001.png" {
		t.Errorf("uploaded filename = %q", gotName)
	}
	if gotOverwrite != "true" || gotType != "input" {
		t.Errorf("form fields overwrite=%q type=%q", gotOverwrite, gotType)
	}
	if ui.Name != "scene_001.png" || ui.InputValue() != "scene_001.png" {
		t.Errorf("UploadedImage = %+v, InputValue=%q", ui, ui.InputValue())
	}
}

func TestClient_UploadImage_Subfolder(t *testing.T) {
	if got := (UploadedImage{Name: "a.png", Subfolder: "sub"}).InputValue(); got != "sub/a.png" {
		t.Errorf("InputValue = %q, want sub/a.png", got)
	}
}

func TestClient_UploadImage_HTTPError(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	defer srv.Close()
	if _, err := c.UploadImage(context.Background(), "x.png", strings.NewReader("x")); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestNew_GeneratesStableClientID(t *testing.T) {
	c := New("http://x:1", nil)
	first := c.ClientID()
	if first == "" {
		t.Fatal("ClientID empty")
	}
	if c.ClientID() != first {
		t.Fatal("ClientID changed between calls")
	}
	c2 := New("http://x:1", nil)
	if c.ClientID() == c2.ClientID() {
		t.Fatal("expected distinct Clients to have distinct client IDs")
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c := New("http://x:1/", nil)
	if c.BaseURL() != "http://x:1" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL(), "http://x:1")
	}
}

func TestClient_Submit_OK(t *testing.T) {
	var gotBody map[string]any
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/prompt" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"prompt_id":"abc","number":1,"node_errors":{}}`))
	})
	_ = srv

	workflow := map[string]any{
		"3": map[string]any{
			"class_type": "KSampler",
			"inputs":     map[string]any{"seed": 42},
		},
	}
	id, err := c.Submit(context.Background(), workflow)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if id != "abc" {
		t.Errorf("prompt_id = %q, want %q", id, "abc")
	}

	if got, ok := gotBody["client_id"].(string); !ok || got != c.ClientID() {
		t.Errorf("client_id in body = %v, want %s", gotBody["client_id"], c.ClientID())
	}
	promptField, ok := gotBody["prompt"].(map[string]any)
	if !ok {
		t.Fatalf("body.prompt missing or wrong type: %T", gotBody["prompt"])
	}
	node3, ok := promptField["3"].(map[string]any)
	if !ok {
		t.Fatalf("body.prompt[\"3\"] missing or wrong type: %T", promptField["3"])
	}
	if node3["class_type"] != "KSampler" {
		t.Errorf("body.prompt[\"3\"].class_type = %v, want KSampler", node3["class_type"])
	}
}

func TestClient_Submit_NodeErrors(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 200 OK with embedded validation errors — ComfyUI's actual shape for
		// a rejected workflow.
		_, _ = w.Write([]byte(`{
			"prompt_id": "",
			"number": 0,
			"node_errors": {
				"7": {
					"class_type": "CLIPTextEncode",
					"dependent_outputs": ["8"],
					"errors": [
						{"type": "required_input_missing", "message": "Required input is missing", "details": "text"}
					]
				}
			}
		}`))
	})

	id, err := c.Submit(context.Background(), map[string]any{})
	if err == nil {
		t.Fatalf("expected error, got id=%q", id)
	}
	var se *SubmitError
	if !errors.As(err, &se) {
		t.Fatalf("err is not *SubmitError: %T %v", err, err)
	}
	if _, ok := se.NodeErrors["7"]; !ok {
		t.Fatalf("missing node 7 in SubmitError.NodeErrors: %+v", se.NodeErrors)
	}
	msg := err.Error()
	if !strings.Contains(msg, "node 7") {
		t.Errorf("error message missing node id: %s", msg)
	}
	if !strings.Contains(msg, "Required input is missing") {
		t.Errorf("error message missing detail text: %s", msg)
	}
}

func TestClient_Submit_HTTPError(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	_, err := c.Submit(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

func TestClient_GetHistory_Completed(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history/abc" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"abc":{"status":{"completed":true},"outputs":{"9":{"images":[{"filename":"x.png","subfolder":"","type":"output"}]}}}}`))
	})

	h, err := c.GetHistory(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if h == nil {
		t.Fatal("history nil")
	}
	if !h.Status.Completed {
		t.Errorf("Completed = false, want true")
	}
	out, ok := h.Outputs["9"]
	if !ok {
		t.Fatalf("Outputs missing key 9: %+v", h.Outputs)
	}
	if len(out.Images) != 1 {
		t.Fatalf("Images = %+v, want one entry", out.Images)
	}
	if out.Images[0].Filename != "x.png" || out.Images[0].Type != "output" {
		t.Errorf("Image = %+v", out.Images[0])
	}
}

// TestClient_GetHistory_VideoAndGifOutputs verifies the History decoder
// recognises the non-image output buckets ComfyUI emits for video / animated
// workflows: `videos` (modern SaveVideo, recent VHS_VideoCombine) and `gifs`
// (SaveAnimatedWEBP, older VHS variants). Same OutputFile shape; only the
// containing key differs.
func TestClient_GetHistory_VideoAndGifOutputs(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"abc":{"status":{"completed":true},"outputs":{
			"10":{"videos":[{"filename":"out.mp4","subfolder":"","type":"output"}]},
			"11":{"gifs":[{"filename":"out.webp","subfolder":"sub","type":"output"}]}
		}}}`))
	})

	h, err := c.GetHistory(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if h == nil {
		t.Fatal("history nil")
	}
	v, ok := h.Outputs["10"]
	if !ok {
		t.Fatalf("Outputs missing key 10: %+v", h.Outputs)
	}
	if len(v.Videos) != 1 {
		t.Fatalf("Videos = %+v, want one entry", v.Videos)
	}
	if v.Videos[0].Filename != "out.mp4" || v.Videos[0].Type != "output" {
		t.Errorf("Video[0] = %+v", v.Videos[0])
	}
	if len(v.Images) != 0 || len(v.Gifs) != 0 {
		t.Errorf("unexpected sibling buckets on node 10: %+v", v)
	}
	g, ok := h.Outputs["11"]
	if !ok {
		t.Fatalf("Outputs missing key 11: %+v", h.Outputs)
	}
	if len(g.Gifs) != 1 {
		t.Fatalf("Gifs = %+v, want one entry", g.Gifs)
	}
	if g.Gifs[0].Filename != "out.webp" || g.Gifs[0].Subfolder != "sub" {
		t.Errorf("Gif[0] = %+v", g.Gifs[0])
	}
}

func TestClient_GetHistory_NotFound(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	h, err := c.GetHistory(context.Background(), "missing")
	if h != nil {
		t.Errorf("history = %+v, want nil", h)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_GetHistory_NotFoundOn404(t *testing.T) {
	// Defensive: some ComfyUI versions might 404 unknown prompt_ids.
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	_, err := c.GetHistory(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_WaitForCompletion(t *testing.T) {
	var n int32
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// WaitForCompletion now prefers a WS subscription and falls back to
		// polling on handshake failure. Reject the /ws upgrade attempt with
		// a 404 (without touching the polling counter) so this test
		// continues to exercise the polling path.
		if r.URL.Path == "/ws" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch atomic.AddInt32(&n, 1) {
		case 1:
			_, _ = w.Write([]byte(`{}`))
		case 2:
			_, _ = w.Write([]byte(`{"abc":{"status":{"completed":false},"outputs":{}}}`))
		default:
			_, _ = w.Write([]byte(`{"abc":{"status":{"completed":true},"outputs":{"9":{"images":[{"filename":"x.png","subfolder":"","type":"output"}]}}}}`))
		}
	})

	const interval = 30 * time.Millisecond
	start := time.Now()
	h, err := c.WaitForCompletion(context.Background(), "abc", interval)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if h == nil || !h.Status.Completed {
		t.Fatalf("history = %+v", h)
	}
	if elapsed < 2*interval {
		t.Errorf("elapsed = %s, want at least %s", elapsed, 2*interval)
	}
	if atomic.LoadInt32(&n) < 3 {
		t.Errorf("server hit %d times, want at least 3", n)
	}
}

func TestClient_WaitForCompletion_ContextCanceled(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Reject the WS subscription so WaitForCompletion falls back to
		// polling — that's the timing path this test cares about.
		if r.URL.Path == "/ws" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Always pending.
		_, _ = w.Write([]byte(`{}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.WaitForCompletion(ctx, "abc", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.DeadlineExceeded or context.Canceled", err)
	}
}

func TestClient_FetchView(t *testing.T) {
	payload := []byte("abcdef")
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/view" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("filename") != "x.png" {
			t.Errorf("filename = %q", q.Get("filename"))
		}
		if q.Get("type") != "output" {
			t.Errorf("type = %q", q.Get("type"))
		}
		if q.Get("subfolder") != "" {
			t.Errorf("subfolder = %q", q.Get("subfolder"))
		}
		_, _ = w.Write(payload)
	})

	rc, err := c.FetchView(context.Background(), OutputFile{Filename: "x.png", Type: "output"})
	if err != nil {
		t.Fatalf("FetchView: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestClient_FetchView_DefaultsType(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "output" {
			t.Errorf("default type = %q, want %q", got, "output")
		}
		_, _ = w.Write([]byte("ok"))
	})

	rc, err := c.FetchView(context.Background(), OutputFile{Filename: "x.png"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
}

func TestClient_FetchView_HTTPError(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})

	_, err := c.FetchView(context.Background(), OutputFile{Filename: "x.png", Type: "output"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status, got: %v", err)
	}
}

func TestClient_SaveOutputToFile(t *testing.T) {
	payload := []byte("hello-image-bytes")
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})

	dir := t.TempDir()
	dest := filepath.Join(dir, "nested", "x.png")
	written, err := c.SaveOutputToFile(context.Background(), OutputFile{Filename: "x.png", Type: "output"}, dest)
	if err != nil {
		t.Fatalf("SaveOutputToFile: %v", err)
	}
	if !filepath.IsAbs(written) {
		t.Errorf("returned path %q is not absolute", written)
	}
	got, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got %q, want %q", got, payload)
	}

	// Ensure the .tmp staging file was cleaned up.
	if _, err := os.Stat(written + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("stray .tmp file: err = %v", err)
	}
}

func TestClient_FreeMemory_OK(t *testing.T) {
	var gotMethod, gotPath, gotBody, gotCT string
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()
	if err := c.FreeMemory(context.Background()); err != nil {
		t.Fatalf("FreeMemory: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/free" {
		t.Errorf("path = %q, want /free", gotPath)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("content-type = %q, want json", gotCT)
	}
	// Both flags must be set; otherwise ComfyUI only unloads weights
	// without releasing the cudaMallocAsync-cached VRAM.
	if !strings.Contains(gotBody, `"unload_models":true`) {
		t.Errorf("body missing unload_models=true: %q", gotBody)
	}
	if !strings.Contains(gotBody, `"free_memory":true`) {
		t.Errorf("body missing free_memory=true: %q", gotBody)
	}
}

func TestClient_FreeMemory_HTTPError(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("free not supported on this build"))
	})
	defer srv.Close()
	err := c.FreeMemory(context.Background())
	if err == nil {
		t.Fatal("FreeMemory should propagate non-2xx; got nil")
	}
	if !strings.Contains(err.Error(), "free not supported") {
		t.Errorf("error should surface server body verbatim; got %v", err)
	}
}
