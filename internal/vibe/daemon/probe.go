package daemon

import (
	"log/slog"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// minProbeInterval is the floor on a probe target's period. Probing is
// the one place the control plane deliberately generates inference load,
// and below five minutes it stops being a health check and becomes a
// background job competing with the operator. The cell's own cooldown
// (modelprobe.minGap) is the same figure and is the real bound — this
// one just stops a typo'd config from filling the command queue.
const minProbeInterval = 5 * time.Minute

// startProbeLoop wires the C8 probe scheduler from the daemon config.
// Config errors are loud at startup (a typo'd target is an operator bug)
// and never stop the registry coming up.
func (d *Daemon) startProbeLoop(cfg Config, hosts *fleetcfg.File) {
	targets := probeTargets(cfg, hosts)
	if len(targets) == 0 {
		return
	}
	d.fleet.StartProbeLoop(targets)
}

// probeTargets validates and clamps the declared targets. Split from the
// wiring so the rules are testable without a running daemon.
func probeTargets(cfg Config, hosts *fleetcfg.File) []fleetapi.ProbeTarget {
	if len(cfg.ProbeTargets) == 0 {
		return nil
	}
	var targets []fleetapi.ProbeTarget
	for _, pt := range cfg.ProbeTargets {
		if pt.Cell == "" || pt.Model == "" {
			slog.Warn("probe target needs cell + model; skipped", "cell", pt.Cell, "model", pt.Model)
			continue
		}
		if _, ok := hosts.Cells[pt.Cell]; !ok {
			slog.Warn("probe target names an unknown cell; skipped", "cell", pt.Cell, "model", pt.Model)
			continue
		}
		if pt.Cell == fleetcfg.FrontCell {
			// The front holds no models of its own (its rendered config is
			// peers-only), so a probe there would measure the peer through
			// the front — the exact confounded measurement C8 §1 rejects.
			slog.Warn("probe target names the front cell; skipped (the front serves no models of its own)", "model", pt.Model)
			continue
		}
		every, err := time.ParseDuration(pt.Every)
		if err != nil || every <= 0 {
			slog.Warn("probe target has invalid every; skipped", "cell", pt.Cell, "model", pt.Model, "value", pt.Every)
			continue
		}
		// Clamp, never skip (the warm targets' precedent): a too-eager
		// target is a working policy visible in fleet_status, while a
		// skipped one is silently absent.
		if every < minProbeInterval {
			slog.Warn("probe interval below the floor; clamped", "cell", pt.Cell, "model", pt.Model,
				"value", pt.Every, "clamped_to", minProbeInterval)
			every = minProbeInterval
		}
		targets = append(targets, fleetapi.ProbeTarget{Cell: pt.Cell, Model: pt.Model, Every: every})
	}
	return targets
}
