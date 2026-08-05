package daemon

import (
	"log/slog"
	"maps"
	"slices"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// checkSwapKeys resolves every declared llama-swap API key once at
// fleetd startup (fleet-control C15).
//
// It is the "loud at config load" half of the phase. A `swap_key_file`
// that does not resolve is a permanent configuration answer, not a
// runtime condition: nothing fleetd does later will make the file
// readable, and the first symptom without this line is a warm target
// sitting on `skipped` at 03:00 with an explanation only in the state
// document. So it is logged at ERROR, by name, with the consequence
// spelled out.
//
// It is deliberately NOT fatal. A fleetd that refuses to start because
// one cell's key file is missing takes the whole control plane down over
// a cell that may be switched off — and every affected call fails closed
// anyway, visibly, in fleet_status's swap_auth block and in
// `vibe fleet doctor`'s swap.credential check. Same posture as C12's
// guest token: fail closed, stay up, say so.
//
// It cannot check the other half — whether a cell's llama-swap DEMANDS a
// key it was not given — because that is only knowable by asking the
// cell. That answer arrives on the first probe round and lands in the
// same two surfaces.
func checkSwapKeys(hosts *fleetcfg.File) {
	if hosts == nil {
		return
	}
	var declared []string
	for _, name := range slices.Sorted(maps.Keys(hosts.Cells)) {
		cred, err := hosts.SwapCredentialFor(name)
		if err != nil {
			slog.Error("llama-swap API key will not resolve; every fleetd call to this cell's llama-swap fails closed",
				"cell", name, "err", err,
				"affects", "warms through the front, /running and /v1/models probes, the events stream, unload_model")
			continue
		}
		if cred.Configured {
			declared = append(declared, name)
		}
	}
	if len(declared) > 0 {
		// The value never appears; the cells that carry one do, because
		// "which cells are keyed" is exactly the question an operator has
		// when half the fleet 401s.
		slog.Info("llama-swap API keys resolved", "cells", declared)
	}
}
