package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// upstreamEcho stands in for mlx_lm.server: it records the "model" it was
// sent and serves /v1/models under the id it was launched with.
func upstreamEcho(t *testing.T, upstreamID string, gotModel *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + upstreamID + `","object":"model","created":1}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var doc struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &doc)
		*gotModel = doc.Model
		// Echo the declared length back so a mismatched Content-Length
		// (a truncated rewrite) shows up as a JSON parse failure above.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func newProxyTo(t *testing.T, target string) *Proxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := New("127.0.0.1:0")
	p.SetBackend(u)
	return p
}

func TestModelRewrite_RequestAliasBecomesUpstreamID(t *testing.T) {
	const upstreamID = "/Users/me/models/qwen-mlx-4bit"
	var got string
	up := upstreamEcho(t, upstreamID, &got)
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("qwen-mlx", upstreamID)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-mlx","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got != upstreamID {
		t.Errorf("upstream saw model %q, want the rewritten %q", got, upstreamID)
	}
}

// Other fields must survive the re-serialisation — the rewrite rebuilds the
// JSON document, so a dropped "messages" or "stream" would be silent.
func TestModelRewrite_PreservesOtherFields(t *testing.T) {
	const upstreamID = "/models/x"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("upstream got invalid JSON: %v (%s)", err, body)
			return
		}
		if doc["model"] != upstreamID {
			t.Errorf("model = %v", doc["model"])
		}
		if doc["stream"] != true {
			t.Errorf("stream field lost: %v", doc)
		}
		if doc["temperature"] != 0.7 {
			t.Errorf("temperature lost: %v", doc)
		}
		if _, ok := doc["messages"]; !ok {
			t.Errorf("messages lost: %v", doc)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("alias", upstreamID)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"alias","stream":true,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`))
	p.ServeHTTP(httptest.NewRecorder(), req)
}

// A request naming something else is not ours to touch — passing it through
// unchanged lets the upstream produce its own error.
func TestModelRewrite_LeavesOtherModelsAlone(t *testing.T) {
	var got string
	up := upstreamEcho(t, "/models/x", &got)
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("alias", "/models/x")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"something-else"}`))
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got != "something-else" {
		t.Errorf("model = %q, want the untouched original", got)
	}
}

func TestModelRewrite_ModelsListShowsAlias(t *testing.T) {
	const upstreamID = "/Users/me/models/qwen-mlx-4bit"
	var ignored string
	up := upstreamEcho(t, upstreamID, &ignored)
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("qwen-mlx", upstreamID)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var ml struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ml); err != nil {
		t.Fatalf("bad models payload: %v (%s)", err, rec.Body.String())
	}
	if len(ml.Data) != 1 || ml.Data[0].ID != "qwen-mlx" {
		t.Errorf("/v1/models = %s, want the client-facing alias", rec.Body.String())
	}
}

// Clearing the rewrite must restore plain passthrough, since SetBackend and
// SetModelRewrite are driven together on every profile switch.
func TestModelRewrite_ClearedOnEmptyArgs(t *testing.T) {
	var got string
	up := upstreamEcho(t, "/models/x", &got)
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("alias", "/models/x")
	p.SetModelRewrite("", "")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"alias"}`))
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got != "alias" {
		t.Errorf("model = %q, want the untouched alias after clearing", got)
	}
}

// The response echoes whatever id the upstream knows itself by. Without a
// response-side rewrite that is an absolute filesystem path, leaked to
// every client and (via llama-swap, which doesn't rewrite responses) to
// every hum user too.
func TestModelRewrite_ResponseModelBecomesAlias(t *testing.T) {
	const upstreamID = "/Users/me/models/qwen-mlx-4bit"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"` + upstreamID + `","choices":[]}`))
	}))
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("qwen-mlx", upstreamID)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-mlx"}`)))

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
	}
	if doc["model"] != "qwen-mlx" {
		t.Errorf("response model = %v, want the client-facing alias", doc["model"])
	}
	if strings.Contains(rec.Body.String(), "/Users/me") {
		t.Errorf("response still leaks the snapshot path: %s", rec.Body.String())
	}
}

// SSE chunks carry the model id too, and must be rewritten without the
// stream being buffered into one blob.
func TestModelRewrite_StreamingChunksRewritten(t *testing.T) {
	const upstreamID = "/Users/me/models/qwen-mlx-4bit"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte(`data: {"model":"` + upstreamID + `","choices":[{"delta":{"content":"x"}}]}` + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("qwen-mlx", upstreamID)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-mlx","stream":true}`)))

	body := rec.Body.String()
	if strings.Contains(body, "/Users/me") {
		t.Errorf("streamed chunks still leak the path:\n%s", body)
	}
	if n := strings.Count(body, `"model":"qwen-mlx"`); n != 3 {
		t.Errorf("rewrote %d of 3 chunks:\n%s", n, body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("stream terminator lost:\n%s", body)
	}
}

// Content that merely mentions the path must survive: only the model field
// token is a substitution target.
func TestModelRewrite_LeavesContentMentioningPathAlone(t *testing.T) {
	const upstreamID = "/Users/me/models/qwen-mlx-4bit"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"` + upstreamID + `","choices":[{"message":{"content":"your model is at ` + upstreamID + ` on disk"}}]}`))
	}))
	defer up.Close()

	p := newProxyTo(t, up.URL)
	p.SetModelRewrite("qwen-mlx", upstreamID)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-mlx"}`)))

	var doc struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
	}
	if doc.Model != "qwen-mlx" {
		t.Errorf("model = %q, want the alias", doc.Model)
	}
	if !strings.Contains(doc.Choices[0].Message.Content, upstreamID) {
		t.Errorf("content was rewritten but should not have been: %q", doc.Choices[0].Message.Content)
	}
}
