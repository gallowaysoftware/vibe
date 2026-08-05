package fleetapi

// The C9 adversarial review pass (ground rule 9). Each test below fails
// against the code as first written; the production line each one pins is
// named in its comment.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
)

// notifyDriftFleet stands up the real render loop over one strict def
// owned by `gpu`, announces a mismatched hash, and returns once the
// mismatch set holds it.
//
// `gpu` is OPPORTUNISTIC on purpose. Class policy prunes a roaming
// cell's defs on stale, so byName no longer resolves the model and the
// mismatch drops out for a reason that has nothing to do with the rule
// under test — a roaming fleet would pass this test against the bug. The
// hold classes (always_on, opportunistic) keep their defs rendered, and
// they are exactly where the bug lives.
func notifyDriftFleet(t *testing.T) *Server {
	t.Helper()
	strictDef := llmDef("embed-x", "gpu", "strict")
	cells := []Cell{
		{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"},
		{Name: "gpu", URL: "http://127.0.0.1:3", Class: "opportunistic"},
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front": {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu":   {URL: "http://127.0.0.1:3", Class: fleetcfg.ClassOpportunistic},
	}}
	probe := newRenderProbe(strictDef)
	cfg := RenderLoopConfig{FullWaveTimeout: 30 * time.Second, RenderMinInterval: time.Millisecond}
	cfg.Hosts = hosts
	cfg.FrontConfigPath = filepath.Join(t.TempDir(), "front.yaml")
	cfg.LoadDefs = probe.loadDefs
	cfg.Render = probe.render
	cfg.WriteFile = probe.writeFile
	s := New(cells, filepath.Join(t.TempDir(), "hist.json"), testDaemonInfo, Options{})
	t.Cleanup(s.Close)
	s.StartRenderLoop(cfg)

	rlAnnounce(t, s, "front", rlServing(), nil)
	rlAnnounce(t, s, "gpu", rlServing(), []AnnounceModel{
		{ID: "embed-x", State: "ready", FlagsSHA256: strings.Repeat("0", 64)},
	})
	waitUntil(t, func() bool { return len(s.FingerprintMismatches()) == 1 })
	return s
}

// TestRenderPass_AStaleCellLeavesTheFingerprintMismatchSet is the
// review's headline finding. Presence.Announcing stays TRUE through
// staleness and a clean withdraw — it means "has ever announced" — so
// applyFingerprints kept re-recording a powered-off HOLD-class cell's
// last-announced hash out of its retained model list. The 15-minute
// persistence dwell then paged about serving-flag drift on a box that was
// serving nothing: an always_on outage paged twice, and an OPPORTUNISTIC
// workstation being switched off — the class table's "absence means off,
// alarm? no" — paged every time it went down with drift outstanding.
// C8's probe.degraded roll-up already draws this line for the same
// reason: a stale announce is history, not evidence of what a cell is
// serving right now.
func TestRenderPass_AStaleCellLeavesTheFingerprintMismatchSet(t *testing.T) {
	s := notifyDriftFleet(t)
	markStale(t, s, "gpu")
	waitUntil(t, func() bool { return len(s.FingerprintMismatches()) == 0 })
	if got := kinds(s.notifyConditions(StateSnapshot{})); len(got) != 0 {
		t.Fatalf("a stale cell still raised a drift condition: %v", got)
	}
}

// TestRenderPass_AWithdrawnCellLeavesTheFingerprintMismatchSet is the
// same rule on the clean-shutdown path: a cell that said goodbye is not
// serving mismatched flags, and Presence.Withdrawn does not clear
// Announcing either.
func TestRenderPass_AWithdrawnCellLeavesTheFingerprintMismatchSet(t *testing.T) {
	s := notifyDriftFleet(t)
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 2,
		Intent: &AnnounceIntent{State: "withdrawing", Since: time.Now()},
		Models: []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: strings.Repeat("0", 64)}},
	})
	waitUntil(t, func() bool { return len(s.FingerprintMismatches()) == 0 })
}

// TestRenderPass_AFreshCellKeepsItsMismatch is the positive control: the
// freshness filter must not become "never alarm".
func TestRenderPass_AFreshCellKeepsItsMismatch(t *testing.T) {
	s := notifyDriftFleet(t)
	s.recordAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 2, Intent: rlServing(),
		Models: []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: strings.Repeat("1", 64)}},
	})
	waitUntil(t, func() bool {
		got := s.FingerprintMismatches()
		return len(got) == 1 && got[0].Got == strings.Repeat("1", 64)
	})
	if got := kinds(s.notifyConditions(StateSnapshot{})); len(got) != 1 {
		t.Fatalf("a fresh mismatch stopped raising its condition: %v", got)
	}
}

// TestNotifyConditions_AHoldIsNamedAsAHoldNotAsAnAdvisoryLease: C11 put
// holds in the SAME lease store and made the reserved holder the
// deterministic test, which cli.printDrainReport and fleetmcp.leaseLine
// both key on. C9 landed a third lease renderer that did not, so a drain
// on a held cell paged "hold holds qwen3.6-27b ... 1 advisory lease(s)".
// Both the noun and the consequence are wrong, and the consequence is the
// whole point: a drain evicts a held model regardless.
func TestNotifyConditions_AHoldIsNamedAsAHoldNotAsAnAdvisoryLease(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}})
	if _, err := s.SetIntent("gpu-cell", "drained", "gaming", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHold("gpu-cell", "qwen3.6-27b", "evaluating glm-5", time.Hour); err != nil {
		t.Fatal(err)
	}
	cell := snapCell(s, "gpu-cell", false, boolp(true))
	conds := s.notifyConditions(StateSnapshot{Cells: []CellSnapshot{cell}})
	if len(conds) != 1 || conds[0].Kind != fleetnotify.KindDrainWithLease {
		t.Fatalf("conditions = %v", kinds(conds))
	}
	detail := conds[0].Detail
	if strings.Contains(detail, HoldHolder+" holds") {
		t.Fatalf("the hold rendered as a lease holder named %q: %q", HoldHolder, detail)
	}
	if !strings.Contains(detail, "a hold on qwen3.6-27b") || !strings.Contains(detail, "evicts it") {
		t.Fatalf("the alarm does not say a hold is being overridden: %q", detail)
	}
	if strings.Contains(detail, "advisory lease") {
		t.Fatalf("a hold is not an advisory lease: %q", detail)
	}
}

