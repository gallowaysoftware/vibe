package cli

import (
	"bufio"
	"context"
	"encoding/json"
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
	cmd.AddCommand(cellStatusCmd(), cellAwaitCmd(), cellDrainCmd(), cellResumeCmd(), cellWakeCmd())
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
	var apiFlag string
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
				return renderDegraded(out, err)
			}
			renderStatus(out, target.base, snap)
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
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
		cellUp, models := probeCellDirect(c.URL)
		host := "-"
		if c.HostProbe != "" {
			if probeTCPDirect(c.HostProbe) {
				host = "up"
			} else {
				host = "down"
			}
		}
		cell := "down"
		if cellUp {
			cell = "up"
		}
		fmt.Fprintf(out, "%-14s %-10s %-10s %s\n", termSafe(n), cell, host, termSafe(models))
	}
	return nil
}

func probeCellDirect(url string) (bool, string) {
	hc := &http.Client{Timeout: 3 * time.Second}
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	resp, err := hc.Get(strings.TrimRight(url, "/") + "/v1/models")
	if err != nil {
		return false, "-"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "-"
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return true, "(catalog unreadable)"
	}
	ids := make([]string, 0, len(wrap.Data))
	for _, m := range wrap.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return true, strings.Join(ids, " ")
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
		timeout  time.Duration
		interval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "await <cell>",
		Short: "Block until a cell is reachable (--up) or unreachable (--down).",
		Long: "Park a script on fleet state: `vibe cell await gpu-cell --up && ./overnight-batch.sh`.\n" +
			"--up means the cell's llama-swap answers; intent is deliberately not consulted (routing truth rule).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if down {
				up = false
			} else {
				up = true // default
			}
			if interval <= 0 {
				return fmt.Errorf("--interval must be > 0")
			}
			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			return awaitCell(ctx, cmd.OutOrStdout(), target, args[0], up, timeout, interval)
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	cmd.Flags().BoolVar(&up, "up", false, "wait until the cell answers (default)")
	cmd.Flags().BoolVar(&down, "down", false, "wait until the cell stops answering")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (0 = wait forever)")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "poll interval")
	return cmd
}

// awaitCell blocks until the named cell's reachability matches wantUp.
// C3 made transitions subscribable: it rides /api/fleet/events when the
// stream works (cellUp/cellDown, cellReturned/cellStale/cellWithdrawn)
// and falls back to the 5s poll when it doesn't. Intent is never
// consulted — a drained cell that answers is up (routing truth rule).
func awaitCell(ctx context.Context, out io.Writer, target fleetdTarget, cell string, wantUp bool, timeout, interval time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
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

	matches := func(reachable bool) bool { return reachable == wantUp }
	eventMatches := func(ev fleetEvent) bool {
		if ev.Cell != cell {
			return false
		}
		switch ev.Type {
		case "fleet.cellUp", "fleet.cellReturned":
			return wantUp
		case "fleet.cellDown", "fleet.cellStale", "fleet.cellWithdrawn":
			return !wantUp
		}
		return false
	}

	for {
		ok, err := checkCellReachable(ctx, target, cell, matches)
		if err == nil && ok {
			state := "up"
			if !wantUp {
				state = "down"
			}
			fmt.Fprintf(out, "%s is %s\n", cell, state)
			return nil
		}
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(out, "await %s: %v (retrying)\n", cell, err)
		}
		select {
		case <-ctx.Done():
			if timeout > 0 {
				return fmt.Errorf("timeout waiting for %s to come %s", cell, map[bool]string{true: "up", false: "down"}[wantUp])
			}
			return ctx.Err()
		case ev := <-events:
			if streamAlive.Load() && eventMatches(ev) {
				state := "up"
				if !wantUp {
					state = "down"
				}
				fmt.Fprintf(out, "%s is %s (%s)\n", cell, state, ev.Type)
				return nil
			}
		case <-tick.C:
		}
	}
}

// fleetEvent is the CLI's minimal decode of one /api/fleet/events frame.
type fleetEvent struct {
	Cell string `json:"cell"`
	Type string `json:"type"`
}

// checkCellReachable runs one /state poll for the cell.
func checkCellReachable(ctx context.Context, target fleetdTarget, cell string, matches func(bool) bool) (bool, error) {
	snap, err := target.fetchState(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range snap.Cells {
		if c.Name == cell {
			return matches(c.Reachable), nil
		}
	}
	return false, fmt.Errorf("unknown cell %q (not in fleetd's registry)", cell)
}

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
