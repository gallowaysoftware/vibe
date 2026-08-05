package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// The C14 sleep schedules' wiring. Every refusal here is a permanent
// configuration answer, and each one is loud at startup rather than at
// 23:30 — the whole point of moving the night out of a host crontab is
// that a wrong one becomes visible before the night it matters.

// startSleepLoops wires the declared nights. Config errors are loud and
// never stop the registry coming up (the warm/probe loops' precedent).
func (d *Daemon) startSleepLoops(cfg Config, hosts *fleetcfg.File) {
	entries := sleepEntries(cfg, hosts)
	if len(entries) == 0 {
		return
	}
	front, ok := hosts.Cells[fleetcfg.FrontCell]
	if !ok {
		slog.Warn("sleep_schedule configured but no front cell in hosts.yaml; loops disabled")
		return
	}
	d.fleet.StartSleepLoop(entries, d.suspendCell(hosts), cfg.FleetLocation(), front.URL)
}

// suspendCell builds the loop's suspend verb: an RPC to that cell's own
// daemon, with the credential the other actuation verbs resolve
// (C13's rule — a second resolver would be testing its own code).
//
// There is deliberately NO piggyback fallback. Piggyback commands are
// at-least-once and are retired by a HIGHER announce seq, which resets
// when a cell reboots; a redelivered `warm` costs a second, while a
// redelivered `suspend` is a box that puts itself back to sleep the
// morning after any reboot. The one verb whose redelivery is
// catastrophic is the one that crosses the boundary the retirement rule
// depends on — the cell's own continued liveness.
func (d *Daemon) suspendCell(hosts *fleetcfg.File) fleetapi.SuspendFunc {
	return func(ctx context.Context, cell, reason string) error {
		c, ok := hosts.Cells[cell]
		if !ok {
			return fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
		}
		if c.DaemonURL == "" {
			return fmt.Errorf("cell %q has no daemon_url; the suspend verb is an RPC and has no announce fallback", cell)
		}
		cred, err := hosts.CellCredential(cell, strings.TrimSpace(os.Getenv("VIBE_TOKEN")), fleetcfg.PreferCellFile, vibeclient.ResolveToken)
		if err != nil {
			return err
		}
		client := vibeclient.NewWithToken(c.DaemonURL, cred.Token)
		_, err = client.CellSuspend(ctx, reason, true)
		return err
	}
}

// sleepEntries validates, clamps and refuses. Split from the wiring so
// every rule is testable without a running daemon.
func sleepEntries(cfg Config, hosts *fleetcfg.File) []fleetapi.SleepScheduleEntry {
	if len(cfg.SleepSchedule) == 0 {
		return nil
	}
	knownModels := map[string]bool{}
	if defs, err := router.LoadDefs(paths.BackendsDir()); err != nil {
		slog.Warn("backend defs unreadable; sleep_schedule warm names not validated", "err", err)
	} else {
		for _, def := range defs {
			knownModels[def.Name] = true
		}
	}

	var out []fleetapi.SleepScheduleEntry
	claimed := map[string]bool{}
	for _, e := range cfg.SleepSchedule {
		refuse := func(why string) {
			slog.Warn("sleep_schedule entry refused", "cell", e.Cell, "why", why)
		}
		// One night per cell. Two entries for the same box are two loops
		// arming two suspends against one machine, and the second one's
		// RPC lands on something that is already freezing — a config typo
		// that reads as a flaky suspend.
		if claimed[e.Cell] {
			refuse("a second sleep_schedule entry for this cell; one night per box")
			continue
		}
		switch {
		case e.Cell == "" || e.Suspend == "":
			refuse("needs cell + suspend")
			continue
		case e.Wake == "":
			// A suspend with no declared wake is a box that never comes
			// back. The config should make that unwritable, so it is.
			refuse("needs a paired wake: a suspend with no declared wake is a box that never comes back")
			continue
		case e.Cell == fleetcfg.FrontCell:
			refuse("the front is the data plane and the control plane; suspending it is a total fleet outage")
			continue
		}
		c, ok := hosts.Cells[e.Cell]
		if !ok {
			refuse("unknown cell (not in hosts.yaml)")
			continue
		}
		switch c.Class {
		case fleetcfg.ClassOpportunistic:
		case fleetcfg.ClassAlwaysOn:
			refuse("class always_on: its absence alarms by design (design §4's class table). If this cell should sleep, it is not always_on — change the class, deliberately")
			continue
		default:
			refuse("class roaming: the paired wake is a packet on the house LAN, and a box that left the building cannot receive one. Its own OS power management owns its night")
			continue
		}
		if c.DaemonURL == "" {
			refuse("no daemon_url: the suspend verb is an RPC, deliberately without the at-least-once announce fallback")
			continue
		}
		if c.Wake == nil {
			refuse("no cells." + e.Cell + ".wake in hosts.yaml: nothing could bring this box back")
			continue
		}
		entry := fleetapi.SleepScheduleEntry{
			Cell:      e.Cell,
			Suspend:   e.Suspend,
			Wake:      e.Wake,
			QuietFor:  clampSleepDuration("quiet_for", e.Cell, e.QuietFor, fleetapi.DefaultSleepQuietFor, fleetapi.MinSleepQuietFor, 0),
			MaxDefer:  clampSleepDuration("max_defer", e.Cell, e.MaxDefer, fleetapi.DefaultSleepMaxDefer, fleetapi.MinSleepMaxDefer, fleetapi.MaxSleepMaxDefer),
			WakeGrace: clampSleepDuration("wake_grace", e.Cell, e.WakeGrace, fleetapi.DefaultSleepWakeGrace, fleetapi.MinSleepWakeGrace, fleetapi.MaxSleepWakeGrace),
		}
		for _, m := range e.Warm {
			if len(knownModels) > 0 && !knownModels[m] {
				slog.Warn("sleep_schedule warm names a model with no backend def (front-only alias?)", "cell", e.Cell, "model", m)
			}
			entry.Warm = append(entry.Warm, m)
		}
		claimed[e.Cell] = true
		out = append(out, entry)
	}
	return out
}

// clampSleepDuration parses one optional duration, clamping rather than
// skipping: a clamped value is a working policy visible in
// fleet_status, a skipped entry is silently absent (C4/C8's rule). max
// of 0 means unbounded above.
func clampSleepDuration(field, cell, raw string, def, min, max time.Duration) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("sleep_schedule duration invalid; using the default", "cell", cell, "field", field, "value", raw, "default", def)
		return def
	}
	if d < min {
		slog.Warn("sleep_schedule duration below the floor; clamped", "cell", cell, "field", field, "value", raw, "clamped_to", min)
		return min
	}
	if max > 0 && d > max {
		slog.Warn("sleep_schedule duration above the ceiling; clamped", "cell", cell, "field", field, "value", raw, "clamped_to", max)
		return max
	}
	return d
}
