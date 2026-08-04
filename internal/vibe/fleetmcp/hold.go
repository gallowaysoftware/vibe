package fleetmcp

import (
	"fmt"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// hold_model / release_hold (fleet-control C11): the agent-facing half
// of the warm policy's pause button. Both verbs are declarations against
// the lease store — no new HTTP route, no cell-side code, nothing on the
// data plane.

// holdTools are the two tool descriptors. They live beside the
// implementation so the description and the behaviour are edited
// together: the "this is not a pin" sentence is load-bearing, and an
// agent that drops it will tell an operator their model is safe.
func holdTools() []any {
	return []any{
		map[string]any{
			"name": "hold_model",
			"description": "Suspend fleetd's automatic warm policy on one cell until a deadline — the " +
				"evaluation-afternoon verb. Without it, the cell's warm target correctly notices that " +
				"the challenger model you were testing has been idle since you left for lunch and " +
				"restores the default, evicting it. A hold also suppresses scheduled warms and " +
				"throughput probes on that cell. It does NOT pin the model: llama-swap still owns " +
				"residency and its TTL can still unload it — what a hold guarantees is that FLEETD " +
				"will not be the one to evict, and that nothing fleetd does pulls another model onto " +
				"that GPU. It expires on its own (default 4h, max 24h), so a forgotten hold cannot " +
				"permanently disable the policy. Explicit verbs still work while held: warm_model, " +
				"unload_model, drain_cell and resume_cell are unaffected.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":  map[string]any{"type": "string", "description": "Cell name from hosts.yaml (not the front — it serves no models of its own)."},
					"model": map[string]any{"type": "string", "description": "The model being protected. It is the LABEL on the hold: a hold is cell-scoped, because a warm of any model on that cell is what evicts this one."},
					"for":   map[string]any{"type": "string", "description": "Go duration the hold lasts, e.g. \"3h\". Default 4h, maximum 24h."},
					"note":  map[string]any{"type": "string", "description": "Why (shown in fleet_status, on the page, and in the pre-drain report), e.g. \"evaluating glm-5 vs the default\"."},
				},
				"required": []string{"cell", "model"},
			},
		},
		map[string]any{
			"name": "release_hold",
			"description": "End a hold_model early, letting the warm policy resume on that cell. " +
				"Holds expire on their own, so this is only needed when the evaluation finished " +
				"sooner than planned.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":  map[string]any{"type": "string", "description": "Cell name from hosts.yaml."},
					"model": map[string]any{"type": "string", "description": "The model the hold was declared on."},
				},
				"required": []string{"cell", "model"},
			},
		},
	}
}

// toolHoldModel declares the hold. The reply states the expiry AND the
// limitation, in that order, because an agent relaying only the first
// half leaves the operator believing their model is pinned.
func (s *Server) toolHoldModel(cell, model, forStr, note string) (string, error) {
	d := fleetapi.DefaultHoldFor
	if strings.TrimSpace(forStr) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(forStr))
		if err != nil {
			return "", fmt.Errorf("invalid for %q: want a Go duration like \"3h\"", forStr)
		}
		d = parsed
	}
	l, err := s.fleet.SetHold(cell, model, note, d)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Held %s on %s until %s (%s). fleetd's warm target, warm schedules and probes "+
		"will not act on %s until then. This is not a pin: llama-swap's own TTL can still unload %s — "+
		"the hold only guarantees fleetd won't cause the eviction. Release early with release_hold.",
		l.Model, l.Cell, l.ExpiresAt.Format(time.RFC3339), time.Until(l.ExpiresAt).Round(time.Minute),
		l.Cell, l.Model), nil
}

// toolReleaseHold ends one early. Releasing nothing is reported as
// nothing released — an agent must not tell an operator it lifted a hold
// that was never there (or had already expired).
func (s *Server) toolReleaseHold(cell, model string) (string, error) {
	existed, err := s.fleet.ReleaseHold(cell, model)
	if err != nil {
		return "", err
	}
	if !existed {
		return fmt.Sprintf("No active hold on %s/%s (already expired or never declared). "+
			"The warm policy is running on that cell.", cell, model), nil
	}
	return fmt.Sprintf("Released the hold on %s/%s. fleetd's warm policy resumes there on its next evaluation.", cell, model), nil
}
