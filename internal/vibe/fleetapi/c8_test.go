package fleetapi

// C8 coverage: the probe scheduler's guards, the wire block's ingest
// hardening, verdict transition events, and the invariant the phase
// exists to protect — a degraded MODEL never moves the CELL's
// availability, intent or render.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func probeBlock(verdict string, value, p50 float64) *AnnounceProbe {
	ratio := 0.0
	if p50 > 0 {
		ratio = value / p50
	}
	return &AnnounceProbe{
		Kind: "chat", Spec: "chat/v1:64out", At: time.Now().UTC(),
		Metric: "decode_tok_s", Value: value, BaselineP50: p50,
		Samples: 10, Ratio: ratio, Verdict: verdict,
	}
}

func probeServer(t *testing.T) *Server {
	t.Helper()
	s := New([]Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}},
		t.TempDir()+"/hist.json", testDaemonInfo, Options{})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s
}

// readyPresence announces one ready model and reports an idle cell, so
// every guard passes and a test only has to break the one it is about.
func readyPresence(s *Server, t *testing.T, model string) {
	t.Helper()
	presenceOf(s, "gpu-cell", AnnounceModel{ID: model, State: "ready"})
	s.trackInFlight("gpu-cell", inflightFrame(t))
}

// peekQueued reads the pending command queue WITHOUT draining it —
// warmqueue_test's queuedFor hands the batch over (at-least-once
// delivery), which would hide a second queued probe behind the redeliver
// of the first.
func peekQueued(s *Server, cell string) []AnnounceCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AnnounceCommand{}, s.commands[cell]...)
}

// TestProbeSchedule_QueuesOneProbeWhenEveryGuardPasses is the positive
// control: without it every guard test below could pass on a scheduler
// that never queues anything.
func TestProbeSchedule_QueuesOneProbeWhenEveryGuardPasses(t *testing.T) {
	s := probeServer(t)
	readyPresence(s, t, "qwen")
	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}

	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	cmds := peekQueued(s, "gpu-cell")
	if len(cmds) != 1 || cmds[0].Verb != "probe" || cmds[0].Model != "qwen" {
		t.Fatalf("want one queued probe, got %+v", cmds)
	}
	if st.State != "requested" || st.LastAsk == nil {
		t.Fatalf("status did not record the ask: %+v", st)
	}

	// Due-time gating: a second evaluation inside the interval must not
	// re-ask (the queue is at-least-once; asking twice spends GPU time).
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 1 {
		t.Fatalf("re-asked inside the interval: %+v", cmds)
	}
}

// TestProbeSchedule_SkipsADrainedCell: the operator reclaimed the box.
// Spending its GPU on a measurement is the eviction fight C4 §1 exists
// to prevent, in a new costume.
func TestProbeSchedule_SkipsADrainedCell(t *testing.T) {
	s := probeServer(t)
	readyPresence(s, t, "qwen")
	s.mu.Lock()
	s.intents = map[string]Intent{"gpu-cell": {State: "drained", Since: time.Now().UTC().Add(time.Hour)}}
	s.mu.Unlock()

	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 0 {
		t.Fatalf("queued a probe for a drained cell: %+v", cmds)
	}
	if st.State != "skipped" || !strings.Contains(st.LastSkip, "drained") {
		t.Fatalf("skip reason not visible in fleet_status: %+v", st)
	}
}

// TestProbeSchedule_SkipsWhenInFlightIsUnreported is C5's rule carried
// forward: a guard that cannot be EVALUATED is a skip. Unknown in-flight
// is not zero in-flight.
func TestProbeSchedule_SkipsWhenInFlightIsUnreported(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready"})
	// Deliberately no inflight frame.

	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 0 {
		t.Fatalf("probed a cell whose in-flight count was never reported: %+v", cmds)
	}
	if !strings.Contains(st.LastSkip, "in-flight unknown") {
		t.Fatalf("want the missing evidence named, got %q", st.LastSkip)
	}
}

// TestProbeSchedule_SkipsABusyCell: real work outranks a measurement.
func TestProbeSchedule_SkipsABusyCell(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready"})
	s.trackInFlight("gpu-cell", inflightFrame(t, "qwen"))

	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 0 {
		t.Fatalf("probed a busy cell: %+v", cmds)
	}
	if !strings.Contains(st.LastSkip, "in-flight") {
		t.Fatalf("want the in-flight count in the skip, got %q", st.LastSkip)
	}
}

