package daemon

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// startWarmLoops wires the C4 warm policy: targets and cron schedule
// from the daemon config, warmed through the front cell. Config errors
// are loud at startup (a typo'd policy is an operator bug, not a
// runtime condition) but never stop the registry from coming up.
func (d *Daemon) startWarmLoops(cfg Config, hosts *fleetcfg.File) {
	if len(cfg.WarmTargets) == 0 && len(cfg.WarmSchedule) == 0 {
		return
	}
	front, ok := hosts.Cells[fleetcfg.FrontCell]
	if !ok {
		slog.Warn("warm policy configured but no front cell in hosts.yaml; loops disabled")
		return
	}

	if len(cfg.WarmTargets) > 0 {
		var targets []fleetapi.WarmTarget
		for _, wt := range cfg.WarmTargets {
			dur, err := time.ParseDuration(wt.RestoreAfterIdle)
			if err != nil || dur <= 0 {
				slog.Warn("warm target has invalid restore_after_idle; skipped", "cell", wt.Cell, "model", wt.Model, "value", wt.RestoreAfterIdle)
				continue
			}
			if _, ok := hosts.Cells[wt.Cell]; !ok {
				slog.Warn("warm target names unknown cell; skipped", "cell", wt.Cell, "model", wt.Model)
				continue
			}
			targets = append(targets, fleetapi.WarmTarget{Cell: wt.Cell, Model: wt.Model, RestoreAfterIdle: dur})
		}
		d.fleet.StartWarmLoop(targets, front.URL)
	}

	if len(cfg.WarmSchedule) > 0 {
		cellOfModel := func(model string) string {
			defs, err := router.LoadDefs(paths.BackendsDir())
			if err != nil {
				return ""
			}
			for _, def := range defs {
				if def.Name == model {
					return def.Cell
				}
			}
			return ""
		}
		var entries []fleetapi.WarmScheduleEntry
		for _, e := range cfg.WarmSchedule {
			if e.Cron == "" || e.Model == "" {
				slog.Warn("warm_schedule entry needs cron + model; skipped", "entry", fmt.Sprintf("%+v", e))
				continue
			}
			entries = append(entries, fleetapi.WarmScheduleEntry{Cron: e.Cron, Model: e.Model})
		}
		d.fleet.StartScheduleLoop(entries, cellOfModel, nil, front.URL)
	}
}
