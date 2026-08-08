package swaptest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// llama-swap's capture buffer, doubled — and the one endpoint this
// package's RECORDER is forbidden to touch (fleet-control C25 §1e).
//
// `GET /api/captures/{id}` answers the verbatim request and response
// bytes of one metered request: the user's prompt, the system prompt, the
// tool definitions and the whole completion. Upstream redacts five HEADER
// names (authorization, proxy-authorization, cookie, set-cookie,
// x-api-key) and nothing in the bodies at all.
//
// Two consequences, and they pull in opposite directions:
//
//   - The double MUST serve the route, because C25's harvest reads it and
//     a consumer written against a fake that lacks it is a consumer
//     nobody has tested. Every capture this file can serve is SYNTHETIC —
//     written by hand, here, out of strings that name themselves as
//     fixtures.
//   - The recorder (record_test.go) must REFUSE it by name. Its other
//     redactions work because the sensitive part is a substring of
//     something still worth keeping; a capture's sensitive part is the
//     whole object. Fold the operator's home directory out of a prompt
//     and you still have the prompt. There is no redaction that leaves a
//     useful capture fixture behind, so the only correct handling is
//     refusal — and refusal has to be a mechanism rather than a note,
//     because the failure mode is a fixture commit that publishes a real
//     prompt from the recording box to a PUBLIC repository.
//
// RefuseCaptureEndpoint is that mechanism. It lives in a non-test file so
// it is part of the package's API, is unit-tested in both directions, and
// is mutation-registered.

// Capture is one stored request/response pair, mirroring llama-swap's
// server.ReqRespCapture field for field (verified against upstream v239
// and v247 — the type is byte-identical across both).
//
// ReqBody and RespBody are []byte, so encoding/json renders them as
// base64 strings, which is what the real endpoint puts on the wire.
type Capture struct {
	ID          int               `json:"id"`
	ReqPath     string            `json:"req_path"`
	ReqHeaders  map[string]string `json:"req_headers"`
	ReqBody     []byte            `json:"req_body"`
	RespHeaders map[string]string `json:"resp_headers"`
	RespBody    []byte            `json:"resp_body"`
}

// SetCapture registers a synthetic capture against an activity row id.
//
// There is deliberately NO auto-population: a row created by Request
// reports has_capture: true (the real endpoint does, on every row) and
// yet 404s here until a test writes one. That default is the eviction
// case — the FIFO buffer dropped it while a walk was in progress — and it
// is the one a harvest is most likely to get wrong, so it is what a test
// gets for free.
func (c *Cell) SetCapture(id int64, cp Capture) {
	cp.ID = int(id)
	c.mu.Lock()
	if c.captures == nil {
		c.captures = map[int64]Capture{}
	}
	c.captures[id] = cp
	c.mu.Unlock()
}

// DropCapture evicts one capture, leaving the activity row in place. That
// is exactly what llama-swap's fixed-size FIFO does under load, and it is
// the only way to reproduce a 404 that arrives BETWEEN the activity read
// and the fetch.
func (c *Cell) DropCapture(id int64) {
	c.mu.Lock()
	delete(c.captures, id)
	c.mu.Unlock()
}

// DropAllCaptures empties the buffer without touching the activity rows —
// the shape of a `-watch-config` reload, which builds a new server with a
// fresh, empty captureCache while carrying the activity store across.
func (c *Cell) DropAllCaptures() {
	c.mu.Lock()
	c.captures = nil
	c.captureReads = 0
	c.mu.Unlock()
}

// CaptureReads counts fetches of GET /api/captures/{id}, answered or not.
// A harvest that "handled" an eviction by retrying forever passes every
// assertion about its OUTPUT; the read count is what shows it.
func (c *Cell) CaptureReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captureReads
}

// handleCapture answers GET /api/captures/{id}, matching upstream's own
// status codes: 400 for an id that is not an integer, 404 for one the
// buffer no longer holds, 200 with the JSON object otherwise.
func (c *Cell) handleCapture(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.captureReads++
	c.mu.Unlock()

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid capture ID"})
		return
	}
	c.mu.Lock()
	cp, ok := c.captures[id]
	c.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "capture not found"})
		return
	}
	writeJSON(w, cp)
}

// ── the recorder's refusal ──────────────────────────────────────────────

// CaptureRoutePrefix is the one path fragment the recorder may not fetch.
const CaptureRoutePrefix = "/api/captures"

// RefuseCaptureEndpoint returns a non-empty REASON when a URL or path
// would fetch a stored request/response capture, and "" otherwise.
//
// It over-refuses on purpose. The comparison is on the lower-cased path
// with the query and fragment removed, and it matches a PREFIX rather
// than a route: `/api/captures/12`, `/api/captures/`, `/API/Captures/12`
// and a full `http://host/api/captures/12?x=1` all refuse. A false
// refusal costs a fixture nobody needed; a false accept publishes an
// operator's prompt to a public repository.
func RefuseCaptureEndpoint(rawURL string) string {
	p := rawURL
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			p = rest[j:]
		} else {
			p = "/"
		}
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if !strings.Contains(strings.ToLower(p), CaptureRoutePrefix) {
		return ""
	}
	return "swaptest recorder: " + CaptureRoutePrefix + "/{id} is REFUSED (" + rawURL + ").\n" +
		"A capture holds the verbatim prompt, system prompt, tool definitions and completion of a real " +
		"request on the recording box. This repository is public, and there is no redaction that leaves a " +
		"useful capture fixture behind — the sensitive part is the whole object, not a substring of it. " +
		"Write a synthetic capture with swaptest.Cell.SetCapture instead (fleet-control C25 §1e)."
}
