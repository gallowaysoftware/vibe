package fleetapi

// C10 coverage, server half: the activity block `vibe cell await --idle`
// reads. Every test here is about one rule — fleetd may only claim
// silence it was connected to observe — because the consumer of "idle"
// is a batch job about to take the GPU.

import (
	"testing"
	"time"
)

func activityServer(t *testing.T) *Server {
	t.Helper()
	return newWarmServer(t, []Cell{{Name: "gpu-cell", URL: "http://127.0.0.1:1", Class: "opportunistic"}})
}

// backdate rewinds the observation window and the last frame so a test
// can assert on a window it did not have to sleep through.
func backdate(s *Server, cell string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.cellUpSince[cell]; ok {
		s.cellUpSince[cell] = t.Add(-d)
	}
	if t, ok := s.lastInFlightFrame[cell]; ok {
		s.lastInFlightFrame[cell] = t.Add(-d)
	}
}

// TestActivity_WithoutALiveStreamReportsNoEvidenceAndNoIdleWindow is the
// rule the phase is arranged around. A cell fleetd has no events stream
// to (announce-only membership, a dead watcher) must report the absence
// of EVIDENCE — never an idle window computed from fleetd's own uptime,
// which is the trap C5 spent a phase pulling the warm policy out of.
func TestActivity_WithoutALiveStreamReportsNoEvidenceAndNoIdleWindow(t *testing.T) {
	s := activityServer(t)

	act := s.activityFor("gpu-cell")
	if act == nil {
		t.Fatal("activity block is nil; an absent block reads as absent evidence nowhere")
	}
	if act.Observed {
		t.Error("Observed = true with no stream ever connected")
	}
	if act.IdleSeconds != nil {
		t.Errorf("IdleSeconds = %v with no observation channel, want nil", *act.IdleSeconds)
	}
	if act.Reason == "" {
		t.Error("no Reason: the missing evidence has to be nameable by the CLI")
	}
}

// TestActivity_StreamDropRetiresTheObservationWindow: C4's
// observesActivity accepts "a frame was seen once, ever", which is
// sticky forever. An idle window is a claim about NOW, so a cell whose
// stream has dropped reports no evidence again.
func TestActivity_StreamDropRetiresTheObservationWindow(t *testing.T) {
	s := activityServer(t)
	s.setCellUp("gpu-cell", true)
	s.trackInFlight("gpu-cell", inflightFrame(t))
	if act := s.activityFor("gpu-cell"); !act.Observed || act.IdleSeconds == nil {
		t.Fatalf("live stream should be observable: %+v", act)
	}

	s.setCellUp("gpu-cell", false)
	act := s.activityFor("gpu-cell")
	if act.Observed || act.IdleSeconds != nil {
		t.Errorf("a dropped stream still reports an idle window: %+v", act)
	}
	if act.LastRequest == nil {
		t.Error("the last frame is history and should still be reported")
	}
}

// TestActivity_IdleIsFlooredAtTheStreamConnectNotTheFrameHistory pins
// the reconnect case: a request that was already running when fleetd
// connected produces no frame we saw, so an hour-old frame must not
// license an hour-long idle claim on an eight-second-old connection.
func TestActivity_IdleIsFlooredAtTheStreamConnectNotTheFrameHistory(t *testing.T) {
	s := activityServer(t)
	// A completed request an hour ago, then a stream that connected just
	// now. The completion matters: an in-flight count short-circuits to
	// busy and would let this test pass without exercising the floor.
	s.trackInFlight("gpu-cell", inflightFrame(t, "qwen"))
	s.trackInFlight("gpu-cell", inflightFrame(t))
	s.mu.Lock()
	s.lastInFlightFrame["gpu-cell"] = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	s.setCellUp("gpu-cell", true)

	act := s.activityFor("gpu-cell")
	if act.IdleSeconds == nil {
		t.Fatalf("no idle window on a live stream: %+v", act)
	}
	if *act.IdleSeconds > 60 {
		t.Errorf("idle_s = %.0f: the window was measured from the frame history, not the connect — "+
			"fleetd cannot claim silence it was not watching", *act.IdleSeconds)
	}
}

