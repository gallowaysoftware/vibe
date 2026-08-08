package swaptest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
)

// I7 — the capture contract (fleet-control C25, U13).
//
// C25's harvest is built on three facts about llama-swap that the design
// read off UPSTREAM SOURCE at v239 and v247 rather than off a binary:
// there is no listable /api/captures, the one route is
// `GET /api/captures/{id}` keyed on an ACTIVITY ROW ID, and the payload is
// ReqRespCapture with base64 bodies. This invariant promotes all three
// from "read off source" to "measured", against every wire of the double
// AND — in CI's conformance job — against a real llama-swap binary of each
// pinned version.
//
// It is the one place in this repository that deliberately fetches a
// capture, and the reason it is allowed to is that the request it is
// fetching is one this test issued three lines earlier: the prompt is
// "hi". It asserts STRUCTURE and writes nothing. The recorder, which
// writes to git, refuses the endpoint by name (captures.go's
// RefuseCaptureEndpoint, gated by
// TestRecorderFetchesOnlyThroughTheCaptureRefusal), and no fixture may
// contain one (TestNoFixtureContainsACapture).
func i7(t *testing.T, tgt target) {
	before := headID(t, tgt)
	drive(t, tgt, false)

	// Find the row this call produced, and read its has_capture flag —
	// which is the ONLY enumeration llama-swap offers. There is no listing
	// endpoint; a harvest that assumed one would have nothing to walk.
	var row swaptest.Row
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, r := range activityRows(t, tgt, 20) {
			if r.ID > before && strings.Contains(r.ReqPath, "chat") {
				row = r
			}
		}
		if row.ID != 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if row.ID == 0 {
		t.Fatal("no activity row appeared for the request this test drove; the harvest's enumeration path does not exist on this target")
	}
	if !row.HasCapture {
		if tgt.fake() {
			t.Fatal("the double reported has_capture: false on a row it just wrote. Every real row carries it true (v239 and v247 alike), and a harvest keyed on the flag would find nothing")
		}
		t.Skip("this llama-swap reports has_capture: false — captureBuffer is 0 on this instance, which is a legitimate configuration and the one C25 refuses by name")
	}

	// The route. Method, path shape and status codes are upstream's own:
	// 400 for an id that is not an integer, 404 for one the FIFO buffer no
	// longer holds, 200 with the object otherwise.
	status, body := getRaw(t, fmt.Sprintf("%s/api/captures/%d", tgt.url, row.ID))
	if status != http.StatusOK {
		t.Fatalf("GET /api/captures/%d: HTTP %d — the one route C25 reads is not answering, so the phase has no source of samples on this target", row.ID, status)
	}

	// The payload. Field names and base64 bodies, exactly as
	// internal/vibe/benchreplay decodes them.
	var cap struct {
		ID          int               `json:"id"`
		ReqPath     string            `json:"req_path"`
		ReqHeaders  map[string]string `json:"req_headers"`
		ReqBody     []byte            `json:"req_body"`
		RespHeaders map[string]string `json:"resp_headers"`
		RespBody    []byte            `json:"resp_body"`
	}
	if err := json.Unmarshal(body, &cap); err != nil {
		t.Fatalf("decode the capture: %v", err)
	}
	if int64(cap.ID) != row.ID {
		t.Errorf("capture id %d for activity row %d: the key is the ACTIVITY row id, and a harvest that walked the activity log would fetch the wrong object", cap.ID, row.ID)
	}
	if !strings.Contains(cap.ReqPath, "chat") {
		t.Errorf("req_path %q does not match the row's %q", cap.ReqPath, row.ReqPath)
	}
	if len(cap.ReqBody) == 0 {
		t.Fatal("req_body is empty. The whole phase replays that body; a capture without one is not a sample")
	}
	// The request body is the one this test just sent, so asserting on it
	// asserts nothing about anyone's traffic.
	if !json.Valid(cap.ReqBody) {
		t.Errorf("req_body did not decode to valid JSON (%d bytes); encoding/json + encoding/base64 is the whole decoder C25 uses", len(cap.ReqBody))
	}
	var sent struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(cap.ReqBody, &sent); err != nil {
		t.Errorf("req_body is not the request that was sent: %v", err)
	} else if len(sent.Messages) == 0 {
		t.Error("req_body carries no messages; C25 rewrites `model` and `stream` in exactly this object and leaves the rest alone")
	}

	// The redaction llama-swap performs, and its limit. Five header names
	// are stripped and NOTHING in the bodies is — which is the entire
	// reason this phase treats a capture as the most private object in the
	// fleet.
	for h := range cap.ReqHeaders {
		switch strings.ToLower(h) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key":
			t.Errorf("req_headers carried %q; upstream redacts these five by name", h)
		}
	}

	// And the 404, which is the eviction case a harvest must count rather
	// than retry. An id far above the head cannot exist.
	if s, _ := getRaw(t, fmt.Sprintf("%s/api/captures/%d", tgt.url, row.ID+1_000_000)); s != http.StatusNotFound {
		t.Errorf("GET a capture that cannot exist answered HTTP %d, want 404: the harvest counts a 404 as `evicted`, and anything else is a shape it does not model", s)
	}
}

func getRaw(t *testing.T, url string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, b
}
