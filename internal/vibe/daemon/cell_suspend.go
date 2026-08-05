package daemon

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// CellSuspend is the fleet-control C14 verb: this box goes to sleep.
// Same shape as CellDrain — it acts on the daemon's OWN box, remote
// reach is calling a remote daemon — with two differences that come from
// suspend being the most consequential verb in the plan.
//
// First, an idle PROOF. fleetd's schedule already applied the whole
// guard ladder before it called, but the sending side guarding and the
// receiving side not is this repo's most repeated defect, so the cell
// checks its own in-flight counter too. An UNREPORTED count refuses just
// as a non-zero one does: unknown is not zero here either.
//
// Second, the local intent stamp. A box that is about to freeze is not
// serving, and its own echo has to say so before it goes: otherwise the
// first heartbeat after waking carries `serving` at an OLDER instant
// than fleetd's sleep request, C3's conflict rule hands that request
// back as desired_intent, and the cell dutifully runs cell_cmds.drain on
// a box that just woke up. The stamp is not a violation of C2's
// one-writer rule — that rule is about who writes intent at FLEETD, and
// this is the cell's record of its own state.
func (d *Daemon) CellSuspend(ctx context.Context, req *connect.Request[vibev1.CellSuspendRequest]) (*connect.Response[vibev1.CellSuspendResponse], error) {
	// The receiving side holds the structural refusal too. The schedule
	// refuses the front at wiring and suspend_cell refuses it at the
	// guard, but this repo's most repeated defect is exactly the shape
	// where the senders check and the receiver does not — and a third
	// caller (a script with a token, `vibe cell suspend front`) is one
	// line away at all times. Local invocation is untouched: a human at
	// the front box typing the verb has declared it.
	if isRemoteInvocation(ctx) && d.cfg.Fleet.Cell == fleetcfg.FrontCell {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("this daemon is the %s cell: the data plane and the control plane ride it, so a remote suspend is a total fleet outage. Suspend it at the box if that is really what you mean", fleetcfg.FrontCell))
	}
	if d.cfg.CellCmds.Suspend == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("this daemon has no suspend verb configured (cell_cmds.suspend is unset)"))
	}

	report := &vibev1.CellSuspendResponse{IdleStatus: fleetapi.SuspendIdleNotRequired}
	report.ResidentModels = d.probeResidentModels(ctx)
	var inFlight int
	reported := false
	if d.fleet != nil {
		if n, ok := d.fleet.InFlight(d.localCellKey()); ok {
			n64 := int64(n)
			report.InFlightRequests = &n64
			inFlight, reported = n, true
		}
	}

	if req.Msg.RequireIdle {
		switch {
		case !reported:
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("cannot prove this cell is idle: nothing has reported an in-flight count "+
					"(unknown is not zero — suspend explicitly with --force if that is what you mean)"))
		case inFlight > 0:
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("%d request(s) in flight; not suspending", inFlight))
		}
		report.IdleStatus = fleetapi.SuspendIdleVerified
	}

	vctx, cancelVerb := verbCtx(ctx)
	defer cancelVerb()
	if stderr, err := d.cellCmdRunner(vctx, d.cfg.CellCmds.Suspend); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("suspend command failed: %v: %s", err, strings.TrimSpace(stderr)))
	}

	// After the command, never before: a failed verb records nothing (the
	// C2 rule). The window where the box freezes between the command
	// returning and this line is one heartbeat wide and self-heals — the
	// cell wakes echoing `serving`, takes fleetd's drained request, re-runs
	// its idempotent drain verb, and the wake's serving request resumes it.
	d.noteLocalIntent("drained")
	if !isRemoteInvocation(ctx) {
		reason := strings.TrimSpace(req.Msg.Reason)
		if reason == "" {
			reason = "suspended at the box"
		}
		d.postIntentBestEffort(fleetapi.Intent{State: "drained", Reason: reason})
	}
	return connect.NewResponse(report), nil
}
