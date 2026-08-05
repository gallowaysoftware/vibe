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

// `vibe fleet notify` is the human half of fleet-control C9: check what
// the pager would say, prove it still works, and declare away/home. The
// alarm policy itself is fleetd's — there is nothing to tune here,
// deliberately, because a per-invocation override is a mute switch
// nobody remembers flipping.

func fleetNotifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Fleet alarm notifications: status, test, away/home.",
		Long: "fleetd delivers the design's alarm column — an always_on cell absent with no " +
			"declared intent, a serving-flags mismatch that has persisted, a drain landing on a " +
			"cell that still holds advisory leases — to one webhook. Everything else stays in " +
			"fleet_status where it belongs.",
	}
	cmd.AddCommand(fleetNotifyStatusCmd(), fleetNotifyTestCmd(), fleetNotifyScopeCmd(true), fleetNotifyScopeCmd(false))
	return cmd
}

func fleetNotifyStatusCmd() *cobra.Command {
	var apiFlag string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the notifier's scope, live alarms, and delivery counters.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmdContext(cmd)
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			snap, err := target.fetchState(ctx)
			if err != nil {
				return err
			}
			renderNotifyStatus(cmd.OutOrStdout(), snap)
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	return cmd
}

func renderNotifyStatus(out io.Writer, snap *fleetapi.StateSnapshot) {
	n := snap.Notify
	if n == nil {
		fmt.Fprintln(out, "notify: not configured (set fleet.notify.url_file in the fleetd config)")
		return
	}
	fmt.Fprintf(out, "notify: scope %s", termSafe(n.Scope))
	if n.ScopeUntil != nil {
		fmt.Fprintf(out, " until %s", n.ScopeUntil.Format(time.RFC3339))
	}
	if n.ScopeReason != "" {
		fmt.Fprintf(out, " (%s)", termSafe(n.ScopeReason))
	}
	fmt.Fprintln(out)
	if !n.Configured {
		fmt.Fprintln(out, "  no webhook configured — alarms are evaluated nowhere and delivered nowhere")
		return
	}
	fmt.Fprintf(out, "  endpoint: %s\n", termSafe(n.Endpoint))
	fmt.Fprintf(out, "  alarms enabled: %s\n", termSafe(strings.Join(n.Enabled, " ")))
	if n.FingerprintSource != "" {
		fmt.Fprintf(out, "  fingerprint source: %s\n", termSafe(n.FingerprintSource))
	}
	if len(n.Alarms) == 0 {
		fmt.Fprintln(out, "  no alarms live")
	}
	for _, a := range n.Alarms {
		fmt.Fprintf(out, "  %-10s %-40s %s\n", a.State, termSafe(a.Key), termSafe(a.Detail))
	}
	if n.Suppressed > 0 {
		fmt.Fprintf(out, "  SUPPRESSED while away: %d (%s) — one digest goes out when you declare home\n",
			n.Suppressed, termSafe(strings.Join(n.SuppressedKeys, " ")))
	}
	if n.Deferred > 0 {
		fmt.Fprintf(out, "  deferred by the rate limit: %d\n", n.Deferred)
	}
	if d := n.Delivery; d != nil {
		fmt.Fprintf(out, "  delivery: sent %d · failed %d · retries %d · dropped %d\n", d.Sent, d.Failed, d.Retries, d.Dropped)
		if d.LastError != "" {
			fmt.Fprintf(out, "  last error: %s\n", termSafe(d.LastError))
		}
	}
}

func fleetNotifyTestCmd() *cobra.Command {
	var apiFlag string
	cmd := &cobra.Command{
		Use:   "test [message]",
		Short: "Send one notification through the real delivery path.",
		Long: "An alerting path nobody has ever sent a message through is not an alerting path. " +
			"The test message is not an alarm: it skips the dwell, the dedup and the away gate, " +
			"so it also works as a liveness check while away.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg := "test notification from vibe fleet"
			if len(args) == 1 {
				msg = args[0]
			}
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			if err := target.postFleet(cmdContext(cmd), "/api/fleet/notify/send",
				map[string]string{"title": "vibe fleet: test", "message": msg}, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "queued; if it does not arrive, `vibe fleet notify status` has the delivery counters")
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	return cmd
}

// fleetNotifyScopeCmd builds both `away` and `home`: one code path, so
// they cannot drift in what they claim to do.
func fleetNotifyScopeCmd(away bool) *cobra.Command {
	var (
		apiFlag string
		reason  string
		until   string
	)
	use, short := "home", "Resume alarm delivery (and send one digest of what was suppressed)."
	if away {
		use, short = "away", "Withhold alarm delivery (vacation). Alarms keep firing and stay visible in fleet_status."
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: "This gates NOTIFICATION only. Nothing is drained, no catalog changes, no request " +
			"is routed differently. Suppressed alarms are counted and listed in fleet_status the " +
			"whole time, and declaring home sends one digest naming them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			body := map[string]string{"scope": "home", "by": "cli"}
			if away {
				body = map[string]string{"scope": "away", "reason": reason, "until": until, "by": "cli"}
			}
			var scope fleetapi.NotifyScope
			if err := target.postFleet(cmdContext(cmd), "/api/fleet/notify/scope", body, &scope); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "notify scope: %s\n", termSafe(scope.Scope))
			switch {
			case scope.Until != nil:
				fmt.Fprintf(out, "  alarms resume automatically at %s\n", scope.Until.Format(time.RFC3339))
			case away:
				fmt.Fprintln(out, "  no --until set: alarms stay undelivered until someone runs `vibe fleet notify home`")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	if away {
		cmd.Flags().StringVar(&reason, "reason", "", "why (e.g. \"vacation\")")
		cmd.Flags().StringVar(&until, "until", "", "when away ends: RFC3339 instant or Go duration (e.g. 336h)")
	}
	return cmd
}

// postFleet POSTs a JSON body to a fleetd route, decoding into out when
// non-nil. Shared by the notify verbs and `vibe cell await --notify`.
func (t fleetdTarget) postFleet(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+path, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(t.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
