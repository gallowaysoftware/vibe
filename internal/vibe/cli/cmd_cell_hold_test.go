package cli

// C11 coverage for the desk path: `vibe cell hold` speaks to the lease
// endpoint (no new route), reports what the REGISTRY stored, and never
// drops the not-a-pin caveat.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// leaseRecorder is a fleetd that records lease mutations and answers
// them the way the real endpoint does.
type leaseRecorder struct {
	method string
	body   map[string]any
}

func cannedLeaseFleetd(t *testing.T, rec *leaseRecorder) *httptest.Server {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		rec.body = map[string]any{}
		_ = json.Unmarshal(raw, &rec.body)
		if r.Method == http.MethodDelete {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
			return
		}
		_ = json.NewEncoder(w).Encode(fleetapi.Lease{
			Cell: "gpu-cell", Model: "glm-5", Holder: fleetapi.HoldHolder,
			Hold: true, Note: "evaluating", ExpiresAt: time.Now().UTC().Add(3 * time.Hour),
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/lease", h)
	mux.HandleFunc("DELETE /api/fleet/lease", h)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestCellHoldPostsAHoldAndReportsTheRegistrysExpiry(t *testing.T) {
	rec := &leaseRecorder{}
	ts := cannedLeaseFleetd(t, rec)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCellHold(t.Context(), &out, target, "gpu-cell", "glm-5", "evaluating", 3*time.Hour, false); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if rec.method != http.MethodPost {
		t.Errorf("method = %s, want POST", rec.method)
	}
	if rec.body["holder"] != fleetapi.HoldHolder || rec.body["hold"] != true || rec.body["ttl"] != "3h0m0s" {
		t.Errorf("body = %+v, want a hold under the reserved holder with the ttl", rec.body)
	}
	s := out.String()
	for _, want := range []string{"held glm-5 on gpu-cell", "not a pin", "warm schedules and probes"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestCellHoldReleaseDeletesTheSameKey(t *testing.T) {
	rec := &leaseRecorder{}
	ts := cannedLeaseFleetd(t, rec)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCellHold(t.Context(), &out, target, "gpu-cell", "glm-5", "", 0, true); err != nil {
		t.Fatalf("release: %v", err)
	}
	if rec.method != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", rec.method)
	}
	if rec.body["holder"] != fleetapi.HoldHolder || rec.body["model"] != "glm-5" {
		t.Errorf("body = %+v, want the three-field hold key", rec.body)
	}
	if _, ok := rec.body["ttl"]; ok {
		t.Errorf("release sent a ttl: %+v", rec.body)
	}
	if !strings.Contains(out.String(), "hold released") {
		t.Errorf("output = %q", out.String())
	}
}

// TestCellHoldRejectsAnUnboundedHold: the flag path enforces the same
// bound the registry does, so an operator gets the reason locally
// instead of a 400.
func TestCellHoldRejectsAnUnboundedHold(t *testing.T) {
	cmd := cellHoldCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"gpu-cell", "glm-5", "--for", "48h"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "24h") {
		t.Fatalf("err = %v, want the 24h bound", err)
	}
}

// TestCellStatusShowsAHoldWithItsRemainingTime: the intent column is
// where an operator asks "why is this cell like this".
func TestCellStatusShowsAHoldWithItsRemainingTime(t *testing.T) {
	ts := cannedFleetd(t, func() fleetapi.StateSnapshot {
		return statusState(fleetapi.CellSnapshot{
			Name: "gpu-cell", URL: "http://gpu.lan:9000", Class: "opportunistic",
			Reachable: true, Display: fleetapi.DisplayServing,
			Models: []fleetapi.ModelState{{ID: "glm-5", State: "ready"}},
			Leases: []fleetapi.Lease{{
				Cell: "gpu-cell", Model: "glm-5", Holder: fleetapi.HoldHolder, Hold: true,
				Note: "evaluating", ExpiresAt: time.Now().Add(90 * time.Minute),
			}},
		})
	})
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := target.fetchState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderStatus(&out, target.base, snap)
	s := out.String()
	for _, want := range []string{"held: glm-5", "1h30m", "left", "evaluating"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}
