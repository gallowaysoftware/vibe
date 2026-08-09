package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// `vibe cell` is the fleet-control observability surface for humans and
// scripts (fleet-control C1): what is every cell doing, and why. The
// fleetd address resolves --api → $VIBE_API → hosts.yaml fleetd_url →
// the local daemon, and an unreachable fleetd degrades to probing cells
// directly (labeled, intent unavailable) rather than failing blind.

func cellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cell",
		Short: "Fleet cell observability (status, await).",
	}
	cmd.AddCommand(cellStatusCmd(), cellAwaitCmd(), cellDrainCmd(), cellResumeCmd(), cellWakeCmd(), cellSuspendCmd(), cellHoldCmd())
	return cmd
}

// fleetdTarget is the resolved fleetd address: either a remote base URL
// (http client + bearer token) or the local daemon over its unix socket.
type fleetdTarget struct {
	base string // "" means local unix socket
	hc   *http.Client
}

// resolveFleetd applies the resolution order; explicit --api beats env
// beats hosts.yaml fleetd_url beats the local daemon. A hosts.yaml that
// exists but fails to parse is surfaced via the returned error so a
// broken registry is never misreported as an absent one.
func resolveFleetd(apiFlag string) (fleetdTarget, error) {
	base := apiFlag
	if base == "" {
		base = strings.TrimSpace(os.Getenv("VIBE_API"))
	}
	if base == "" {
		hosts, err := fleetcfg.Load()
		if err != nil {
			return fleetdTarget{}, fmt.Errorf("parse %s: %w", paths.HostsFile(), err)
		}
		if hosts != nil {
			base = hosts.FleetdURL
		}
	}
	if base == "" {
		// Local daemon over the unix socket (no token — the socket's 0600
		// perms are the boundary), same dial trick as the RPC client.
		return fleetdTarget{hc: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", paths.Socket())
				},
			},
		}, base: "http://vibe.local"}, nil
	}
	return fleetdTarget{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// fetchState GETs /api/fleet/state from the resolved fleetd.
func (t fleetdTarget) fetchState(ctx context.Context) (*fleetapi.StateSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/api/fleet/state", nil)
	if err != nil {
		return nil, err
	}
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(t.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET %s/api/fleet/state: HTTP %d: %s", t.base, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var snap fleetapi.StateSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode fleet state: %w", err)
	}
	return &snap, nil
}

func cellStatusCmd() *cobra.Command {
	var (
		apiFlag string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show every cell's derived state, resident models, intent, and last-seen.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			snap, err := target.fetchState(ctx)
			out := cmd.OutOrStdout()
			if err != nil {
				// The degraded fallback is a DIFFERENT document: it is
				// assembled by probing cells from this box, carries no intent
				// and no last-seen, and shares nothing but the word "cell"
				// with a state snapshot. Emitting it under --json would hand a
				// script something shaped like fleetd's answer that fleetd
				// never said, so --json fails here and names the human path.
				if asJSON {
					return fmt.Errorf("fleetd unreachable (%w) — --json emits the snapshot fleetd serves and nothing else; re-run without --json for the degraded direct-probe table", err)
				}
				return renderDegraded(out, err)
			}
			if asJSON {
				return writeJSON(out, snap)
			}
			renderStatus(out, target.base, snap)
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the snapshot as JSON (the same document /api/fleet/state serves)")
	return cmd
}

// termSafe strips control characters from server-supplied strings
// before they hit the terminal: cell names, intent reasons, and model
// ids all originate off-box, and a malicious or garbled one must not
// inject escape sequences into the operator's tty.
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
}