// TestSendNotification_BoundsTheMessageForEveryProducer: the HTTP route
// capped an explicit message and the fleet_notify_test MCP verb — the
// door an agent actually drives — did not, so the two producers of the
// same outbound POST had different hygiene. The predicate lives on
// SendNotification now, which is what both call.
func TestSendNotification_BoundsTheMessageForEveryProducer(t *testing.T) {
	s := notifyServer(t, []Cell{{Name: "front", URL: "http://127.0.0.1:1", Class: "always_on"}})
	sink := &captureSink{}
	s.StartNotifyLoop(NotifyLoopConfig{Sink: sink, Interval: time.Hour})

	if err := s.SendNotification("vibe fleet", strings.Repeat("x", maxNotifyFieldLen+1)); err == nil {
		t.Fatal("an oversized message reached the webhook through the in-process path")
	}
	if err := s.SendNotification("vibe fleet\nX-Injected: yes", "hi"); err == nil {
		t.Fatal("a control-character title reached the webhook through the in-process path")
	}
	if err := s.SendNotification("vibe fleet", "line one\nline two"); err != nil {
		t.Fatalf("a legitimate multiline body was refused: %v", err)
	}
	waitUntil(t, func() bool { return len(sink.sent()) == 1 })
}

// TestAnnounce_ModelFlagsSHAGetsTheSameHygieneAsItsSiblings: the sending
// side guards and the receiving side did not — the recurring defect class
// in this repo. Every other announce string that reaches a status surface
// runs through clean(), including probe.flags_sha256 on this same model;
// the model-level flags_sha256 escaped, and C9 routes it into an alarm's
// detail line on top of the presence document and the mismatch event.
func TestAnnounce_ModelFlagsSHAGetsTheSameHygieneAsItsSiblings(t *testing.T) {
	for _, tc := range []struct {
		name string
		sha  string
	}{
		{"control character", "abc\r\ndef"},
		{"oversized", strings.Repeat("a", maxAnnounceFieldLen+1)},
	} {
		err := validateAnnounce(&AnnounceRequest{
			V: AnnounceVersion, Cell: "gpu", Seq: 1,
			Models: []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: tc.sha}},
		})
		if err == nil {
			t.Errorf("%s: an unhygienic flags_sha256 was accepted", tc.name)
		}
	}
	if err := validateAnnounce(&AnnounceRequest{
		V: AnnounceVersion, Cell: "gpu", Seq: 1,
		Models: []AnnounceModel{{ID: "embed-x", State: "ready", FlagsSHA256: strings.Repeat("0", 64)}},
	}); err != nil {
		t.Fatalf("a real hash was rejected: %v", err)
	}
}

// TestNotifyEvaluator_UsesAMonotonicClock: every dwell and the rate
// bucket are computed with Sub on the timestamps evalNotify hands the
// tracker, and time.Now().UTC() strips the monotonic reading. An NTP step
// backwards would then stall the pager — pending alarms never reaching
// their dwell, the deferral queue never draining — until wall time caught
// up, and a step forward would fire an alarm below its threshold. The
// tracker normalises to UTC on the way out instead, so the wire format is
// unchanged; that half is pinned below.
func TestNotifyEvaluator_UsesAMonotonicClock(t *testing.T) {
	now := notifyNow()
	// The documented spelling: a Time carrying a monotonic reading prints
	// a trailing " m=±<seconds>", and .UTC()/.Round(0) is what removes it.
	if !strings.Contains(now.String(), " m=") {
		t.Fatalf("the evaluator's clock lost its monotonic reading: %s", now)
	}
}

// TestNotifyStatus_TimestampsAreUTCEvenFromAMonotonicClock is the other
// half: making the dwell monotonic must not move the rendered timestamps
// off UTC, because they are read by the page, the CLI and the webhook's
// json body.
func TestNotifyStatus_TimestampsAreUTCEvenFromAMonotonicClock(t *testing.T) {
	tr := fleetnotify.NewTracker(fleetnotify.Policy{
		Dwell: map[fleetnotify.Kind]time.Duration{fleetnotify.KindCellAbsent: 0},
	})
	local := time.Now().In(time.FixedZone("nowhere", 7*3600))
	out := tr.Step(local, []fleetnotify.Condition{
		{Kind: fleetnotify.KindCellAbsent, Scope: "hum", Detail: "hum is OFF/AWAY"},
	}, false)
	if len(out) != 1 {
		t.Fatalf("setup: want one firing, got %d", len(out))
	}
	if out[0].At.Location() != time.UTC {
		t.Fatalf("notification timestamp is not UTC: %s", out[0].At)
	}
	st := tr.Status()
	if len(st.Alarms) != 1 || st.Alarms[0].Since.Location() != time.UTC {
		t.Fatalf("status timestamp is not UTC: %+v", st.Alarms)
	}
	if st.Alarms[0].FiredAt == nil || st.Alarms[0].FiredAt.Location() != time.UTC {
		t.Fatalf("fired_at is not UTC: %+v", st.Alarms)
	}
}
