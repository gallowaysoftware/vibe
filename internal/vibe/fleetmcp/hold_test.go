package fleetmcp

// C11 coverage for the facade: the two hold verbs an agent actually
// calls, and the sentence the reply must never lose.

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// TestDrainReportNamesAHoldAsAHold: the same fix the CLI prompt got, on
// the agent's copy of the pre-drain report. The reserved holder is the
// key — that is what reserving it buys.
func TestDrainReportNamesAHoldAsAHold(t *testing.T) {
	exp := timestamppb.New(time.Now().Add(time.Hour))
	held := leaseLine(&vibev1.LeaseView{Model: "glm-5", Holder: fleetapi.HoldHolder, Note: "evaluating", ExpiresAt: exp})
	if !strings.HasPrefix(held, "HELD: glm-5") || strings.Contains(held, "hold holds") {
		t.Errorf("hold rendered as %q", held)
	}
	plain := leaseLine(&vibev1.LeaseView{Model: "qwen", Holder: "batch", ExpiresAt: exp})
	if !strings.Contains(plain, "batch holds qwen") || !strings.Contains(plain, "(no note)") {
		t.Errorf("ordinary lease rendering changed: %q", plain)
	}
}

func TestMCPHoldModel(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: cellSrv.srv.URL},
		"gpu-cell": {URL: cellSrv.srv.URL, Class: fleetcfg.ClassOpportunistic},
	}, nil)

	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hold_model",
		"arguments":{"cell":"gpu-cell","model":"glm-5","for":"3h","note":"evaluating"}}}`))
	if isErr {
		t.Fatalf("hold_model failed: %s", text)
	}
	for _, want := range []string{"Held glm-5 on gpu-cell", "not a pin", "release_hold"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply %q missing %q", text, want)
		}
	}

	// Read it back the way an agent would, through fleet_status.
	status, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}`))
	if isErr {
		t.Fatalf("fleet_status failed: %s", status)
	}
	if !strings.Contains(status, `"hold":true`) || !strings.Contains(status, `"model":"glm-5"`) {
		t.Errorf("fleet_status does not carry the hold: %s", status)
	}

	rel, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"release_hold",
		"arguments":{"cell":"gpu-cell","model":"glm-5"}}}`))
	if isErr || !strings.Contains(rel, "Released the hold") {
		t.Errorf("release_hold = %q (isErr=%v)", rel, isErr)
	}
	// Releasing nothing must not claim it undid something.
	again, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"release_hold",
		"arguments":{"cell":"gpu-cell","model":"glm-5"}}}`))
	if isErr || !strings.Contains(again, "No active hold") {
		t.Errorf("second release_hold = %q (isErr=%v)", again, isErr)
	}
}

func TestMCPHoldModelRefusals(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
	_, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: cellSrv.srv.URL},
		"gpu-cell": {URL: cellSrv.srv.URL, Class: fleetcfg.ClassOpportunistic},
	}, nil)

	for _, tc := range []struct{ name, args, want string }{
		{"unknown cell", `{"cell":"laptop","model":"m","for":"1h"}`, "unknown cell"},
		{"the front cell", `{"cell":"front","model":"m","for":"1h"}`, "no models of its own"},
		{"beyond the bound", `{"cell":"gpu-cell","model":"m","for":"48h"}`, "24h"},
		{"unparseable duration", `{"cell":"gpu-cell","model":"m","for":"an afternoon"}`, "Go duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hold_model","arguments":`+tc.args+`}}`))
			if !isErr {
				t.Fatalf("hold_model accepted %s: %s", tc.args, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("error %q, want it to mention %q", text, tc.want)
			}
		})
	}
}

// TestMCPHoldDoesNotBlockExplicitVerbs is gate 8's other half, which had
// no test: a hold suspends what fleetd INITIATES, and an operator asking
// is not fleetd guessing. unload_model on the held model itself is the
// sharpest case — it is the operator undoing their own evaluation, and
// the control plane must not second-guess that.
func TestMCPHoldDoesNotBlockExplicitVerbs(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
	f, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: cellSrv.srv.URL},
		"gpu-cell": {URL: cellSrv.srv.URL, Class: fleetcfg.ClassOpportunistic},
	}, nil)
	if _, err := f.fleet.SetHold("gpu-cell", "qwen3.6-27b", "evaluating", time.Hour); err != nil {
		t.Fatalf("SetHold: %v", err)
	}

	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"unload_model",
		"arguments":{"cell":"gpu-cell","model":"qwen3.6-27b"}}}`))
	if isErr {
		t.Fatalf("unload_model refused on a held cell: %s", text)
	}
	if cellSrv.unloaded.Load() != "qwen3.6-27b" {
		t.Errorf("the cell never saw the unload: %v", cellSrv.unloaded.Load())
	}
	if _, held := f.fleet.HoldOn("gpu-cell"); !held {
		t.Error("an explicit verb cleared the hold — a hold expires or is released, nothing else")
	}
}

// TestMCPHoldModelDefaultsToAnAfternoon: the backlog entry's unit. An
// agent calling hold_model with no duration must get a bounded hold, not
// an error and not an open-ended one.
func TestMCPHoldModelDefaultsToAnAfternoon(t *testing.T) {
	cellSrv := newFakeLlamaSwap(t, "qwen3.6-27b")
	f, ts := newTestFacade(t, map[string]fleetcfg.Cell{
		"front":    {URL: cellSrv.srv.URL},
		"gpu-cell": {URL: cellSrv.srv.URL, Class: fleetcfg.ClassOpportunistic},
	}, nil)
	text, isErr := toolText(t, rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hold_model",
		"arguments":{"cell":"gpu-cell","model":"glm-5"}}}`))
	if isErr {
		t.Fatalf("hold_model failed: %s", text)
	}
	l, held := f.fleet.HoldOn("gpu-cell")
	if !held {
		t.Fatal("no hold recorded")
	}
	left := time.Until(l.ExpiresAt)
	if left <= 3*time.Hour+50*time.Minute || left > fleetapi.DefaultHoldFor {
		t.Errorf("default hold window = %s, want ~%s", left, fleetapi.DefaultHoldFor)
	}
	if !strings.Contains(text, "4h") && !strings.Contains(text, "3h59m") {
		t.Errorf("reply %q does not state the default 4h window", text)
	}
}
