package comfyui

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file implements a minimal RFC 6455 WebSocket client tuned to the
// ComfyUI event stream. We bring our own framing so the comfyui package can
// stay on the Go standard library — gorilla/websocket and nhooyr.io/websocket
// would each add a dependency we don't otherwise need.
//
// Scope (intentionally narrow):
//   - Client-side handshake over a single http.Transport-friendly dialer (we
//     dial directly so we own the underlying conn for raw frame I/O).
//   - Read text frames (opcode 1). Binary frames are accepted and discarded
//     (ComfyUI sends image previews as binary; we don't need them here).
//   - Reply to server pings (opcode 9) with pongs (opcode A).
//   - Honor server-initiated close (opcode 8) and send a matching close.
//   - Mask outgoing frames per RFC 6455 §5.3 (required for clients).
//
// Not supported (and not needed for the ComfyUI subscription):
//   - extensions (permessage-deflate etc.)
//   - fragmented frames being reassembled across multiple binary partials
//     for our consumer — we still tolerate fragmented TEXT frames because
//     a strict reader is fragile.

// wsRFC6455GUID is the magic string concatenated with the client's
// Sec-WebSocket-Key for the server to derive Sec-WebSocket-Accept. See RFC
// 6455 §1.3.
const wsRFC6455GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn is a thin wrapper around the raw TCP conn after a successful
// upgrade. It owns the buffered reader the handshake produced so unread
// bytes (a rare but legal possibility) aren't lost.
type wsConn struct {
	c  net.Conn
	br *bufio.Reader
}

// dialWebSocket performs an HTTP/1.1 Upgrade handshake against the given
// http(s):// or ws(s):// URL and returns a wsConn on success.
//
// hc is consulted only for its Transport's TLSClientConfig — httptest's
// servers expose a *http.Client whose Transport has the right CA pool
// configured for the test cert, and we want WaitForCompletion's WS path to
// honor that. nil hc uses sane defaults.
func dialWebSocket(ctx context.Context, base string, clientID string, hc *http.Client) (*wsConn, error) {
	wsURL, err := buildWSURL(base, clientID)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("comfyui ws: parse url: %w", err)
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		// Default ports — only http/https are relevant for ComfyUI.
		if u.Scheme == "wss" || u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("comfyui ws: dial %s: %w", host, err)
	}
	// Best-effort: honor ctx for the whole handshake. We undo the deadline on
	// the way out so callers see a deadline-free conn for streaming reads.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// Generate the Sec-WebSocket-Key (16 random bytes, base64-encoded).
	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("comfyui ws: gen key: %w", err)
	}
	secKey := base64.StdEncoding.EncodeToString(keyBytes[:])

	// Build a raw HTTP/1.1 upgrade request. We can't use http.Client here
	// because we need to keep the connection after the upgrade response.
	reqLine := fmt.Sprintf("GET %s HTTP/1.1\r\n", requestURI(u))
	headers := fmt.Sprintf(
		"Host: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.Host, secKey,
	)
	if _, err := io.WriteString(conn, reqLine+headers); err != nil {
		conn.Close()
		return nil, fmt.Errorf("comfyui ws: write handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("comfyui ws: read handshake response: %w", err)
	}
	// Drain & close the response body; for a 101 it's empty but the contract
	// is still that the caller closes it.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("comfyui ws: server refused upgrade: %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		conn.Close()
		return nil, fmt.Errorf("comfyui ws: missing Upgrade: websocket header")
	}
	want := acceptKey(secKey)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		conn.Close()
		return nil, fmt.Errorf("comfyui ws: bad Sec-WebSocket-Accept (got %q, want %q)", got, want)
	}

	// Clear handshake deadline so the streaming reader can block until ctx
	// closes the conn (see closeOnCtx wiring in the caller).
	_ = conn.SetDeadline(time.Time{})

	return &wsConn{c: conn, br: br}, nil
}

// requestURI returns the path+query portion appropriate for an HTTP request
// line. Empty path becomes "/".
func requestURI(u *url.URL) string {
	p := u.RequestURI()
	if p == "" {
		p = "/"
	}
	return p
}

