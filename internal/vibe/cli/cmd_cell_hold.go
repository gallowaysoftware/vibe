package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// `vibe cell hold` — fleet-control C11. The desk half of the warm
// policy's pause button (the agent half is fleetmcp's hold_model). A
// hold is a lease with hold: true, so this is a POST/DELETE against the
// lease endpoint the fleet already has; C11 adds no route.

func cellHoldCmd() *cobra.Command {
	var (
		apiFlag string
		note    string
		release bool
		holdFor time.Duration
	)
	cmd := &cobra.Command{
		Use:   "hold <cell> <model>",
		Short: "Suspend fleetd's warm policy on a cell while you evaluate a model.",
		Long: "Hold declares that fleetd must not act on its own warm policy for this cell until the\n" +
			"hold expires: the warm-target restore, scheduled warms and throughput probes all skip,\n" +
			"with the hold named in `vibe cell status` and fleet_status. It is the answer to the one\n" +
			"case observation cannot reach — the challenger you are evaluating looks abandoned\n" +
			"because you went to lunch, and restore-after-idle is right to evict it.\n" +
			"A hold is NOT a pin: llama-swap still owns residency and its TTL can still unload the\n" +
			"model. What a hold buys is that fleetd will not be the one to evict it.\n" +
			"It expires on its own (default 4h, max 24h). --release ends one early.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if release && (cmd.Flags().Changed("for") || cmd.Flags().Changed("note")) {
				// Silently ignoring them would leave an operator believing
				// they had shortened a hold or annotated its removal.
				return fmt.Errorf("--release takes no --for/--note (it deletes the hold)")
			}
			if !release {
				if holdFor <= 0 {
					return fmt.Errorf("--for must be a positive duration")
				}
				if holdFor > fleetapi.MaxHoldFor {
					return fmt.Errorf("--for may not exceed %s (a hold suspends a configured policy; re-issue instead of forgetting)", fleetapi.MaxHoldFor)
				}
			}
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			return runCellHold(ctx, cmd.OutOrStdout(), target, args[0], args[1], note, holdFor, release)
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	cmd.Flags().DurationVar(&holdFor, "for", fleetapi.DefaultHoldFor, "how long the hold lasts")
	cmd.Flags().StringVar(&note, "note", "", "why (shown in fleet_status, on the page, and in the pre-drain report)")
	cmd.Flags().BoolVar(&release, "release", false, "end an existing hold early")
	return cmd
}

// noteSuffix renders an optional lease/hold note in parentheses.
func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " (" + termSafe(note) + ")"
}

// runCellHold POSTs (or DELETEs) the hold at fleetd and reports what the
// registry actually stored — the expiry comes back from the server, not
// from this process's clock.
func runCellHold(ctx context.Context, out io.Writer, target fleetdTarget, cell, model, note string, holdFor time.Duration, release bool) error {
	body := map[string]any{"cell": cell, "model": model, "holder": fleetapi.HoldHolder}
	method := http.MethodPost
	if release {
		method = http.MethodDelete
	} else {
		body["hold"] = true
		body["ttl"] = holdFor.String()
		body["note"] = note
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, target.base+"/api/fleet/lease", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := target.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s/api/fleet/lease: %w", method, target.base, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusNotFound {
		// The lease store — and therefore every hold — exists only in the
		// fleetd role. A bare 404 sends the operator hunting for a typo in
		// the cell name instead.
		return fmt.Errorf("%s has no lease endpoint: holds live at fleetd (fleet_registry: true), "+
			"so point --api or hosts.yaml fleetd_url at it", target.base)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s /api/fleet/lease: HTTP %d: %s", method, resp.StatusCode, termSafe(strings.TrimSpace(string(payload))))
	}
	if release {
		// The registry says whether a hold was actually there. Reporting
		// "released" after a mistyped model would tell an operator the warm
		// policy is running again while the real hold keeps suppressing it —
		// the same answer release_hold refuses to give.
		var deleted struct {
			Existed *bool `json:"existed"`
		}
		_ = json.Unmarshal(payload, &deleted)
		if deleted.Existed != nil && !*deleted.Existed {
			fmt.Fprintf(out, "no active hold on %s/%s (already expired, never declared, or a different model) — the warm policy is running there.\n",
				termSafe(cell), termSafe(model))
			return nil
		}
		fmt.Fprintf(out, "hold released on %s/%s — the warm policy resumes there on its next evaluation.\n",
			termSafe(cell), termSafe(model))
		return nil
	}
	var stored fleetapi.Lease
	if err := json.Unmarshal(payload, &stored); err != nil {
		return fmt.Errorf("decode lease response: %w", err)
	}
	fmt.Fprintf(out, "held %s on %s until %s (%s)\n", termSafe(stored.Model), termSafe(stored.Cell),
		stored.ExpiresAt.Local().Format("15:04 MST"), time.Until(stored.ExpiresAt).Round(time.Minute))
	fmt.Fprintln(out, "  fleetd's warm target, warm schedules and probes will skip this cell until then.")
	fmt.Fprintln(out, "  not a pin: llama-swap's own TTL can still unload the model — the hold only stops fleetd causing it.")
	return nil
}
