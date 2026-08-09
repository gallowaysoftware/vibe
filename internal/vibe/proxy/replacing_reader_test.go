package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The rewriting reader's two obligations, pinned separately from the
// proxy that installs it.
//
// It is the only thing standing between a client and the raw stream on
// the one path that has a body filter at all, and both obligations are
// invisible from the outside when they break: a stream that is CORRECT
// but late still passes every byte-equality test in this package, and a
// stream that ends early still looks like a well-formed OpenAI response
// to anything that does not count [DONE] frames.
//
// SetModelRewrite has one caller — Daemon.applyProfile, on the
// p.Backend.MLXServer arm — so the ids these tests use are filesystem
// paths, because mlx_lm.server answers to nothing else.

const (
	// The shape mlx_lm.server is launched with, and therefore the shape
	// the needle takes in production: `"model": "` + this + `"`.
	mlxModelPath = "/Users/me/models/mlx-community/Qwen3.5-Coder-30B-A3B-Instruct-4bit"
	mlxAlias     = "qwen-mlx"
)

// scriptedSource hands out one chunk per Read, then err (io.EOF when
// nil). reads counts the calls, which is how "the event went out on the
// read that delivered it" is asserted rather than assumed.
type scriptedSource struct {
	chunks [][]byte
	err    error
	reads  int
	closed bool
}

func (s *scriptedSource) Read(p []byte) (int, error) {
	s.reads++
	if len(s.chunks) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		return 0, io.EOF
	}
	n := copy(p, s.chunks[0])
	s.chunks[0] = s.chunks[0][n:]
	if len(s.chunks[0]) == 0 {
		s.chunks = s.chunks[1:]
	}
	return n, nil
}

func (s *scriptedSource) Close() error { s.closed = true; return nil }

func chunks(parts ...string) [][]byte {
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		out = append(out, []byte(p))
	}
	return out
}

// rewritingBody builds the reader the way production does — through
// newRewritingBackend's ModifyResponse — so these tests are pinned to the
// real needles rather than to a second copy of the same string
// concatenation.
func rewritingBody(t *testing.T, alias, upstream string, src io.ReadCloser) io.ReadCloser {
	t.Helper()
	u, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	be := newRewritingBackend(u, &modelRewrite{alias: alias, upstream: upstream})
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   src,
	}
	if err := be.rp.ModifyResponse(resp); err != nil {
		t.Fatalf("ModifyResponse: %v", err)
	}
	if resp.Body == src {
		t.Fatal("ModifyResponse left the body unwrapped: this test would assert nothing")
	}
	return resp.Body
}

func sseFrame(spelling, id, content string) string {
	return `data: {` + spelling + id + `","choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

// TestReplacingReader_HoldsBackOnlyAnAmbiguousTail is ground rule 1 for
// this reader: it may hold back the bytes whose meaning the next chunk
// can still change, and nothing else.
//
// The hold-back used to be one NEEDLE long, and a needle is `"model": "`
// plus the upstream's id — 76 bytes for the path above, longer than a
// whole SSE event. Every event therefore left the proxy only once the
// NEXT event had arrived to push it out, and the last one only at EOF.
// Nothing was lost, which is why every other test in this package stayed
// green, and on a slow model one inter-token gap of lag is what a user
// feels.
func TestReplacingReader_HoldsBackOnlyAnAmbiguousTail(t *testing.T) {
	t.Run("a whole event goes out on the read that delivered it", func(t *testing.T) {
		first := sseFrame(`"model": "`, mlxModelPath, "one")
		second := sseFrame(`"model": "`, mlxModelPath, "two")
		src := &scriptedSource{chunks: chunks(first, second)}
		body := rewritingBody(t, mlxAlias, mlxModelPath, src)

		buf := make([]byte, 4096)
		n, err := body.Read(buf)
		if err != nil {
			t.Fatalf("first read: %v", err)
		}
		want := sseFrame(`"model": "`, mlxAlias, "one")
		if got := string(buf[:n]); got != want {
			t.Errorf("first read = %q,\nwant the whole rewritten event %q", got, want)
		}
		if src.reads != 1 {
			t.Errorf("the reader took %d upstream reads to emit the first event, so events leave "+
				"%d late: the hold-back is bounded by the id's length, not by what is ambiguous",
				src.reads, src.reads-1)
		}
	})

	t.Run("only the ambiguous suffix is held", func(t *testing.T) {
		// `data: {"mo` ends in `"mo`, which could still become `"model": "`.
		// Those three bytes stay; the seven before them cannot be part of any
		// match and go out now.
		src := &scriptedSource{chunks: chunks(`data: {"mo`, `del": "`+mlxModelPath+`","choices":[]}`+"\n\n")}
		body := rewritingBody(t, mlxAlias, mlxModelPath, src)

		buf := make([]byte, 4096)
		n, err := body.Read(buf)
		if err != nil {
			t.Fatalf("first read: %v", err)
		}
		if got := string(buf[:n]); got != `data: {` {
			t.Errorf("first read = %q, want the 7 unambiguous bytes %q", got, `data: {`)
		}
		rest, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("read rest: %v", err)
		}
		whole := `data: {` + string(rest)
		want := `data: {"model": "` + mlxAlias + `","choices":[]}` + "\n\n"
		if whole != want {
			t.Errorf("stream = %q, want %q — the held tail was not reunited with its needle", whole, want)
		}
	})

	t.Run("a tail that cannot match is not held at all", func(t *testing.T) {
		// The end of an SSE event. No needle begins with "\n", so there is
		// nothing to wait for and the terminator must not sit in the buffer
		// waiting for a chunk that, at the end of a slow generation, is a
		// whole token away.
		src := &scriptedSource{chunks: chunks("data: [DONE]\n\n")}
		body := rewritingBody(t, mlxAlias, mlxModelPath, src)
		buf := make([]byte, 4096)
		n, err := body.Read(buf)
		if err != nil {
			t.Fatalf("first read: %v", err)
		}
		if got := string(buf[:n]); got != "data: [DONE]\n\n" {
			t.Errorf("first read = %q, want the whole terminator", got)
		}
	})
}

