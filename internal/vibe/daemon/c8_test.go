package daemon

// C8: the probe scheduler's config wiring. Probing is the one place the
// control plane deliberately spends GPU time, so the rules that bound it
// are pinned here rather than left to review.

import (
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

func probeHosts() *fleetcfg.File {
	return &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		"front":    {URL: "http://front.test"},
		"gpu-cell": {URL: "http://gpu.test"},
	}}
}

// TestProbeTargets_ClampsBelowTheFloorRatherThanSkipping follows the
// warm targets' precedent: a too-eager target is a working policy
// visible in fleet_status, a skipped one is silently absent.
func TestProbeTargets_ClampsBelowTheFloorRatherThanSkipping(t *testing.T) {
	got := probeTargets(Config{ProbeTargets: []ProbeTarget{
		{Cell: "gpu-cell", Model: "qwen", Every: "10s"},
	}}, probeHosts())
	if len(got) != 1 {
		t.Fatalf("a below-floor interval was skipped instead of clamped: %+v", got)
	}
	if got[0].Every != minProbeInterval {
		t.Fatalf("every = %s, want the %s floor", got[0].Every, minProbeInterval)
	}
}

// TestProbeTargets_SkipsUnknownCellsMalformedIntervalsAndTheFront.
func TestProbeTargets_SkipsUnknownCellsMalformedIntervalsAndTheFront(t *testing.T) {
	got := probeTargets(Config{ProbeTargets: []ProbeTarget{
		{Cell: "nope", Model: "qwen", Every: "6h"},
		{Cell: "gpu-cell", Model: "qwen", Every: "banana"},
		{Cell: "gpu-cell", Model: "qwen", Every: "-1h"},
		{Cell: "gpu-cell", Model: "", Every: "6h"},
		// The front serves no models of its own — its rendered config is
		// peers-only — so a probe there measures a peer THROUGH the front,
		// the confounded measurement C8 §1 rejects.
		{Cell: fleetcfg.FrontCell, Model: "qwen", Every: "6h"},
		{Cell: "gpu-cell", Model: "qwen", Every: "6h"},
	}}, probeHosts())
	if len(got) != 1 {
		t.Fatalf("want exactly the one valid target, got %+v", got)
	}
	if got[0].Cell != "gpu-cell" || got[0].Every != 6*time.Hour {
		t.Fatalf("surviving target = %+v", got[0])
	}
}

// TestProbeTargets_EmptyConfigProbesNothing: no entries means no probing
// at all, and that is the default. Declared, never implicit.
func TestProbeTargets_EmptyConfigProbesNothing(t *testing.T) {
	if got := probeTargets(Config{}, probeHosts()); len(got) != 0 {
		t.Fatalf("an unconfigured daemon scheduled probes: %+v", got)
	}
}