// TestProbeSchedule_SkipsACellWithAnActiveLease: a batch consumer holds
// this box; the pre-drain report exists for exactly this and so does
// this guard.
func TestProbeSchedule_SkipsACellWithAnActiveLease(t *testing.T) {
	s := probeServer(t)
	readyPresence(s, t, "qwen")
	s.mu.Lock()
	s.leases = map[string]Lease{
		leaseKey("gpu-cell", "qwen", "batch"): {Cell: "gpu-cell", Model: "qwen", Holder: "batch",
			ExpiresAt: time.Now().Add(time.Hour)},
	}
	s.mu.Unlock()

	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 0 {
		t.Fatalf("probed a leased cell: %+v", cmds)
	}
	if !strings.Contains(st.LastSkip, "lease") {
		t.Fatalf("want the lease named in the skip, got %q", st.LastSkip)
	}
}

// TestProbeSchedule_SkipsAModelThatIsNotResident is the load rule's
// fleetd-side half: this check is what keeps the command queue from
// filling with probes the cell will refuse. The AUTHORITATIVE refusal is
// the cell's (modelprobe), and it is tested there.
func TestProbeSchedule_SkipsAModelThatIsNotResident(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "stopped"})
	s.trackInFlight("gpu-cell", inflightFrame(t))

	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 0 {
		t.Fatalf("queued a probe for a stopped model — that is a load trigger: %+v", cmds)
	}
	if !strings.Contains(st.LastSkip, "not resident") {
		t.Fatalf("want a residency skip, got %q", st.LastSkip)
	}
}

// TestProbeSchedule_SkipsAStaleCellAndOneThatNeverAnnounced.
func TestProbeSchedule_SkipsAStaleCellAndOneThatNeverAnnounced(t *testing.T) {
	s := probeServer(t)
	st := &probeTargetState{Cell: "gpu-cell", Model: "qwen"}
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if !strings.Contains(st.LastSkip, "never announced") {
		t.Fatalf("want a never-announced skip, got %q", st.LastSkip)
	}

	readyPresence(s, t, "qwen")
	s.mu.Lock()
	s.presence["gpu-cell"].Stale = true
	s.mu.Unlock()
	s.evalProbeTarget(ProbeTarget{Cell: "gpu-cell", Model: "qwen", Every: time.Hour}, st)
	if cmds := peekQueued(s, "gpu-cell"); len(cmds) != 0 {
		t.Fatalf("queued a probe for a stale cell: %+v", cmds)
	}
	if !strings.Contains(st.LastSkip, "stale") {
		t.Fatalf("want a staleness skip, got %q", st.LastSkip)
	}
}

// TestProbe_DegradedModelDoesNotChangeCellDisplayOrAvailability is the
// phase's central invariant (C8 §5): health is a per-MODEL state and a
// fourth thing beside the three ownership axes. A cell serving one slow
// model is SERVING, exactly as it was.
func TestProbe_DegradedModelDoesNotChangeCellDisplayOrAvailability(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 4, 40)})

	// The REAL read path (probe merge + decorate + attachProbes), not
	// decorate alone: a leak added in any of those three stages has to be
	// caught, and a test that called one of them proved only that the
	// other two were unread.
	snap := s.snapshotCell(context.Background(), Cell{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"})
	if snap.Display != DisplayServing {
		t.Fatalf("a degraded model changed the cell display to %q", snap.Display)
	}
	if !snap.Reachable {
		t.Fatal("a degraded model changed observed availability")
	}
	if snap.Intent != nil {
		t.Fatalf("a degraded model invented declared intent: %+v", snap.Intent)
	}
	// And the model row still says it is serving: degraded is a health
	// note, not a residency state.
	if len(snap.Models) != 1 || snap.Models[0].State != "ready" {
		t.Fatalf("model residency was rewritten by a probe verdict: %+v", snap.Models)
	}
}

// TestProbe_DegradedModelStaysInTheAnnouncedModelSet: the render loop
// derives the front catalog from presence.Models, so a verdict that
// removed a model here would silently 404 it fleet-wide.
func TestProbe_DegradedModelStaysInTheAnnouncedModelSet(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell",
		AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 4, 40)},
		AnnounceModel{ID: "embed-large", State: "ready"})

	p := s.presenceFor("gpu-cell")
	if len(p.Models) != 2 {
		t.Fatalf("presence dropped a model over a verdict: %+v", p.Models)
	}
	// A verdict change alone is not a membership transition either.
	drained := 0
	for {
		select {
		case <-s.renderTrigger:
			drained++
			continue
		default:
		}
		break
	}
	presenceOf(s, "gpu-cell",
		AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictOK, 40, 40)},
		AnnounceModel{ID: "embed-large", State: "ready"})
	select {
	case cell := <-s.renderTrigger:
		t.Fatalf("a verdict change triggered a front render for %s", cell)
	default:
	}
}