// TestReplacingReader_MatchesANeedleSplitAtEveryOffset is the property
// the hold-back exists for, checked exhaustively rather than at a few
// sampled boundaries: a `"model": "<id>"` token arriving in two pieces
// must still be rewritten, no matter where the seam falls. Both
// spellings, because encoding/json upstream may or may not put a space
// after the colon, and both id lengths, because the needle's length is
// the id's.
func TestReplacingReader_MatchesANeedleSplitAtEveryOffset(t *testing.T) {
	for _, id := range []string{"upstream-id", mlxModelPath} {
		for _, spelling := range []string{`"model": "`, `"model":"`} {
			frame := sseFrame(spelling, id, "x")
			want := strings.Replace(frame, spelling+id+`"`, spelling+mlxAlias+`"`, 1)
			if want == frame {
				t.Fatalf("test bug: nothing to rewrite in %q", frame)
			}
			t.Run(strings.ReplaceAll(spelling, `"`, "")+"id-"+strconv.Itoa(len(id)), func(t *testing.T) {
				for i := 0; i <= len(frame); i++ {
					src := &scriptedSource{chunks: chunks(frame[:i], frame[i:])}
					got, err := io.ReadAll(rewritingBody(t, mlxAlias, id, src))
					if err != nil {
						t.Fatalf("split at %d: %v", i, err)
					}
					if string(got) != want {
						t.Fatalf("split at %d gave %q,\nwant %q", i, got, want)
					}
				}
				// The same needle arriving one byte per read: the worst case
				// the hold-back has to survive, and the one a small
				// terminal-side write pattern produces.
				parts := make([]string, 0, len(frame))
				for i := 0; i < len(frame); i++ {
					parts = append(parts, frame[i:i+1])
				}
				src := &scriptedSource{chunks: chunks(parts...)}
				got, err := io.ReadAll(rewritingBody(t, mlxAlias, id, src))
				if err != nil {
					t.Fatalf("byte at a time: %v", err)
				}
				if string(got) != want {
					t.Fatalf("byte at a time gave %q,\nwant %q", got, want)
				}
			})
		}
	}
}

// TestReplacingReader_SurfacesTheSourcesMidStreamError: an upstream that
// dies mid-generation must not be reported as a stream that ended.
//
// The reader used to record only THAT the source was done, so the next
// call answered io.EOF and the connection reset became a clean 200 whose
// body simply stops before [DONE]. That erases both signals a truncated
// completion has at once: the client's read error, and the
// "ReverseProxy read error during body copy" line the operator would
// otherwise find in the daemon log. A client cannot tell that response
// from a model that finished.
func TestReplacingReader_SurfacesTheSourcesMidStreamError(t *testing.T) {
	reset := errors.New("read tcp 127.0.0.1:1: connection reset by peer")

	t.Run("with the buffer already drained", func(t *testing.T) {
		frame := sseFrame(`"model": "`, mlxModelPath, "one")
		src := &scriptedSource{chunks: chunks(frame), err: reset}
		got, err := io.ReadAll(rewritingBody(t, mlxAlias, mlxModelPath, src))
		if !errors.Is(err, reset) {
			t.Errorf("read error = %v, want the source's %v", err, reset)
		}
		if want := sseFrame(`"model": "`, mlxAlias, "one"); string(got) != want {
			t.Errorf("bytes = %q, want everything that survived %q", got, want)
		}
	})

	t.Run("with bytes still held back", func(t *testing.T) {
		// The reset lands while a partial needle is being held. Those bytes
		// are the client's — they are emitted first — and the error follows
		// on the next call rather than being lost with them.
		src := &scriptedSource{chunks: chunks(`data: {"mo`), err: reset}
		got, err := io.ReadAll(rewritingBody(t, mlxAlias, mlxModelPath, src))
		if !errors.Is(err, reset) {
			t.Errorf("read error = %v, want the source's %v", err, reset)
		}
		if string(got) != `data: {"mo` {
			t.Errorf("bytes = %q, want the held tail emitted before the error", got)
		}
	})

	t.Run("a source that really ended still reports io.EOF", func(t *testing.T) {
		src := &scriptedSource{chunks: chunks("data: [DONE]\n\n")}
		body := rewritingBody(t, mlxAlias, mlxModelPath, src)
		got, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("read error = %v, want a clean end", err)
		}
		if string(got) != "data: [DONE]\n\n" {
			t.Errorf("bytes = %q", got)
		}
		buf := make([]byte, 8)
		if n, err := body.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("read past the end = (%d, %v), want (0, EOF)", n, err)
		}
		// The wrapper owns the upstream body now, and ReverseProxy closes
		// only what it was handed: a Close that stops here leaks the
		// upstream connection for every completion on an mlx profile.
		if err := body.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if !src.closed {
			t.Error("closing the wrapper did not close the upstream body")
		}
	})
}

