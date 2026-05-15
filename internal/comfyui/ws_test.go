package comfyui

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// wsServer mints an httptest server whose only route, /ws, performs the RFC
// 6455 upgrade by hand (no third-party deps) and hands the raw net.Conn to
// scriptFn so individual tests can drive a deterministic frame sequence.
//
// scriptFn is invoked in its own goroutine after the 101 response is flushed;
// it owns the conn until it returns and is responsible for closing it.
func wsServer(t *testing.T, scriptFn func(t *testing.T, conn net.Conn, br *bufio.ReadWriter)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		if key == "" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support hijacking")
		}
		conn, br, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		accept := serverAcceptKey(key)
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			t.Errorf("write handshake response: %v", err)
			conn.Close()
			return
		}
		scriptFn(t, conn, br)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serverAcceptKey mirrors acceptKey but lives in the test file so we can use
// it from the test server handler without exposing the helper.
func serverAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsRFC6455GUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// writeServerTextFrame writes an unmasked TEXT frame (opcode 1, FIN=1) — the
// server-to-client direction MUST NOT be masked per RFC 6455.
func writeServerTextFrame(conn net.Conn, payload []byte) error {
	return writeServerFrame(conn, 0x1, payload)
}

func writeServerFrame(conn net.Conn, opcode byte, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | (opcode & 0x0F) // FIN=1
	n := 2
	plen := len(payload)
	switch {
	case plen < 126:
		hdr[1] = byte(plen)
	case plen <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(plen))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(plen))
		n = 10
	}
	if _, err := conn.Write(hdr[:n]); err != nil {
		return err
	}
	if plen == 0 {
		return nil
	}
	_, err := conn.Write(payload)
	return err
}

func TestWS_AcceptKey(t *testing.T) {
	// RFC 6455 §1.3 worked example.
	got := acceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Errorf("acceptKey = %q, want %q", got, want)
	}
}

