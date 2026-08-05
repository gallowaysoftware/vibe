package fleetmcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// suspend_cell (fleet-control C14): the explicit half of the declared
// night — "goodnight, put the 5090 to sleep". It shares the schedule's
// guard so an agent and the loop cannot disagree about whether a box may
// go down, and it shares wake_cell's counterpart role: this is the only
// pair of verbs in the plan that takes a box off the fleet and brings it
// back.

func sleepTools() []any {
	return []any{
		map[string]any{
			"name": "suspend_cell",
			"description": "Suspend a cell — the whole BOX goes to sleep (the operator's " +
				"'goodnight' verb, and the manual half of sleep_schedule). It runs that " +
				"cell's configured cell_cmds.suspend, which is house-specific and typically " +
				"stops the serving stack first. Refuses, with the reason, when the cell is " +
				"busy: in-flight requests, an unreported in-flight count (unknown is not " +
				"zero), active leases, a hold_model, an outstanding probe, recent request " +
				"activity, or a declared drain (a drained box is one the operator already " +
				"took). Only opportunistic cells may sleep, never the front. Bring it back " +
				"with wake_cell — and check the cell HAS a wake path before suspending, " +
				"because nothing else can.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cell":   map[string]any{"type": "string", "description": "Cell name from hosts.yaml (opportunistic class, with wake: configured)."},
					"reason": map[string]any{"type": "string", "description": "Why (recorded as declared intent and shown in every status surface), e.g. \"done for the night\"."},
					"force": map[string]any{"type": "boolean", "description": "Skip the policy guards (leases, holds, activity, drain) and the cell's own idle proof. " +
						"An operator asking is not fleetd guessing — but it does NOT skip the structural refusals (the front, an unknown cell, the wrong class), " +
						"and it does not conjure a wake path for a cell that has none."},
				},
				"required": []string{"cell"},
			},
		},
	}
}

// toolSuspendCell drives the suspend RPC. The guard runs here rather
// than only on the cell because only fleetd knows about leases, holds
// and fleet-wide in-flight work — and the cell enforces the idle half a
// second time (require_idle), because the sending side guarding and the
// receiving side not is this repo's most repeated defect.
func (s *Server) toolSuspendCell(ctx context.Context, cell, reason string, force bool) (string, error) {
	cell = strings.TrimSpace(cell)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "suspended on request"
	}
	c, ok := s.hosts.Cells[cell]
	if !ok {
		return "", fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
	}
	// The way BACK is checked before anything about tonight: nothing else
	// in this fleet can bring a suspended box back, so "there is no wake
	// path" is the answer the operator needs first, whatever else is also
	// true about the cell right now.
	if c.Wake == nil && !force {
		return "", fmt.Errorf("cell %q has no wake: config in hosts.yaml — nothing could bring it back remotely. "+
			"Add cells.%s.wake, or pass force if you are standing next to the box", cell, cell)
	}
	if block, ok := s.fleet.SuspendGuard(cell, 0); !ok {
		if !force || block.Structural {
			return "", fmt.Errorf("not suspending %s: %s", cell, block.Why)
		}
	}
	client, err := s.cellClient(cell)
	if err != nil {
		return "", err
	}
	if client == nil {
		// No announce fallback, deliberately: a piggybacked suspend is
		// redelivered after a cell reboot, which is a box that puts itself
		// back to sleep the morning after.
		return "", fmt.Errorf("cell %q has no daemon_url; the suspend verb is an RPC and has no announce fallback", cell)
	}
	resp, err := client.CellSuspend(ctx, reason, !force)
	if err != nil {
		return "", fmt.Errorf("suspend %s: %v", cell, err)
	}
	if _, err := s.fleet.SetIntent(cell, "drained", reason, ""); err != nil {
		return "", fmt.Errorf("suspended %s but failed to record intent at fleetd: %v", cell, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Suspended %s (reason: %s). Intent recorded — status shows DRAINED/OFF with that reason until it is back.", cell, reason)
	if len(resp.ResidentModels) > 0 {
		fmt.Fprintf(&b, "\n- resident at suspend: %s", strings.Join(resp.ResidentModels, ", "))
	}
	if resp.InFlightRequests != nil {
		fmt.Fprintf(&b, "\n- in-flight at suspend: %d", *resp.InFlightRequests)
	}
	if resp.IdleStatus == fleetapi.SuspendIdleNotRequired {
		b.WriteString("\n- the cell's idle proof was NOT required (forced)")
	}
	b.WriteString("\n- bring it back with wake_cell; a sleep_schedule entry would do it on its own cron.")
	return b.String(), nil
}