// TestProbeStatus_SurfacesTheVerdictOnTheModelRow: the page, the CLI and
// fleet_status all read this one field, so it is the whole display
// surface.
func TestProbeStatus_SurfacesTheVerdictOnTheModelRow(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 4, 40)})

	snap := s.snapshotCell(context.Background(), Cell{Name: "gpu-cell", URL: "http://127.0.0.1:1"})
	if len(snap.Models) != 1 {
		t.Fatalf("presence-derived model list = %+v", snap.Models)
	}
	if snap.Models[0].Probe == nil || snap.Models[0].Probe.Verdict != VerdictDegraded {
		t.Fatalf("verdict missing from the snapshot row: %+v", snap.Models[0])
	}
	rep := s.probeReport()
	if rep == nil || len(rep.Degraded) != 1 || rep.Degraded[0] != "gpu-cell/qwen" {
		t.Fatalf("fleet_status's degraded roll-up is wrong: %+v", rep)
	}
}

// TestProbeEvents_FireOnTransitionsOnly: a steady-state degraded cell
// heartbeats every 15s, and an event per heartbeat is a notification
// storm that trains the operator to ignore it.
func TestProbeEvents_FireOnTransitionsOnly(t *testing.T) {
	s := probeServer(t)
	events := make(chan Event, 32)
	s.mu.Lock()
	s.subs[events] = struct{}{}
	s.mu.Unlock()

	// Healthy first, so the degraded announce is a real transition.
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictOK, 40, 40)})
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 4, 40)})
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 5, 40)})
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictOK, 39, 40)})

	var got []string
	for {
		select {
		case ev := <-events:
			if ev.Type == EventModelDegraded || ev.Type == EventModelRecovered {
				got = append(got, ev.Type)
			}
			continue
		default:
		}
		break
	}
	if len(got) != 2 || got[0] != EventModelDegraded || got[1] != EventModelRecovered {
		t.Fatalf("want exactly one degraded then one recovered, got %v", got)
	}
}

// TestProbeEvents_LosingEvidenceIsNotARecovery: degraded → unknown means
// the last probe could not be scored, which is not the same as a good
// number coming back.
func TestProbeEvents_LosingEvidenceIsNotARecovery(t *testing.T) {
	s := probeServer(t)
	events := make(chan Event, 8)
	s.mu.Lock()
	s.subs[events] = struct{}{}
	s.mu.Unlock()

	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictDegraded, 4, 40)})
	presenceOf(s, "gpu-cell", AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictUnknown, 0, 0)})
	for {
		select {
		case ev := <-events:
			if ev.Type == EventModelRecovered {
				t.Fatal("degraded → unknown published a recovery")
			}
			continue
		default:
		}
		break
	}
}