// renderStatus prints the derived table plus per-cell deep links.
func renderStatus(out io.Writer, base string, snap *fleetapi.StateSnapshot) {
	fmt.Fprintf(out, "fleetd: %s", termSafe(base))
	if snap.Daemon.AuthRejected > 0 {
		fmt.Fprintf(out, "  (auth rejections since start: %d — a client somewhere holds a stale token)", snap.Daemon.AuthRejected)
	}
	if snap.Daemon.GuestEnabled {
		// A separate sentence from the line above on purpose: a guest
		// reaching past their two routes is not a stale-token client, and
		// C12 keeps the two counters apart so this one stays readable.
		fmt.Fprintf(out, "  (guest read-only token: on; %d request(s) refused past its two routes)", snap.Daemon.GuestRejected)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%-14s %-13s %-14s %-34s %s\n", "CELL", "DISPLAY", "CLASS", "MODELS", "INTENT / LAST SEEN")
	for _, c := range snap.Cells {
		fmt.Fprintf(out, "%-14s %-13s %-14s %-34s %s\n",
			termSafe(c.Name), termSafe(c.Display), dash(termSafe(c.Class)), termSafe(modelSummary(c)), termSafe(intentLastSeen(c)))
	}
	for _, c := range snap.Cells {
		fmt.Fprintf(out, "  %-12s ui: %s/ui\n", termSafe(c.Name), termSafe(strings.TrimRight(c.URL, "/")))
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// modelSummary compresses resident models to "id:state" pairs, stopped
// models omitted — a peers-only front reads as "-".
func modelSummary(c fleetapi.CellSnapshot) string {
	var live []string
	stopped := 0
	for _, m := range c.Models {
		if m.State == "stopped" || m.State == "" {
			stopped++
			continue
		}
		live = append(live, m.ID+":"+m.State)
	}
	if len(live) == 0 {
		if !c.Reachable {
			return "-"
		}
		return "(none resident)"
	}
	sort.Strings(live)
	s := strings.Join(live, " ")
	if stopped > 0 {
		s += fmt.Sprintf(" (+%d stopped)", stopped)
	}
	return s
}

func intentLastSeen(c fleetapi.CellSnapshot) string {
	var parts []string
	if c.Intent != nil {
		in := c.Intent.Reason
		if c.Intent.ETA != "" {
			in += ", eta " + c.Intent.ETA
		}
		if in == "" {
			in = c.Intent.State
		}
		if c.IntentPending {
			in += " (requested, awaiting cell ack)"
		}
		parts = append(parts, "intent: "+in)
	} else if c.IntentPending {
		// A resume request has no drained intent to show, but the
		// pending marker is the point.
		parts = append(parts, "intent: resume requested, awaiting cell ack")
	}
	// A hold (C11) belongs in this column: it is a declaration, and it is
	// the reason a cell's default model has not come back.
	for _, l := range c.Leases {
		if !l.Hold {
			continue
		}
		held := fmt.Sprintf("held: %s, %s", l.Model, fleetapi.HoldLeft(l.ExpiresAt))
		if l.Note != "" {
			held += " (" + l.Note + ")"
		}
		parts = append(parts, held)
	}
	if c.LastSeen != nil && !c.Reachable {
		parts = append(parts, "last seen "+time.Since(*c.LastSeen).Round(time.Second).String()+" ago")
	}
	return strings.Join(parts, "; ")
}

// renderDegraded probes cells directly from hosts.yaml when fleetd is
// unreachable. Intent is fleetd's — it is unavailable here, and the
// output says so rather than guessing.
func renderDegraded(out io.Writer, fleetdErr error) error {
	hosts, err := fleetcfg.Load()
	if err != nil {
		return fmt.Errorf("fleetd unreachable (%v) and %s is invalid: %w", fleetdErr, paths.HostsFile(), err)
	}
	if !hosts.HasCells() {
		return fmt.Errorf("fleetd unreachable (%v) and no hosts.yaml cells for the degraded fallback", fleetdErr)
	}
	fmt.Fprintf(out, "DEGRADED (fleetd unreachable: %v)\n", fleetdErr)
	fmt.Fprintln(out, "probing cells directly from hosts.yaml — intent and last-seen unavailable")
	fmt.Fprintf(out, "%-14s %-10s %-10s %s\n", "CELL", "CELL", "HOST", "MODELS")
	names := make([]string, 0, len(hosts.Cells))
	for n := range hosts.Cells {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := hosts.Cells[n]
		cellUp, models, authNote := probeCellDirect(hosts, n)
		host := "-"
		if c.HostProbe != "" {
			if probeTCPDirect(c.HostProbe) {
				host = "up"
			} else {
				host = "down"
			}
		}
		cell := "down"
		switch {
		case cellUp:
			cell = "up"
		case authNote != "":
			// A cell whose llama-swap refused the credential is not a cell
			// that is DOWN — it answered. Printing "down" here is the same
			// absent-evidence-as-fact mistake the warm policy spent two
			// phases removing, and this is the screen someone reads while
			// fleetd is already broken (C15).
			cell = "auth?"
			models = authNote
		}
		fmt.Fprintf(out, "%-14s %-10s %-10s %s\n", termSafe(n), cell, host, termSafe(models))
	}
	return nil
}

// probeCellDirect reads one cell's llama-swap catalog from THIS box,
// carrying the cell's declared API key (C15). The third return names a
// CREDENTIAL problem, which callers must not render as "the cell is
// down": llama-swap answered, it just refused us, and the two have
// completely different fixes.
func probeCellDirect(hosts *fleetcfg.File, name string) (up bool, models, authNote string) {
	c := hosts.Cells[name]
	hc := &http.Client{Timeout: 3 * time.Second}
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(c.URL, "/")+"/v1/models", nil)
	if err != nil {
		return false, "-", ""
	}
	cred, err := hosts.SwapCredentialFor(name)
	if err != nil {
		// Fail closed, exactly as fleetd does: a declared key that will not
		// read must not become an unauthenticated probe reported as a dead
		// cell.
		return false, "-", "swap_key_file unreadable: " + err.Error()
	}
	if cred.Configured {
		req.Header.Set("Authorization", "Bearer "+cred.Key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, "-", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if cred.Configured {
			return false, "-", "llama-swap refused cells." + name + ".swap_key_file"
		}
		return false, "-", "llama-swap wants an API key; no cells." + name + ".swap_key_file declared"
	}
	if resp.StatusCode != http.StatusOK {
		return false, "-", ""
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return true, "(catalog unreadable)", ""
	}
	ids := make([]string, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return true, strings.Join(ids, " "), ""
}

func probeTCPDirect(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func cellAwaitCmd() *cobra.Command {
	var (
		apiFlag  string
		up       bool
		down     bool
		notify   bool
		timeout  time.Duration
		interval time.Duration
		cond     awaitConds
	)
	cmd := &cobra.Command{
		Use:   "await <cell>",
		Short: "Block until a cell is reachable (--up), unreachable (--down), warm (--ready) or quiet (--idle).",
		Long: "Park a script on fleet state: `vibe cell await gpu-cell --up && ./overnight-batch.sh`.\n" +
			"--up means the cell's llama-swap answers; intent is deliberately not consulted (routing truth rule).\n" +
			"--model <id> --ready waits for llama-swap to report that model resident and ready — cell-up is not\n" +
			"model-warm, and on the heavy cell that gap is 6-10 minutes.\n" +
			"--idle <dur> waits until fleetd has OBSERVED that long without a request on the cell. Where fleetd\n" +
			"has no live event stream to it there is no evidence, and await keeps waiting and says so rather\n" +
			"than firing a batch into a busy box.\n" +
			"--unleased waits for other consumers' advisory leases to clear; --lease <holder> takes one on\n" +
			"success, which is what makes the pair a queue.\n" +
			"--notify pushes one message through fleetd's configured webhook when the wait ends — the\n" +
			"await-unblocked half of C9, for a human parked on a cell rather than a script. It is sent after\n" +
			"the --lease claim and reports its outcome, so the one message a human reads is the one that says\n" +
			"whether the box is theirs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmdContext(cmd)
			if up && down {
				return fmt.Errorf("--up and --down are contradictory")
			}
			cond.wantUp = !down // --up is the default
			if interval <= 0 {
				return fmt.Errorf("--interval must be > 0")
			}
			if err := validateAwaitFlags(args[0], cond, cmd.Flags().Changed("lease-ttl")); err != nil {
				return err
			}
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			ev, err := awaitCell(ctx, out, target, args[0], cond, timeout, interval)
			if err != nil {
				// C9's rule, unchanged: --notify is await-UNBLOCKED. A
				// timeout or a fail-fast typo is the script's business, and
				// a phone that buzzes for both has taught its owner nothing.
				return err
			}
			// The claim, then the push, then the report. Ordering the push
			// last is what lets it carry the lease outcome; see
			// notifyAwaitUnblocked.
			var leaseErr error
			if cond.lease.holder != "" {
				leaseErr = acquireLease(ctx, target, args[0], cond)
			}
			if notify {
				notifyAwaitUnblocked(out, target, args[0], cond, ev, leaseErr)
			}
			if leaseErr != nil {
				return fmt.Errorf("the wait was satisfied but the lease was refused: %w", leaseErr)
			}
			if cond.lease.holder != "" {
				fmt.Fprintf(out, "lease held: %s on %s/%s for %s\n", cond.lease.holder, args[0], cond.model, cond.lease.ttl)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	cmd.Flags().BoolVar(&up, "up", false, "wait until the cell answers (default)")
	cmd.Flags().BoolVar(&down, "down", false, "wait until the cell stops answering")
	cmd.Flags().StringVar(&cond.model, "model", "", "the model this wait is about (with --ready)")
	cmd.Flags().BoolVar(&cond.ready, "ready", false, "also wait until --model is resident and ready on the cell")
	cmd.Flags().DurationVar(&cond.idle, "idle", 0, "also wait until the cell has been observed request-idle this long")
	cmd.Flags().BoolVar(&cond.unleased, "unleased", false, "also wait until no other holder's advisory lease names the cell")
	cmd.Flags().StringVar(&cond.lease.holder, "lease", "", "on success, take an advisory lease under this holder name")
	cmd.Flags().DurationVar(&cond.lease.ttl, "lease-ttl", time.Hour, "TTL for --lease")
	cmd.Flags().StringVar(&cond.lease.note, "lease-note", "", "note recorded with --lease (shown in the pre-drain report)")
	cmd.Flags().BoolVar(&notify, "notify", false, "push a notification through fleetd's webhook when the wait ends")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (0 = wait forever)")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "poll interval")
	return cmd
}

// notifyAwaitUnblocked pushes the await's own result through fleetd's
// notifier. Best-effort by construction: the wait already ended, so a
// failed push warns rather than failing a command whose whole job just
// succeeded — and the failure names itself rather than being swallowed.
//
// It fires AFTER the --lease claim and reports that claim's outcome
// (C9 + C10). The question a human parked on a cell has when the phone
// buzzes is not "did the wait end" — it is "is the box mine". A push
// sent before the claim cannot answer that, and a push skipped on a
// refusal answers it with silence, which is the reading that sends
// someone to bed believing a batch is running. So: exactly one message
// either way, naming what happened, and the refusal still fails the
// command.
//
// The message repeats the terminal's success line verbatim, because on
// this path the terminal is the surface nobody is watching — and C10's
// rule that a qualified evidence gap rides the SUCCESS line is worth
// least if it is dropped from the only surface a sleeping operator
// reads.
func notifyAwaitUnblocked(out io.Writer, target fleetdTarget, cell string, cond awaitConds, ev awaitEval, leaseErr error) {
	title := "vibe fleet: " + cell + " is " + upWord(cond.wantUp)
	lines := []string{"the wait on " + cell + " ended: " + awaitSuccessLine(cell, cond.wantUp, ev)}
	switch {
	case leaseErr != nil:
		title += ", but the lease was refused"
		lines = append(lines,
			notifyText("lease REFUSED for holder "+cond.lease.holder+": "+leaseErr.Error(), maxNotifyDetailBytes),
			"the command exited non-zero and nothing is holding "+cell)
	case cond.lease.holder != "":
		lines = append(lines, fmt.Sprintf("lease held: %s on %s/%s for %s",
			cond.lease.holder, cell, cond.model, cond.lease.ttl))
	}
	body := map[string]string{
		"title": notifyText(title, maxNotifyTitleBytes),
		// Not notifyText: the newlines between these lines are structure,
		// and each line was made printable on its way in.
		"message": clampBytes(strings.Join(lines, "\n"), maxNotifyMessageBytes),
	}
	// Deliberately not the command's context: a wait interrupted at the
	// terminal still ended, and the push is the half of it a human sees.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := target.postFleet(ctx, "/api/fleet/notify/send", body, nil); err != nil {
		fmt.Fprintf(out, "warning: --notify push failed: %v\n", err)
	}
}

// The push's own budgets, deliberately under fleetd's (which rejects a
// title carrying a control character or exceeding its announce-field
// bound, and caps a message at 2000 bytes). These strings are assembled
// from things that came off the wire — an evidence gap fleetd worded, a
// refusal body fleetd wrote, up to 4 KB of it — and a --notify that 400s
// is a human who never gets paged.
const (
	maxNotifyTitleBytes   = 200
	maxNotifyDetailBytes  = 800
	maxNotifyMessageBytes = 1500
)

// clampBytes truncates to a byte budget on a rune boundary. Splitting a
// rune would not fail fleetd's printability check — the fragment decodes
// to U+FFFD, which is printable — it would just render as garbage on the
// phone.
func clampBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	for len(s)+len(ell) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		if size == 0 {
			break
		}
		s = s[:len(s)-size]
	}
	return s + ell
}

func notifyText(s string, max int) string { return clampBytes(termSafe(s), max) }

// awaitCell blocks until every requested condition holds for the named
// cell IN THE SAME SNAPSHOT (C10): evaluating them across different
// polls would report "ready and idle" having never observed a moment
// when both were true.
//
// C3 made transitions subscribable: it rides /api/fleet/events when the
// stream works (cellUp/cellDown, cellReturned/cellStale/cellWithdrawn)
// and falls back to the poll interval when it doesn't. A transition for
// THIS cell triggers an immediate re-poll — that is what keeps C3's
// sub-second unblock — but the snapshot is what decides, always: an
// event proves something moved, never that the cell is reachable, that a
// model is warm or that a GPU is free. Intent is never consulted — a
// drained cell that answers is up (routing truth rule).
// The satisfied evaluation is RETURNED, not just printed: --notify's
// message is built from it, and a push that describes the wait in
// weaker terms than the terminal did is the same evidence gap C10
// refuses everywhere else.
func awaitCell(ctx context.Context, out io.Writer, target fleetdTarget, cell string, cond awaitConds, timeout, interval time.Duration) (awaitEval, error) {
	// Cancelled on EVERY return, not just the --timeout path: the events
	// goroutine below only exits on ctx, so a successful wait under
	// `--timeout 0` used to leave it streaming until the caller's context
	// died. Harmless in a process that exits next; a leak in a function
	// that is also called from tests and could be called from a loop.
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	tick := time.NewTicker(interval)
	defer tick.Stop()

	// Events channel: the stream goroutine reports liveness so a dead
	// stream can't be mistaken for "no transitions happening".
	events := make(chan fleetEvent, 16)
	var streamAlive atomic.Bool
	streamAlive.Store(true)
	go func() {
		if err := streamFleetEvents(ctx, target, events); err != nil {
			streamAlive.Store(false)
		}
	}()

	// A transition frame is a WAKE-UP, never a verdict. The five types
	// mean five different things and only the snapshot reconciles them:
	// `fleet.cellStale` says the ANNOUNCER died, and C6's whole finding
	// is that a dead announcer leaves a healthy cell serving
	// (`snap.Reachable = probeOK`), so returning "down" on it reports the
	// exact cell C6 kept reachable as gone. `fleet.cellReturned` says the
	// announcer is back, which a drained echo can still render
	// unreachable. Even `fleet.cellDown` is the events stream dropping,
	// not the two GETs `Reachable` is defined by. Re-polling costs one
	// round trip and keeps the sub-second unblock C3 built this for,
	// while leaving ONE thing deciding — the same snapshot rule the
	// composite conditions already follow.
	eventRelevant := func(ev fleetEvent) bool {
		if ev.Cell != cell {
			return false
		}
		switch ev.Type {
		case "fleet.cellUp", "fleet.cellDown", "fleet.cellStale",
			"fleet.cellWithdrawn", "fleet.cellReturned":
			return true
		}
		return false
	}

	var lastKey string
	var printed bool
	var lastUnmet []string
	var lastErr error
	for {
		ev, err := evaluateAwait(ctx, target, cell, cond)
		switch {
		case err == nil && ev.ok:
			fmt.Fprintln(out, awaitSuccessLine(cell, cond.wantUp, ev))
			return ev, nil
		case err == nil:
			lastUnmet, lastErr = ev.unmet, nil
			// On a CHANGE OF STATE, not of text. The status line carries a
			// live idle counter, so deduplicating on the rendered line
			// deduplicates nothing against a real fleetd — an overnight
			// `--idle` wait would log one line per poll for eight hours.
			if !printed || ev.key != lastKey {
				lastKey, printed = ev.key, true
				fmt.Fprintf(out, "await %s: %s\n", cell, ev.status)
			}
		case errors.Is(err, errUnknownCell), errors.Is(err, errUnknownModel):
			// A typo never becomes true by waiting, and --timeout 0 is the
			// documented overnight-batch idiom: without this the command
			// parks until the machine reboots.
			return awaitEval{}, err
		default:
			// A transport error is a restarting fleetd, which is exactly
			// what the retry loop is for. An error caused by our OWN
			// deadline expiring mid-poll is not evidence of anything and
			// must not become the reported cause — it is the timeout.
			if ctx.Err() == nil {
				lastErr = err
				fmt.Fprintf(out, "await %s: %v (retrying)\n", cell, err)
			}
		}
		// Wait for the next REASON to re-evaluate. An unrelated frame is
		// not one: fleetd forwards every upstream llama-swap payload
		// (logData, modelStatus, metrics) from every cell onto this
		// stream, and /api/fleet/state is an uncached probe round of the
		// whole fleet. Re-polling per frame turns a long wait — which is
		// the entire point of --idle — into a probe flood against the
		// llama-swaps that are serving the traffic.
		for repoll := false; !repoll; {
			select {
			case <-ctx.Done():
				if timeout > 0 {
					switch {
					case lastErr != nil:
						// fleetd was unreachable at the end: reporting "the cell
						// never came up" would blame the cell for the registry.
						return awaitEval{}, fmt.Errorf("timeout waiting for %s: last attempt failed: %w", cell, lastErr)
					case cond.extras() && len(lastUnmet) > 0:
						return awaitEval{}, fmt.Errorf("timeout waiting for %s: %s", cell, strings.Join(lastUnmet, "; "))
					}
					return awaitEval{}, fmt.Errorf("timeout waiting for %s to come %s", cell, upWord(cond.wantUp))
				}
				return awaitEval{}, ctx.Err()
			case ev := <-events:
				if !streamAlive.Load() || !eventRelevant(ev) {
					continue
				}
				repoll = true
			case <-tick.C:
				repoll = true
			}
		}
	}
}

// awaitSuccessLine is the ONE rendering of a satisfied wait. The
// terminal line and the --notify push are both built from it, for the
// same reason condResult exists: two surfaces describing one wait
// differently is how a qualification gets dropped from the one a human
// actually reads.
func awaitSuccessLine(cell string, up bool, ev awaitEval) string {
	return fmt.Sprintf("%s is %s%s", cell, upWord(up), successSuffix(ev))
}

// successSuffix appends the conditions beyond reachability to the
// success line, so the script's log records what was actually true.
func successSuffix(ev awaitEval) string {
	if len(ev.extra) == 0 {
		return ""
	}
	return ": " + strings.Join(ev.extra, "; ")
}

// evaluateAwait runs one /state poll and judges every condition against
// that one document.
func evaluateAwait(ctx context.Context, target fleetdTarget, cell string, cond awaitConds) (awaitEval, error) {
	snap, err := target.fetchState(ctx)
	if err != nil {
		return awaitEval{}, err
	}
	return cond.evaluate(snap, cell)
}

// fleetEvent is the CLI's minimal decode of one /api/fleet/events frame.
type fleetEvent struct {
	Cell string `json:"cell"`
	Type string `json:"type"`
}

// errUnknownCell separates "fleetd answered and has never heard of this
// cell" from a transport error. The first can only be a typo and must
// fail fast even with --timeout 0; the second is a restarting fleetd and
// must keep retrying, which is the whole point of the loop.
var errUnknownCell = errors.New("unknown cell")

// streamFleetEvents reads the SSE stream, decoding data: frames until
// ctx or the stream dies. An establishment failure or mid-stream error
// returns non-nil so the caller falls back to polling.
func streamFleetEvents(ctx context.Context, target fleetdTarget, out chan<- fleetEvent) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.base+"/api/fleet/events", nil)
	if err != nil {
		return err
	}
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// No client timeout: the stream is long-lived by definition.
	hc := &http.Client{Transport: target.hc.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events: HTTP %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20) // llama-swap's logData frames run megabytes
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev fleetEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &ev); err != nil {
			continue // non-JSON frame (comment, keepalive): not a transition
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}