// TestProxy_AMidStreamUpstreamFailureIsNotACleanEnd is the same defect
// end to end, and the plain backend is the control: the rewriting path
// must fail a truncated stream the same way the path without a body
// filter does. When it did not, the rewrite was the difference between a
// client that knew its completion was cut off and one that did not.
func TestProxy_AMidStreamUpstreamFailureIsNotACleanEnd(t *testing.T) {
	const frame = "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"
	// The event the model was part way through when it died. It stops
	// inside a `"model": "…"` token on purpose: that is the state in which
	// the rewriting reader is holding bytes back, which is where a
	// swallowed source error becomes a clean io.EOF one call later. A run
	// where the reset outraces those bytes still exercises the plainer
	// path, so this only ever makes the test bite harder, never fail.
	const truncated = `data: {"mo`
	for _, tc := range []struct {
		name  string
		setup func(p *Proxy, u *url.URL)
	}{
		{"plain", func(p *Proxy, u *url.URL) { p.SetBackend(u) }},
		{"with a model rewrite", func(p *Proxy, u *url.URL) {
			p.SetBackend(u)
			p.SetModelRewrite(mlxAlias, mlxModelPath)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Headers, one event, then the connection is reset rather than
			// closed: what an OOM-killed or crashed mlx_lm.server leaves
			// behind, and the one failure that arrives mid-body instead of
			// as a status code.
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, frame)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, truncated)
				w.(http.Flusher).Flush()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0) // close(2) sends RST, not FIN
				}
				_ = conn.Close()
			}))
			defer up.Close()
			upstreamConns := &http.Transport{}
			defer upstreamConns.CloseIdleConnections()
			uu, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}

			p := New("127.0.0.1:0")
			tc.setup(p, uu)
			dialUpstreamWith(p, upstreamConns)
			// The operator's half of the signal. ReverseProxy logs a
			// mid-body read error and then aborts the response; with the
			// error swallowed it logs nothing, because as far as it can see
			// the body was copied in full.
			logged := captureUpstreamErrors(p)
			front := httptest.NewServer(p)
			defer front.Close()

			clientConns := &http.Transport{}
			defer clientConns.CloseIdleConnections()
			client := &http.Client{Transport: clientConns}

			resp, err := client.Post(front.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"`+mlxAlias+`","stream":true,"messages":[]}`))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want the 200 the upstream already sent", resp.StatusCode)
			}

			// Bytes are not the assertion — a reset may or may not race the
			// data already in the receive queue. Whether the truncation is
			// VISIBLE is.
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Error("the client read a clean end from a stream the upstream reset: it sees a " +
					"well-formed completion that merely stops before [DONE], with no way to tell " +
					"that apart from a model that finished")
			}
			select {
			case line := <-logged:
				if line == "" {
					t.Error("ReverseProxy logged an empty line")
				}
			case <-time.After(5 * time.Second):
				t.Error("ReverseProxy logged nothing for a body copy that was cut off: the operator " +
					"has no record of the truncation either")
			}
		})
	}
}

// captureUpstreamErrors routes the default backend's ReverseProxy error
// log to a channel. A channel and not a buffer: the log is written on the
// front server's handler goroutine and read on the test's, and a guard
// that trips the race detector is a guard people learn to re-run.
func captureUpstreamErrors(p *Proxy) <-chan string {
	ch := make(chan string, 8)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.def != nil {
		p.def.rp.ErrorLog = log.New(chanWriter(ch), "", 0)
	}
	return ch
}

type chanWriter chan string

func (c chanWriter) Write(b []byte) (int, error) {
	select {
	case c <- string(b):
	default: // never block the proxy on a test's reader
	}
	return len(b), nil
}
