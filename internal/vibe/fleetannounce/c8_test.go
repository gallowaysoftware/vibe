package fleetannounce

// C8 coverage on the cell side of the wire: the probe verb must not
// block the heartbeat, and probe results must ride the model rows.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// TestAnnounceProbeCommand_ReturnsWhileTheProbeIsStillRunning is gate
// 6's wire half. The heartbeat is the cell's ONLY evidence of life
// (fleetd marks a cell stale at 3 intervals), so the command handler
// hands the probe off and moves on. The assertion is that the announce
// completed while the probe was demonstrably still in flight — a test
// that only timed the announce would pass against a handler that waited
// for a hook which happened to be fast.
//
// The other half — that the real prober's Start honours this contract —
// is modelprobe's TestProberStart_ReturnsWhileTheProbeIsStillRunning.
func TestAnnounceProbeCommand_ReturnsWhileTheProbeIsStillRunning(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.Commands = []fleetapi.AnnounceCommand{{Verb: "probe", Model: "qwen", Rebaseline: true}}

	release := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	rebaseline := make(chan bool, 1)
	cfg := testConfig(reg, swap)
	cfg.RunProbe = func(model string, rb bool) {
		rebaseline <- rb
		go func() {
			close(started)
			<-release
			close(finished)
		}()
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := c.announceOnce(context.Background()); err != nil {
		t.Fatalf("announceOnce: %v", err)
	}
	<-started
	select {
	case <-finished:
		t.Fatal("the announce waited for the probe to finish")
	default:
	}
	close(release)
	<-finished
	if rb := <-rebaseline; !rb {
		t.Fatal("the rebaseline flag did not reach the prober")
	}
}

// TestAnnounceProbeCommand_WithoutAProberIsANoOp: a cell with no prober
// configured (a slim announcer that could not construct one) must log
// and continue, not crash the loop.
func TestAnnounceProbeCommand_WithoutAProberIsANoOp(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.Commands = []fleetapi.AnnounceCommand{{Verb: "probe", Model: "qwen"}}
	c, err := New(testConfig(reg, swap))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(context.Background()); err != nil {
		t.Fatalf("announceOnce: %v", err)
	}
	swap.mu.Lock()
	defer swap.mu.Unlock()
	if len(swap.warmed) != 0 {
		t.Fatalf("a probe command fell through to a warm: %v", swap.warmed)
	}
}

// TestAnnounce_ProbeBlocksRideTheModelRows: the reserved per-model field
// C3 §1 set aside is where the result travels, and a cell with no
// prober still announces `probe: null` exactly as v1 did.
func TestAnnounce_ProbeBlocksRideTheModelRows(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[{"model":"qwen","state":"ready"}]}`)

	cfg := testConfig(reg, swap, testDef("qwen", ""))
	cfg.IntentPath = filepath.Join(t.TempDir(), "cell-intent.json")
	silent, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := silent.announceOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := reg.calls()
	if len(got[0].Models) != 1 || got[0].Models[0].Probe != nil {
		t.Fatalf("a cell with no prober must announce a null probe block: %+v", got[0].Models)
	}

	cfg.Probes = func() map[string]*fleetapi.AnnounceProbe {
		return map[string]*fleetapi.AnnounceProbe{"qwen": {
			Kind: "chat", Metric: "decode_tok_s", Value: 12, BaselineP50: 40,
			Ratio: 0.3, Verdict: fleetapi.VerdictDegraded,
		}}
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := reg.calls()
	last := calls[len(calls)-1]
	if len(last.Models) != 1 || last.Models[0].Probe == nil {
		t.Fatalf("probe block did not reach the registry: %+v", last.Models)
	}
	if v := last.Models[0].Probe.Verdict; v != fleetapi.VerdictDegraded {
		t.Fatalf("verdict on the wire = %q", v)
	}
}
