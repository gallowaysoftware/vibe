package daemon

import (
	"context"
	"log/slog"
	"sync"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"github.com/gallowaysoftware/vibe/internal/vibe/usagemeter"
)

// Actual cloud spend (fleet-control C7b §6). fleetd tails the FRONT's
// activity log for cloud_peer model ids only and prices them at the real
// model's real rate — a bill reconstruction, not a counterfactual.
//
// It sits beside the notional savings at the same type size because it
// is the induced-demand control: notional savings are a counterfactual
// that never happened, actual spend is a fact, and if both rose together
// the fleet added work rather than replacing it.
//
// The front is the only place this traffic is visible: cloud peers are
// served by no cell, so nothing in the announce path ever sees them.

// startCloudSpendLoop wires the front tailer. Absent cloud_peer defs, an
// unreadable defs dir or an activity store the front does not keep all
// degrade to the same thing: no cloud line, rendered as "not measured"
// rather than as $0.
func (d *Daemon) startCloudSpendLoop(hosts *fleetcfg.File) {
	front, ok := hosts.Cells[fleetcfg.FrontCell]
	if !ok || front.URL == "" {
		return
	}
	poll := cloudSpendPoller(front.URL, paths.FrontCloudUsageFile(), paths.BackendsDir())
	if poll == nil {
		return
	}
	d.fleet.StartCloudSpendLoop(poll)
}

// cloudSpendPoller builds the polling closure. Exposed as a package
// function (not a method) so it is testable without a whole daemon.
func cloudSpendPoller(frontURL, statePath, backendsDir string) func(context.Context) *fleetapi.AnnounceUsage {
	var mu sync.Mutex
	ids := map[string]bool{}
	coll, err := usagemeter.New(usagemeter.Config{
		LlamaSwapURL: frontURL,
		StatePath:    statePath,
		ModelFilter: func(model string) bool {
			mu.Lock()
			defer mu.Unlock()
			return ids[model]
		},
	})
	if err != nil {
		slog.Warn("cloud spend collector not started; actual cloud spend renders as not measured", "err", err)
		return nil
	}
	warned := false
	return func(ctx context.Context) *fleetapi.AnnounceUsage {
		next, err := cloudPeerModelIDs(backendsDir)
		if err != nil {
			// A defs read that failed cannot tell cloud models from local
			// ones, and folding unfiltered would count every cell's traffic
			// a second time under the front. Skip the whole poll: the
			// counters are cumulative, so the next successful one carries
			// the arrears.
			if !warned {
				warned = true
				slog.Warn("backend defs unreadable; skipping cloud spend polls until they parse", "err", err)
			}
			return nil
		}
		warned = false
		if len(next) == 0 {
			return nil
		}
		mu.Lock()
		ids = next
		mu.Unlock()
		return coll.PollAndSnapshot(ctx)
	}
}

// cloudPeerModelIDs is every model id served by a cloud_peer def. These
// are the ONLY ids the front tailer counts: every other row on the
// front's log is a request some cell also counted, and folding those
// would double the fleet.
func cloudPeerModelIDs(backendsDir string) (map[string]bool, error) {
	defs, err := router.LoadDefs(backendsDir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, def := range defs {
		if def.Backend.CloudPeer == nil {
			continue
		}
		for _, m := range def.Backend.CloudPeer.Models {
			out[m] = true
		}
	}
	return out, nil
}
