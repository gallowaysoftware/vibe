package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// `vibe cell suspend` — the box goes to sleep (fleet-control C14). The
// verb to type at the box, and the manual half of sleep_schedule.
//
// It is the same RPC the schedule drives, with one difference: the cell
// is asked to PROVE it is idle unless --force says otherwise. An
// operator asking is not fleetd guessing (C11's line), but the default
// for a verb that takes a box off the fleet is still "refuse and say
// why".

func cellSuspendCmd() *cobra.Command {
	var (
		reason string
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "suspend [cell]",
		Short: "Suspend a cell — the whole box goes to sleep.",
		Long: "Suspend runs the cell's configured cell_cmds.suspend (house-specific; the " +
			"reference example stops the serving stack first, because CUDA contexts do not " +
			"reliably survive S3). The cell refuses unless it can prove it is idle — a " +
			"non-zero in-flight count AND an unreported one both refuse, because unknown is " +
			"not zero. --force skips that proof.\n\n" +
			"Bring the box back with `vibe cell wake <cell>`; a sleep_schedule entry does it " +
			"on its own cron. Check the cell HAS a wake path before suspending it remotely — " +
			"nothing else can.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			var cell string
			if len(args) > 0 {
				cell = args[0]
			}
			return suspendCell(ctx, cmd.OutOrStdout(), cell, reason, force)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the box is going to sleep (recorded as intent)")
	cmd.Flags().BoolVar(&force, "force", false, "suspend without the cell's idle proof (use when you are standing at the box)")
	return cmd
}

// suspendPreflight applies the guard's STRUCTURAL refusals on this side
// of the wire. `vibe cell suspend <cell>` dials that cell's daemon
// directly and so never reaches fleetapi.SuspendGuard, which is where
// the schedule and `suspend_cell` get theirs — and a refusal that exists
// in only two of three producers is not a rule. --force is about
// tonight's conditions and never bypasses any of these (C14 §8): a force
// that suspends the front is not an override, it is a bug.
//
// A cell name of "" is the local daemon — the operator standing at the
// box, with a keyboard attached and nothing to be wrong about.
func suspendPreflight(cell string, force bool) error {
	if cell == "" {
		return nil
	}
	if cell == fleetcfg.FrontCell {
		return fmt.Errorf("refusing to suspend %q: the front is the data plane and the control plane, so suspending it is a total fleet outage", cell)
	}
	hosts, err := fleetcfg.Load()
	if err != nil {
		return fmt.Errorf("load hosts.yaml: %w", err)
	}
	c, ok := hosts.Cells[cell]
	if !ok {
		// resolveCellClient reports the unknown cell with the registry
		// listing; duplicating a worse version of that message here helps
		// nobody.
		return nil
	}
	if c.Class != fleetcfg.ClassOpportunistic {
		return fmt.Errorf("refusing to suspend %q: class %s — only opportunistic cells sleep "+
			"(an always_on cell's absence alarms by design, and a roaming cell cannot be woken by a packet on this LAN)", cell, c.Class)
	}
	if c.Wake == nil && !force {
		return fmt.Errorf("cell %q has no wake: config in hosts.yaml — nothing could bring it back remotely. "+
			"Add cells.%s.wake, or pass --force if you are standing next to the box", cell, cell)
	}
	return nil
}

func suspendCell(ctx context.Context, out io.Writer, cell, reason string, force bool) error {
	if strings.TrimSpace(reason) == "" {
		reason = "suspended at the box"
	}
	if err := suspendPreflight(cell, force); err != nil {
		return err
	}
	client, name, err := resolveCellClient(cell)
	if err != nil {
		return err
	}
	resp, err := client.CellSuspend(ctx, reason, !force)
	if err != nil {
		return fmt.Errorf("suspend %s: %w", displayCell(name), err)
	}
	if cell != "" {
		// The remote path: the cell daemon records intent only for
		// LOCALLY-invoked verbs (the C2 one-writer rule), so a named cell
		// is recorded here, best-effort, exactly as drain does it.
		postCLIIntent(out, cell, "drained", reason, "")
	}
	fmt.Fprintf(out, "%s suspending (%s).\n", displayCell(name), reason)
	if len(resp.ResidentModels) > 0 {
		fmt.Fprintf(out, "- resident at suspend: %s\n", strings.Join(resp.ResidentModels, ", "))
	}
	if resp.InFlightRequests != nil {
		fmt.Fprintf(out, "- in-flight at suspend: %d\n", *resp.InFlightRequests)
	}
	if resp.IdleStatus == fleetapi.SuspendIdleNotRequired {
		fmt.Fprintln(out, "- the idle proof was NOT required (--force)")
	}
	fmt.Fprintf(out, "- wake it with `vibe cell wake %s`.\n", displayCell(name))
	return nil
}
