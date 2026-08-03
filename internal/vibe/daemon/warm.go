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

// minRestoreAfterIdle is the lower bound on a warm target's idle window.
// Below a minute the policy stops being "restore after the operator's
// swap goes quiet" and becomes a keep-warm timer racing llama-swap's own
// TTL — the alternative C4 §1 explicitly rejects.
const minRestoreAfterIdle = time.Minute

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

	// One defs read for the whole wiring pass: model-name validation is
	// advisory (a front-only alias is legitimate), so a defs error must
	// warn, not disable the policy.
	knownModels := map[string]bool{}
	if defs, err := router.LoadDefs(paths.BackendsDir()); err != nil {
		slog.Warn("backend defs unreadable; warm policy model names not validated", "err", err)
	} else {
		for _, def := range defs {
			knownModels[def.Name] = true
		}
	}

	if len(cfg.WarmTargets) > 0 {
		var targets []fleetapi.WarmTarget
		for _, wt := range cfg.WarmTargets {
			dur, err := time.ParseDuration(wt.RestoreAfterIdle)
			if err != nil || dur <= 0 {
				slog.Warn("warm target has invalid restore_after_idle; skipped", "cell", wt.Cell, "model", wt.Model, "value", wt.RestoreAfterIdle)
				continue
			}
			// Clamp, never skip: a too-eager warm is a working policy that
			// shows up in fleet_status, while a skipped target is silently
			// absent.
			if dur < minRestoreAfterIdle {
				slog.Warn("restore_after_idle below the floor; clamped", "cell", wt.Cell, "model", wt.Model, "value", wt.RestoreAfterIdle, "clamped_to", minRestoreAfterIdle)
				dur = minRestoreAfterIdle
			}
			if _, ok := hosts.Cells[wt.Cell]; !ok {
				slog.Warn("warm target names unknown cell; skipped", "cell", wt.Cell, "model", wt.Model)
				continue
			}
			if len(knownModels) > 0 && !knownModels[wt.Model] {
				slog.Warn("warm target names a model with no backend def (front-only alias?)", "cell", wt.Cell, "model", wt.Model)
			}
			targets = append(targets, fleetapi.WarmTarget{Cell: wt.Cell, Model: wt.Model, RestoreAfterIdle: dur})
		}
		d.fleet.StartWarmLoop(targets, front.URL)
	}

	if len(cfg.WarmSchedule) > 0 {
		// The error is load-bearing: LoadDefs fails on an unreadable dir
		// OR any one malformed YAML in it, and collapsing that into "no
		// cell" would silently convert every scheduled warm into an
		// UNGUARDED warm — the eviction fight the guard exists to prevent.
		cellOfModel := func(model string) (string, error) {
			defs, err := router.LoadDefs(paths.BackendsDir())
			if err != nil {
				return "", err
			}
			for _, def := range defs {
				if def.Name == model {
					return def.Cell, nil
				}
			}
			return "", nil
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
