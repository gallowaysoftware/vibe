package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
//
// Every socket in this rig belongs to the subtest that opened it, and the
// teardown order below is part of the test. A connection closed under
// httputil.ReverseProxy's body copy makes it panic with ErrAbortHandler,
// which truncates the chunked response the client is reading — and that
// arrives here as "unexpected EOF" from a read the assertion cannot tell
// apart from the proxy having buffered the completion. Both halves of that
// sentence are reachable through shared state: the client and the proxy's
// upstream dialer are both http.DefaultTransport unless told otherwise,
// and every httptest.Server.Close in this binary (there are dozens) calls
// http.DefaultTransport.CloseIdleConnections as a courtesy. A guard that
// can go red for a reason other than its own trains everyone to re-run it.
func TestProxy_StreamingCompletionIsUnbuffered(t *testing.T) {
	const (
		firstFrame = "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"
		doneFrame  = "data: [DONE]\n\n"
	)
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
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }

			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, firstFrame)
				w.(http.Flusher).Flush()
				// No timeout here: the teardown releases it on every exit
				// path, so the handler cannot outlive the subtest and
				// httptest.Server.Close cannot be left waiting on it.
				<-release
				_, _ = io.WriteString(w, doneFrame)
				w.(http.Flusher).Flush()
			}))
			defer up.Close()
			upstreamConns := &http.Transport{}
			defer upstreamConns.CloseIdleConnections()
			uu, _ := url.Parse(up.URL)

			p := New("127.0.0.1:0")
			tc.setup(p, uu)
			dialUpstreamWith(p, upstreamConns)
			front := httptest.NewServer(p)
			defer front.Close()

			clientConns := &http.Transport{}
			defer clientConns.CloseIdleConnections()
			client := &http.Client{Transport: clientConns}

			resp, err := client.Post(front.URL+"/v1/chat/completions", "application/json", //nolint:bodyclose // closed by the teardown below, which bodyclose cannot follow
				strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
			if err != nil {
				t.Fatalf("post: %v", err)
			}

			readerDone := make(chan struct{})
			// Runs before front.Close/up.Close (defers are LIFO), in the one
			// order that cannot manufacture the failure this test reports:
			// let the upstream finish so the response ends on its own, then
			// close the body — which is also what unparks a reader still
			// waiting on a stream that never came — then join it. A body
			// closed while the copy is live cancels the front request's
			// context, which closes the upstream connection out from under
			// ReverseProxy and logs a copy error belonging to no assertion.
			defer func() {
				releaseUpstream()
				resp.Body.Close()
				<-readerDone
			}()

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
				defer close(readerDone)
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

			releaseUpstream()
			rest, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read rest: %v", err)
			}
			whole := string(head) + string(rest)
			if want := firstFrame + doneFrame; whole != want {
				t.Errorf("stream = %q, want the upstream's bytes verbatim %q", whole, want)
			}
		})
	}
}

// dialUpstreamWith points every backend the proxy currently holds at tr, so
// the hop the streaming assertion depends on is dialed from a pool this test
// owns. Backends are built inside SetBackend/AddRoute/SetModelRewrite, and
// SetModelRewrite rebuilds the default one, so this belongs after a case's
// setup and before the front server takes a request.
func dialUpstreamWith(p *Proxy, tr http.RoundTripper) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.def != nil {
		p.def.rp.Transport = tr
	}
	for _, be := range p.routes {
		be.rp.Transport = tr
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