// TestAnnounceProbe_V1NullRoundTripsUnchanged: C3 reserved this field
// and every v1 announcer sends `"probe": null`. Changing its Go type
// must not change a byte on the wire.
func TestAnnounceProbe_V1NullRoundTripsUnchanged(t *testing.T) {
	data, err := json.Marshal(AnnounceModel{ID: "qwen", State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"probe":null`) {
		t.Fatalf("v1 reservation no longer marshals as null: %s", data)
	}
	var back AnnounceModel
	if err := json.Unmarshal([]byte(`{"id":"qwen","state":"ready","probe":null}`), &back); err != nil {
		t.Fatalf("a v1 announce no longer decodes: %v", err)
	}
	if back.Probe != nil {
		t.Fatal("null decoded to something")
	}
}

// TestAnnounceProbe_UnknownKindAndMetricAreAccepted: a cell one version
// ahead must not have its whole heartbeat rejected over an accounting
// field — that would take presence and the intent echo down with it.
func TestAnnounceProbe_UnknownKindAndMetricAreAccepted(t *testing.T) {
	req := &AnnounceRequest{V: AnnounceVersion, Cell: "gpu-cell", Seq: 1,
		Models: []AnnounceModel{{ID: "qwen", State: "ready", Probe: &AnnounceProbe{
			Kind: "vision-batch", Metric: "images_per_s", Value: 3, Verdict: VerdictOK,
		}}}}
	if err := validateAnnounce(req); err != nil {
		t.Fatalf("rejected an announce over an unknown probe kind: %v", err)
	}
}

// TestAnnounceProbe_GarbageVerdictNormalizesToUnknown: the verdict is
// the one probe field that drives an event, so it is the one that gets
// an enum.
func TestAnnounceProbe_GarbageVerdictNormalizesToUnknown(t *testing.T) {
	s := probeServer(t)
	events := make(chan Event, 8)
	s.mu.Lock()
	s.subs[events] = struct{}{}
	s.mu.Unlock()

	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "gpu-cell", Seq: 1,
		Models: []AnnounceModel{{ID: "qwen", State: "ready", Probe: &AnnounceProbe{
			Verdict: "CATASTROPHIC", Value: 1,
		}}}})
	p := s.presenceFor("gpu-cell")
	if got := p.Models[0].Probe.Verdict; got != VerdictUnknown {
		t.Fatalf("verdict %q survived ingest", got)
	}
	for {
		select {
		case ev := <-events:
			if ev.Type == EventModelDegraded {
				t.Fatal("an unrecognised verdict published a degraded event")
			}
			continue
		default:
		}
		break
	}
}

// TestAnnounceProbe_NegativeNumbersAreClampedAtIngest: the announce is
// untrusted input (the fleet token is every cell's voice), and these
// numbers render straight into status surfaces.
func TestAnnounceProbe_NegativeNumbersAreClampedAtIngest(t *testing.T) {
	s := probeServer(t)
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "gpu-cell", Seq: 1,
		Models: []AnnounceModel{{ID: "qwen", State: "ready", Probe: &AnnounceProbe{
			Verdict: VerdictOK, Value: -5, BaselineP50: -1, Ratio: -2, Samples: -3, TTFTMS: -7,
		}}}})
	got := s.presenceFor("gpu-cell").Models[0].Probe
	if got.Value < 0 || got.BaselineP50 < 0 || got.Ratio < 0 || got.Samples < 0 || got.TTFTMS < 0 {
		t.Fatalf("negative counters survived ingest: %+v", got)
	}
}

// TestAnnounceProbe_OversizedStringsAreRejected keeps the existing
// announce hygiene rule (length, control characters) covering the new
// block too.
func TestAnnounceProbe_OversizedStringsAreRejected(t *testing.T) {
	req := &AnnounceRequest{V: AnnounceVersion, Cell: "gpu-cell", Seq: 1,
		Models: []AnnounceModel{{ID: "qwen", State: "ready", Probe: &AnnounceProbe{
			Verdict: VerdictOK, Note: strings.Repeat("x", maxAnnounceFieldLen+1),
		}}}}
	if err := validateAnnounce(req); err == nil {
		t.Fatal("an oversized probe note was accepted into every status surface")
	}
	req.Models[0].Probe.Note = "line\nbreak"
	if err := validateAnnounce(req); err == nil {
		t.Fatal("a control character in a probe note was accepted")
	}
}

// TestQueueCommand_AcceptsProbeAndStillRejectsJunkVerbs.
func TestQueueCommand_AcceptsProbeAndStillRejectsJunkVerbs(t *testing.T) {
	s := probeServer(t)
	readyPresence(s, t, "qwen")
	if err := s.QueueCommand("gpu-cell", AnnounceCommand{Verb: "probe", Model: "qwen"}); err != nil {
		t.Fatalf("probe verb rejected: %v", err)
	}
	if err := s.QueueCommand("gpu-cell", AnnounceCommand{Verb: "explode", Model: "qwen"}); err == nil {
		t.Fatal("an unknown verb was queued")
	}
	// The model is still validated against what the cell ANNOUNCED.
	if err := s.QueueCommand("gpu-cell", AnnounceCommand{Verb: "probe", Model: "typo"}); err == nil {
		t.Fatal("queued a probe for a model the cell does not announce")
	}
}

// TestLatestProbe_CopiesAndReportsNilForAnUnprobedModel.
func TestLatestProbe_CopiesAndReportsNilForAnUnprobedModel(t *testing.T) {
	s := probeServer(t)
	presenceOf(s, "gpu-cell",
		AnnounceModel{ID: "qwen", State: "ready", Probe: probeBlock(VerdictOK, 40, 40)},
		AnnounceModel{ID: "embed-large", State: "ready"})

	if got := s.LatestProbe("gpu-cell", "embed-large"); got != nil {
		t.Fatalf("invented a result for an unprobed model: %+v", got)
	}
	got := s.LatestProbe("gpu-cell", "qwen")
	if got == nil {
		t.Fatal("no result for a probed model")
	}
	got.Verdict = "tampered"
	if again := s.LatestProbe("gpu-cell", "qwen"); again.Verdict == "tampered" {
		t.Fatal("LatestProbe handed out a pointer into presence memory")
	}
}
