package proxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The streaming contract, pinned.
//
// internal/vibe/proxy is the data plane, and the catalog fix is allowed to
// exist at all only because /v1/models is a discovery GET on a path a
// completion never takes. This test is the standing proof of the other
// half of that claim: a completion still streams through byte for byte and
// chunk by chunk, with the catalog branch present, under every
// configuration that branch touches.
//
// A frontend that stalls here is not a subtle regression. Claude Code
// drops a request after five minutes of silence, so a proxy that buffers a
// stream instead of flushing it turns a slow model into a broken one.
func TestProxy_StreamingCompletionIsUnbuffered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(p *Proxy, upstream *url.URL)
	}{
		{"plain", func(p *Proxy, u *url.URL) { p.SetBackend(u) }},
		{"with a model route registered", func(p *Proxy, u *url.URL) {
			p.SetBackend(u)
			p.AddRoute("other-model", u)
		}},
		{"with a model rewrite", func(p *Proxy, u *url.URL) {
			p.SetBackend(u)
			p.SetModelRewrite("alias", "upstream-id")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			released := make(chan struct{})
			second := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-released:
				case <-time.After(10 * time.Second):
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				w.(http.Flusher).Flush()
				close(second)
			}))
			defer up.Close()
			uu, _ := url.Parse(up.URL)

			p := New("127.0.0.1:0")
			tc.setup(p, uu)
			front := httptest.NewServer(p)
			defer front.Close()

			resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", //nolint:bodyclose // deferred below; bodyclose can't see past the reader goroutine
				strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()

			// Bytes must reach the client while the upstream is still
			// holding the connection open. If the proxy buffered the
			// response, this read blocks until the upstream finishes —
			// which it will not do until we release it.
			//
			// Raw bytes rather than a full SSE frame, because the rewriting
			// backend legitimately holds back one needle's length of tail
			// between reads so a `"model": "…"` token split across a chunk
			// boundary is still found. That hold-back predates this test and
			// is not what it is guarding: full-response buffering is.
			type readResult struct {
				n   int
				err error
			}
			first := make(chan readResult, 1)
			buf := make([]byte, 4096)
			go func() {
				n, err := resp.Body.Read(buf)
				first <- readResult{n, err}
			}()
			var head []byte
			select {
			case got := <-first:
				if got.err != nil && got.err != io.EOF {
					t.Fatalf("first read: %v", got.err)
				}
				if got.n == 0 {
					t.Fatal("first read returned no bytes")
				}
				head = append(head, buf[:got.n]...)
				if !strings.HasPrefix(string(head), "data: ") {
					t.Fatalf("first bytes = %q, want the upstream's bytes verbatim", string(head))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no SSE bytes reached the client while the upstream was still " +
					"streaming: the proxy is buffering the completion path")
			}

			close(released)
			<-second
			rest, err := io.ReadAll(bufio.NewReader(resp.Body))
			if err != nil {
				t.Fatalf("read rest: %v", err)
			}
			whole := string(head) + string(rest)
			if want := "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\ndata: [DONE]\n\n"; whole != want {
				t.Errorf("stream = %q, want the upstream's bytes verbatim %q", whole, want)
			}
		})
	}
}

// TestProxy_CompletionBodyReachesUpstreamVerbatim: the catalog branch runs
// before the routing fast path, so it must not touch a POST body on its
// way past. (The rewrite case, where the body IS edited by design, is
// covered by TestProxy_RoutesByModel and the rewrite tests.)
func TestProxy_CompletionBodyReachesUpstreamVerbatim(t *testing.T) {
	const body = `{"model":"qwen","messages":[{"role":"user","content":"hi"}],"stream":true}`
	got := make(chan string, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
	}))
	defer up.Close()
	uu, _ := url.Parse(up.URL)

	p := New("127.0.0.1:0")
	p.SetBackend(uu)
	p.AddRoute("some-service", uu)

	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if sent := <-got; sent != body {
		t.Errorf("upstream received %q, want %q", sent, body)
	}
}