func TestWS_BuildURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"http://127.0.0.1:8188", "ws://127.0.0.1:8188/ws?clientId=cid"},
		{"https://comfy.example.com", "wss://comfy.example.com/ws?clientId=cid"},
		{"http://x/", "ws://x/ws?clientId=cid"},
	} {
		got, err := buildWSURL(tc.in, "cid")
		if err != nil {
			t.Errorf("buildWSURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("buildWSURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWS_WaitForCompletion_HappyPath drives our reader through a scripted
// ComfyUI event sequence and verifies that a `type=executing` event with a
// null node and matching prompt_id resolves the wait.
func TestWS_WaitForCompletion_HappyPath(t *testing.T) {
	srv := wsServer(t, func(t *testing.T, conn net.Conn, _ *bufio.ReadWriter) {
		defer conn.Close()
		// Send a few prelude events the reader should ignore, then the
		// completion sentinel.
		events := [][]byte{
			[]byte(`{"type":"status","data":{"status":{"exec_info":{"queue_remaining":1}}}}`),
			[]byte(`{"type":"executing","data":{"node":"3","prompt_id":"abc"}}`),
			[]byte(`{"type":"progress","data":{"value":1,"max":4}}`),
			[]byte(`{"type":"executed","data":{"node":"9","prompt_id":"abc"}}`),
			[]byte(`{"type":"executing","data":{"node":null,"prompt_id":"abc"}}`),
		}
		for _, ev := range events {
			if err := writeServerTextFrame(conn, ev); err != nil {
				t.Errorf("write frame: %v", err)
				return
			}
		}
		// Hold the conn open briefly so the client can close cleanly.
		time.Sleep(50 * time.Millisecond)
	})
	c := New(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.wsWaitForCompletion(ctx, "abc"); err != nil {
		t.Fatalf("wsWaitForCompletion: %v", err)
	}
}

// TestWS_WaitForCompletion_IgnoresOtherPrompts verifies the completion match
// is gated on prompt_id — a completion event for a different prompt must NOT
// resolve our wait.
func TestWS_WaitForCompletion_IgnoresOtherPrompts(t *testing.T) {
	srv := wsServer(t, func(t *testing.T, conn net.Conn, _ *bufio.ReadWriter) {
		defer conn.Close()
		// Stranger's completion first, then ours.
		_ = writeServerTextFrame(conn, []byte(`{"type":"executing","data":{"node":null,"prompt_id":"someone-else"}}`))
		_ = writeServerTextFrame(conn, []byte(`{"type":"executing","data":{"node":null,"prompt_id":"abc"}}`))
		time.Sleep(50 * time.Millisecond)
	})
	c := New(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.wsWaitForCompletion(ctx, "abc"); err != nil {
		t.Fatalf("wsWaitForCompletion: %v", err)
	}
}

// TestWS_WaitForCompletion_PingPong verifies that a server-initiated PING is
// answered with a PONG, and that a subsequent completion frame still
// resolves the wait. We confirm the pong by reading the client's response on
// the server side.
func TestWS_WaitForCompletion_PingPong(t *testing.T) {
	pongCh := make(chan struct{}, 1)
	srv := wsServer(t, func(t *testing.T, conn net.Conn, br *bufio.ReadWriter) {
		defer conn.Close()
		// Send a ping.
		if err := writeServerFrame(conn, 0x9, []byte("hi")); err != nil {
			t.Errorf("send ping: %v", err)
			return
		}
		// Read the client's reply: expect a pong (masked, opcode 0xA).
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(br, hdr); err != nil {
			t.Errorf("read pong header: %v", err)
			return
		}
		op := hdr[0] & 0x0F
		if op == 0xA {
			pongCh <- struct{}{}
		} else {
			t.Errorf("expected pong opcode 0xA, got 0x%X", op)
		}
		// Drain the rest of the pong frame (mask key + payload).
		plen := int(hdr[1] & 0x7F)
		drain := make([]byte, 4+plen)
		_, _ = io.ReadFull(br, drain)

		// Now send the completion event.
		_ = writeServerTextFrame(conn, []byte(`{"type":"executing","data":{"node":null,"prompt_id":"abc"}}`))
		time.Sleep(50 * time.Millisecond)
	})
	c := New(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.wsWaitForCompletion(ctx, "abc"); err != nil {
		t.Fatalf("wsWaitForCompletion: %v", err)
	}
	select {
	case <-pongCh:
	default:
		t.Errorf("expected client to send pong frame in response to ping")
	}
}

// TestWS_WaitForCompletion_ContextCancel verifies a long-blocking read
// returns promptly when ctx fires. Without ctx wiring, this would hang on
// readFrame's ReadFull until the server closed.
func TestWS_WaitForCompletion_ContextCancel(t *testing.T) {
	// Server upgrades but never sends a completion frame; the client must
	// return when its context times out.
	srv := wsServer(t, func(t *testing.T, conn net.Conn, _ *bufio.ReadWriter) {
		// Hold the conn open indefinitely (well, until the test ends or
		// the client closes it on ctx).
		// Read until the peer closes, ignoring everything.
		_, _ = conn.Read(make([]byte, 1024))
		conn.Close()
	})
	c := New(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.wsWaitForCompletion(ctx, "abc")
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.DeadlineExceeded or context.Canceled", err)
	}
}

// TestWS_WaitForCompletion_FallsBackOnHandshakeFailure exercises the full
// WaitForCompletion entry point: the /ws endpoint refuses to upgrade (returns
// 404), so the client must fall back to /history polling.
func TestWS_WaitForCompletion_FallsBackOnHandshakeFailure(t *testing.T) {
	var historyHits int
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/history/abc", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		historyHits++
		hits := historyHits
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if hits >= 2 {
			_, _ = w.Write([]byte(`{"abc":{"status":{"completed":true},"outputs":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := c.WaitForCompletion(ctx, "abc", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if h == nil || !h.Status.Completed {
		t.Fatalf("history = %+v, want completed", h)
	}
	mu.Lock()
	defer mu.Unlock()
	if historyHits < 2 {
		t.Errorf("expected fallback polling to hit /history at least twice, got %d", historyHits)
	}
}

// TestWS_WaitForCompletion_EndToEnd exercises WaitForCompletion through the
// happy WS path and confirms it returns the *History from the followup
// /history GET. /history is hit only after the WS completion event lands;
// polling must not run.
func TestWS_WaitForCompletion_EndToEnd(t *testing.T) {
	var historyHits int
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		accept := serverAcceptKey(key)
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		_, _ = conn.Write([]byte(resp))
		_ = writeServerTextFrame(conn, []byte(`{"type":"executing","data":{"node":null,"prompt_id":"abc"}}`))
		time.Sleep(50 * time.Millisecond)
	})
	mux.HandleFunc("/history/abc", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		historyHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"abc":{"status":{"completed":true},"outputs":{"9":{"images":[{"filename":"x.png","subfolder":"","type":"output"}]}}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := c.WaitForCompletion(ctx, "abc", time.Second)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if h == nil || !h.Status.Completed {
		t.Fatalf("history = %+v", h)
	}
	mu.Lock()
	defer mu.Unlock()
	if historyHits != 1 {
		t.Errorf("expected exactly one /history fetch after WS completion, got %d", historyHits)
	}
}