// TestActivity_IdleGrowsFromTheLastFrameOnceItIsInsideTheWindow is the
// positive control for the floor test above: past the connect, the frame
// is what the window is measured from.
func TestActivity_IdleGrowsFromTheLastFrameOnceItIsInsideTheWindow(t *testing.T) {
	s := activityServer(t)
	s.setCellUp("gpu-cell", true)
	s.trackInFlight("gpu-cell", inflightFrame(t, "qwen"))
	s.trackInFlight("gpu-cell", inflightFrame(t)) // it finished
	backdate(s, "gpu-cell", 20*time.Minute)
	// The connect is older than the frame: 30 min watching, 20 min quiet.
	s.mu.Lock()
	s.cellUpSince["gpu-cell"] = time.Now().Add(-30 * time.Minute)
	s.mu.Unlock()

	act := s.activityFor("gpu-cell")
	if act.IdleSeconds == nil {
		t.Fatal("no idle window on a live stream")
	}
	if *act.IdleSeconds < 19*60 || *act.IdleSeconds > 21*60 {
		t.Errorf("idle_s = %.0f, want ~1200 (measured from the last frame)", *act.IdleSeconds)
	}
}

// TestActivity_AnyFrameIsActivityIncludingTheCompletionEdge: llama-swap
// sends inflight frames on edges, so the empty frame that closes out a
// generation is request activity on that cell — and it is the ONLY
// signal for a model that was TTL-unloaded right after.
func TestActivity_AnyFrameIsActivityIncludingTheCompletionEdge(t *testing.T) {
	s := activityServer(t)
	s.setCellUp("gpu-cell", true)
	s.trackInFlight("gpu-cell", inflightFrame(t, "qwen"))
	s.trackInFlight("gpu-cell", inflightFrame(t))
	backdate(s, "gpu-cell", time.Hour)
	if act := s.activityFor("gpu-cell"); act.IdleSeconds == nil || *act.IdleSeconds < 3000 {
		t.Fatalf("setup: want a stale window, got %+v", act)
	}

	s.trackInFlight("gpu-cell", inflightFrame(t)) // completion edge: zero requests
	act := s.activityFor("gpu-cell")
	if act.IdleSeconds == nil || *act.IdleSeconds > 60 {
		t.Errorf("a completion edge did not reset the window: %+v", act)
	}
	if act.InFlight == nil || *act.InFlight != 0 {
		t.Errorf("in_flight = %v, want a reported 0", act.InFlight)
	}
}

// TestActivity_UnreportedInFlightIsNotAReportedZero keeps C4/C8's rule
// visible on this surface: the field is absent until a frame arrives,
// and the reason says the window rests on the connect alone.
func TestActivity_UnreportedInFlightIsNotAReportedZero(t *testing.T) {
	s := activityServer(t)
	s.setCellUp("gpu-cell", true)

	act := s.activityFor("gpu-cell")
	if act.InFlight != nil {
		t.Errorf("in_flight = %d before any frame, want absent", *act.InFlight)
	}
	if act.IdleSeconds == nil {
		t.Fatal("a live stream with no frames is honest evidence of silence since the connect")
	}
	if act.Reason == "" {
		t.Error("the qualification (no frame seen yet) has to be visible")
	}
}

// TestActivity_InFlightRequestsReportBusyNotIdle: the frame that starts
// a long generation stamps once and would otherwise read as idle for the
// whole generation — C4's completion-edge finding, applied to the cell.
func TestActivity_InFlightRequestsReportBusyNotIdle(t *testing.T) {
	s := activityServer(t)
	s.setCellUp("gpu-cell", true)
	s.trackInFlight("gpu-cell", inflightFrame(t, "qwen", "bge"))
	backdate(s, "gpu-cell", time.Hour)

	act := s.activityFor("gpu-cell")
	if act.InFlight == nil || *act.InFlight != 2 {
		t.Fatalf("in_flight = %v, want 2", act.InFlight)
	}
	if act.IdleSeconds == nil || *act.IdleSeconds != 0 {
		t.Errorf("idle_s = %v with 2 in flight, want 0", act.IdleSeconds)
	}
	if act.Reason == "" {
		t.Error("busy needs a reason string; the CLI renders it")
	}
}

// TestSnapshotAlwaysCarriesAnActivityBlock: the block is emitted even
// when the answer is "no evidence". An omitted field is exactly how a
// consumer ends up reading missing evidence as idleness.
func TestSnapshotAlwaysCarriesAnActivityBlock(t *testing.T) {
	s := activityServer(t)
	snap := s.Snapshot(t.Context())
	if len(snap.Cells) != 1 {
		t.Fatalf("cells = %d", len(snap.Cells))
	}
	act := snap.Cells[0].Activity
	if act == nil {
		t.Fatal("snapshot carries no activity block for an unwatched cell")
	}
	if act.Observed || act.IdleSeconds != nil || act.Reason == "" {
		t.Errorf("unwatched cell reported as observable: %+v", act)
	}
}