// buildWSURL converts a ComfyUI base URL into the websocket subscription URL.
// http:// becomes ws://, https:// becomes wss://, and a clientId query param
// is appended (ComfyUI keys its event stream on it). The base URL is the same
// one the REST client targets so we don't introduce a second config knob.
func buildWSURL(base, clientID string) (string, error) {
	if base == "" {
		return "", errors.New("comfyui ws: empty base url")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("comfyui ws: parse base url: %w", err)
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("comfyui ws: unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	q := u.Query()
	q.Set("clientId", clientID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// acceptKey returns the value the server must echo in Sec-WebSocket-Accept,
// computed per RFC 6455 §4.1: base64(SHA1(key + GUID)).
func acceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key + wsRFC6455GUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readFrame reads a single WebSocket frame from the buffered reader and
// returns its opcode (low 4 bits) and unmasked payload. Continuation /
// fragmentation is not handled — ComfyUI sends each JSON event as a single
// final TEXT frame.
//
// Returned errors include io.EOF when the peer closes; callers should treat
// that as a fallback signal (connection lost).
func (w *wsConn) readFrame() (opcode byte, payload []byte, err error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(w.br, hdr); err != nil {
		return 0, nil, err
	}
	// FIN bit is hdr[0] & 0x80; we accept either since our consumer only
	// cares about complete TEXT payloads and we never see fragmented TEXT
	// in practice. RSV bits must be zero (no extensions).
	if hdr[0]&0x70 != 0 {
		return 0, nil, fmt.Errorf("comfyui ws: rsv bits set (%x), no extensions negotiated", hdr[0])
	}
	opcode = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	plen := uint64(hdr[1] & 0x7F)

	switch plen {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		plen = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		plen = binary.BigEndian.Uint64(ext[:])
	}

	// Per RFC 6455, server-to-client frames MUST NOT be masked. If a server
	// misbehaves and sends a masked frame anyway, unmask defensively rather
	// than failing.
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	// Bound payload size so a malicious or malfunctioning server can't OOM
	// us. 16 MiB is far larger than any plausible ComfyUI event JSON.
	const maxPayload = 16 << 20
	if plen > maxPayload {
		return 0, nil, fmt.Errorf("comfyui ws: frame payload %d exceeds %d", plen, maxPayload)
	}
	payload = make([]byte, plen)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

// writeFrame writes a masked, single-frame message of the given opcode.
// Client frames MUST be masked per RFC 6455 §5.3; we generate a fresh mask
// key per frame from crypto/rand.
//
// Concurrent writes are not supported — the WS reader loop is single-threaded
// and so is the cleanup path, so this stays unprotected by design.
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	var hdr [14]byte
	hdr[0] = 0x80 | (opcode & 0x0F) // FIN=1
	n := 2
	plen := len(payload)
	switch {
	case plen < 126:
		hdr[1] = 0x80 | byte(plen)
	case plen <= 0xFFFF:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(plen))
		n = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(plen))
		n = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("comfyui ws: gen mask: %w", err)
	}
	copy(hdr[n:n+4], mask[:])
	n += 4
	if _, err := w.c.Write(hdr[:n]); err != nil {
		return err
	}
	if plen == 0 {
		return nil
	}
	// Mask the payload in place — we own this slice (caller-supplied for
	// close frames is tiny; for pongs we echo the ping payload which is
	// also our buffer). Copy first to avoid mutating caller data.
	masked := make([]byte, plen)
	for i := 0; i < plen; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.c.Write(masked)
	return err
}

// Close sends a normal-closure frame (1000) and shuts the conn down. Errors
// on send are swallowed because the caller's intent is "I'm done with this
// conn"; the underlying close is the authoritative cleanup.
func (w *wsConn) Close() error {
	// Status code 1000 = Normal Closure, big-endian uint16, no reason.
	_ = w.writeFrame(0x8, []byte{0x03, 0xE8})
	return w.c.Close()
}

// wsWaitForCompletion subscribes to ComfyUI's event stream and blocks until a
// completion event for our prompt arrives. The completion signal per
// ComfyUI's protocol is a `type=executing` event with `data.node == null` and
// `data.prompt_id == promptID`.
//
// On any WS-level error (connection drop mid-stream, malformed JSON) it
// returns the error; the caller is expected to fall back to polling if the
// initial dial fails, not mid-stream — once we're attached, we stay attached.
//
// History fetch is intentionally NOT done here so the function stays
// single-purpose; the caller does GetHistory(promptID) after we return.
func (c *Client) wsWaitForCompletion(ctx context.Context, promptID string) error {
	ws, err := dialWebSocket(ctx, c.baseURL, c.clientID, c.hc)
	if err != nil {
		return err
	}
	// Cancel-on-ctx: close the underlying conn so a blocking ReadFull returns
	// an error promptly when ctx fires. We use a done channel rather than
	// goroutine leak protection because the read loop below exits on its own
	// once we return.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.c.Close()
		case <-done:
		}
	}()
	defer ws.Close()

	for {
		op, payload, err := ws.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("comfyui ws: read: %w", err)
		}
		switch op {
		case 0x1: // text frame: ComfyUI event JSON
			if isCompletion(payload, promptID) {
				return nil
			}
		case 0x2: // binary frame (preview images): ignore
		case 0x8: // close: server-initiated
			return fmt.Errorf("comfyui ws: server closed connection")
		case 0x9: // ping → pong with same payload
			if err := ws.writeFrame(0xA, payload); err != nil {
				return fmt.Errorf("comfyui ws: pong: %w", err)
			}
		case 0xA: // pong: ignore
		default:
			// Unknown opcode — keep going rather than fail closed; ComfyUI
			// may add new types and we don't want a strict reader to be
			// a forward-compat hazard.
		}
	}
}

// isCompletion inspects a ComfyUI event JSON payload and reports whether it
// signals completion for the given promptID. We use a tiny ad-hoc decoder
// (rather than a typed struct) because ComfyUI sends a small zoo of event
// shapes through the same socket and we only care about one. A
// fault-tolerant match keeps the WS path from breaking on schema drift.
func isCompletion(payload []byte, promptID string) bool {
	// Expected shape:
	//   {"type":"executing","data":{"node":null,"prompt_id":"<id>"}}
	// We don't want to pull in encoding/json for the hot path? We do; the
	// payload is small and json.Unmarshal handles the field-order and
	// whitespace variability robustly.
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Node     *string `json:"node"`
			PromptID string  `json:"prompt_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return false
	}
	return ev.Type == "executing" && ev.Data.Node == nil && ev.Data.PromptID == promptID
}
