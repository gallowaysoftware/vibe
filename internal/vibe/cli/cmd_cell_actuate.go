package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// `vibe cell drain|resume|wake` — the actuation half of fleet-control
// C2. Local verbs run against the local daemon over the unix socket;
// remote verbs reach the named cell's daemon via hosts.yaml
// (daemon_url + per-cell token). Intent is written exactly once per
// invocation path: fleetd/MCP-invoked drains are recorded by fleetd;
// locally-invoked ones by the cell daemon itself (best-effort).

func cellDrainCmd() *cobra.Command {
	var (
		reason, eta string
		yes         bool
		untilExit   bool
		wait        time.Duration
	)
	cmd := &cobra.Command{
		Use:   "drain [cell] [-- command...]",
		Short: "Reclaim a cell (stop its serving stack) with a pre-drain report.",
		Long: "Drain reclaims a cell: its unit stops (llama-swap's SIGTERM cancels in-flight " +
			"streams immediately — only --wait lets generations finish first), " +
			"requests for its ids fail as UPSTREAM_DOWN, and intent (reason/eta) is recorded. " +
			"With --until-exit, drain, run the command, then resume deterministically on its exit " +
			"— the gaming-session wrapper. Resume is never triggered by a GPU-idle heuristic.",
		Args: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if untilExit {
				if dash < 0 || dash == len(args) {
					return fmt.Errorf("--until-exit needs a command after `--`")
				}
				if dash > 1 {
					return fmt.Errorf("at most one optional cell before `--`")
				}
				return nil
			}
			if dash >= 0 {
				return fmt.Errorf("arguments after `--` require --until-exit")
			}
			return cobra.MaximumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			cell, command := parseDrainArgs(args, cmd.ArgsLenAtDash())
			out := cmd.OutOrStdout()

			if untilExit {
				return drainUntilExit(ctx, out, cell, reason, eta, yes, wait, command)
			}
			return drainCell(ctx, out, cell, reason, eta, yes, wait)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the cell is being reclaimed (recorded as intent)")
	cmd.Flags().StringVar(&eta, "eta", "", "when it's expected back (recorded as intent)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt when leases are active")
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for in-flight requests to finish before draining (0 = immediate; llama-swap's SIGTERM cancels streams immediately, so only a wait lets generations finish)")
	cmd.Flags().BoolVar(&untilExit, "until-exit", false, "resume the cell when the command after `--` exits (any exit code)")
	return cmd
}

func cellResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume [cell]",
		Short: "Resume a drained cell (JIT service returns on next request).",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			var cell string
			if len(args) > 0 {
				cell = args[0]
			}
			client, name, err := resolveCellClient(cell)
			if err != nil {
				return err
			}
			if err := client.CellResume(ctx); err != nil {
				return fmt.Errorf("resume %s: %w", displayCell(name), err)
			}
			if cell != "" {
				postCLIIntent(cmd.OutOrStdout(), cell, "serving", "", "")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s resumed — models return by JIT on next request.\n", displayCell(name))
			return nil
		},
	}
}

func cellWakeCmd() *cobra.Command {
	var apiFlag string
	cmd := &cobra.Command{
		Use:   "wake <cell>",
		Short: "Send a Wake-on-LAN packet to a cell (always explicit, never automatic).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			return wakeCell(ctx, cmd.OutOrStdout(), apiFlag, args[0])
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	return cmd
}

func displayCell(name string) string {
	if name == "" {
		return "local cell"
	}
	return name
}

// parseDrainArgs splits the drain command line into the optional cell
// (before `--`) and the until-exit command (after it). cobra's
// ArgsLenAtDash counts the arguments BEFORE the separator, so the
// command always starts at args[dash] — dash==0 means no cell was
// named, dash==1 means args[0] was the cell.
func parseDrainArgs(args []string, dash int) (cell string, command []string) {
	if dash < 0 {
		if len(args) > 0 {
			cell = args[0]
		}
		return cell, nil
	}
	if dash > 0 {
		cell = args[0]
	}
	return cell, args[dash:]
}

// resolveCellClient builds the client for a drain/resume target: empty
// cell means the local daemon (unix socket); a named cell resolves via
// hosts.yaml daemon_url with the token resolution order $VIBE_TOKEN →
// cells.X.token_file → local token file.
func resolveCellClient(cell string) (*vibeclient.Client, string, error) {
	if cell == "" {
		return newClient(), "", nil
	}
	hosts, err := fleetcfg.Load()
	if err != nil {
		return nil, "", fmt.Errorf("load hosts.yaml: %w", err)
	}
	if !hosts.HasCells() {
		return nil, "", fmt.Errorf("cell %q named but %s has no cells: section", cell, hostsPath())
	}
	c, ok := hosts.Cells[cell]
	if !ok {
		names := make([]string, 0, len(hosts.Cells))
		for n := range hosts.Cells {
			names = append(names, n)
		}
		return nil, "", fmt.Errorf("unknown cell %q (hosts.yaml has: %s)", cell, strings.Join(names, ", "))
	}
	if c.DaemonURL == "" {
		return nil, "", fmt.Errorf("cells.%s has no daemon_url in hosts.yaml — the remote verb needs the cell's control plane", cell)
	}
	// Precedence here deliberately DIFFERS from fleetmcp's (actuate.go):
	// for a human typing one command an explicit $VIBE_TOKEN must win, and
	// an unreadable token_file is a hard error rather than a silent
	// fallthrough to the local token — that swallow turned a typo'd path
	// into an opaque 401 from the remote cell.
	token := strings.TrimSpace(os.Getenv("VIBE_TOKEN"))
	switch {
	case token != "":
	case c.TokenFile != "":
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, "", fmt.Errorf("read cells.%s.token_file: %w", cell, err)
		}
		token = strings.TrimSpace(string(data))
	default:
		token = vibeclient.ResolveToken()
	}
	return vibeclient.NewWithToken(c.DaemonURL, token), cell, nil
}

func hostsPath() string {
	// fleetcfg doesn't export the path; keep the message honest via paths.
	return "~/.config/vibe/hosts.yaml"
}

// drainCell executes one drain: lease check + confirmation first, then
// the RPC, then the report.
func drainCell(ctx context.Context, out io.Writer, cell, reason, eta string, yes bool, wait time.Duration) error {
	client, name, err := resolveCellClient(cell)
	if err != nil {
		return err
	}
	leases := fetchLeasesForCell(ctx, cell)
	if len(leases) > 0 && !yes {
		printLeasePrompt(out, displayCell(name), leases)
		if !isatty.IsTerminal(os.Stdin.Fd()) {
			return fmt.Errorf("leases are active and stdin is not a terminal — re-run with --yes to drain anyway")
		}
		fmt.Fprintf(out, "drain %s anyway? [y/N] ", displayCell(name))
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil {
			// EOF (piped stdin, closed terminal) reads as "no": aborting is
			// the safe default when the operator can't actually be asked.
			fmt.Fprintln(out, "aborted")
			return nil
		}
		if strings.ToLower(strings.TrimSpace(answer)) != "y" {
			fmt.Fprintln(out, "aborted")
			return nil
		}
	}

	report, err := client.CellDrain(ctx, reason, eta, int32(wait.Seconds()))
	if err != nil {
		return fmt.Errorf("drain %s: %w", displayCell(name), err)
	}
	if cell != "" {
		// Remote invocation: the cell daemon writes no intent for TCP
		// callers (that path is fleetd's), and the CLI is not fleetd — so
		// the write lands here, or nowhere does.
		postCLIIntent(out, cell, "drained", reason, eta)
	}
	printDrainReport(out, name, report)
	return nil
}

// printLeasePrompt lists the advisory holds a drain would strand. A C11
// hold is a lease, but "hold holds glm-5" is not the sentence this
// prompt exists to say to an operator about to take the box: somebody is
// mid-evaluation on it. Holds still block nothing.
func printLeasePrompt(out io.Writer, name string, leases []fleetapi.Lease) {
	fmt.Fprintf(out, "active leases on %s:\n", name)
	for _, l := range leases {
		if l.Hold {
			fmt.Fprintf(out, "  - HELD: %s until %s%s (a drain evicts it; the hold does not block you)\n",
				termSafe(l.Model), l.ExpiresAt.Local().Format("15:04"), noteSuffix(l.Note))
			continue
		}
		fmt.Fprintf(out, "  - %s holds %s: %s (expires %s)\n",
			termSafe(l.Holder), termSafe(l.Model), termSafe(l.Note), l.ExpiresAt.Local().Format("15:04"))
	}
}

// postCLIIntent records intent at fleetd after a CLI-driven remote verb.
// Best-effort: the verb already happened, so a failed write warns rather
// than fails — a missing entry degrades the display to DRAINED?, which a
// human can correct with one POST.
func postCLIIntent(out io.Writer, cell, state, reason, eta string) {
	target, err := resolveFleetd("")
	if err != nil {
		fmt.Fprintf(out, "warning: intent not recorded (fleetd resolution: %v)\n", err)
		return
	}
	body, _ := json.Marshal(map[string]string{"cell": cell, "state": state, "reason": reason, "eta": eta})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target.base+"/api/fleet/intent", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := target.hc.Do(req)
	if err != nil {
		fmt.Fprintf(out, "warning: intent not recorded at fleetd (%v)\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(out, "warning: intent post rejected: HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

func printDrainReport(out io.Writer, name string, r *vibev1.CellDrainResponse) {
	fmt.Fprintf(out, "%s drained.\n", displayCell(name))
	if len(r.ResidentModels) > 0 {
		fmt.Fprintf(out, "  resident at drain: %s\n", strings.Join(r.ResidentModels, " "))
	}
	if r.InFlightRequests != nil {
		fmt.Fprintf(out, "  in-flight requests at drain: %d\n", *r.InFlightRequests)
	}
	switch {
	case r.WaitStatus == nil || *r.WaitStatus == fleetapi.DrainWaitNotRequested:
	case *r.WaitStatus == fleetapi.DrainWaitSkippedNoInflight:
		fmt.Fprintln(out, "  warning: --wait was SKIPPED (the cell never reported in-flight counts) — the drain ran immediately and cancelled any streams")
	case *r.WaitStatus == fleetapi.DrainWaitWaited:
		fmt.Fprintln(out, "  waited for in-flight requests to reach zero before draining")
	}
	if r.LeasesUnavailable {
		fmt.Fprintln(out, "  warning: lease list unavailable — stranded-work check could not run")
	}
	for _, l := range r.ActiveLeases {
		// The RPC report carries no hold flag (C11 added no proto field),
		// but the reserved holder is a deterministic key — that is what it
		// is FOR. Without this the same drain prints "HELD: glm-5" in the
		// prompt and "hold holds glm-5" three lines later.
		if l.Holder == fleetapi.HoldHolder {
			fmt.Fprintf(out, "  HELD: %s%s (a drain evicts it; the hold does not block you)\n",
				termSafe(l.Model), noteSuffix(l.Note))
			continue
		}
		fmt.Fprintf(out, "  lease: %s holds %s: %s\n", termSafe(l.Holder), termSafe(l.Model), termSafe(l.Note))
	}
}

// drainUntilExit wraps a command with a deterministic drain/resume pair
// (the gaming session): drain, run, resume on ANY exit. The resume is
// deferred from the moment the drain succeeds, so every wrapper exit
// path — non-zero exit, panic, or SIGINT/SIGTERM to the wrapper itself
// (Ctrl-C targets the foreground process group) — still resumes. Only
// SIGKILL escapes it, which nothing can guard. An idle-GPU heuristic
// never gets a vote (design-rejected alternative).
func drainUntilExit(ctx context.Context, out io.Writer, cell, reason, eta string, yes bool, wait time.Duration, command []string) error {
	if cell != "" {
		return fmt.Errorf("--until-exit runs on the local box (resume-on-exit needs it); drain the remote cell separately")
	}
	if err := drainCell(ctx, out, "", reason, eta, yes, wait); err != nil {
		return err
	}
	// Resume is guaranteed from here on. Deferred first so a panic or a
	// signal-driven abort still fires it; the detached 70s context
	// survives the wrapper's own cancellation.
	resume := func() {
		resumeCtx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancel()
		client, _, err := resolveCellClient("")
		if err == nil {
			err = client.CellResume(resumeCtx)
		}
		if err != nil {
			fmt.Fprintf(out, "warning: resume failed (%v) — run `vibe cell resume` manually\n", err)
		} else {
			fmt.Fprintln(out, "cell resumed.")
		}
	}
	defer resume()

	// SIGINT/SIGTERM to the wrapper cancel sigCtx, which kills the child
	// (CommandContext) — c.Run() then returns and the deferred resume
	// runs. The default Go disposition (terminate immediately, skipping
	// defers) is exactly what NotifyContext replaces.
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(out, "running: %s\n", strings.Join(command, " "))
	c := exec.CommandContext(sigCtx, command[0], command[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := c.Run()

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitCodeError{msg: command[0], code: exitErr.ExitCode()}
	}
	return runErr
}

type exitCodeError struct {
	msg  string
	code int
}

func (e exitCodeError) Error() string { return fmt.Sprintf("%s exited with status %d", e.msg, e.code) }

// fetchLeasesForCell reads active leases for the pre-drain prompt from
// fleetd (best-effort: fleetd down = no leases to show, and the RPC's
// own report will note unavailability).
func fetchLeasesForCell(ctx context.Context, cell string) []fleetapi.Lease {
	name := cell
	if name == "" {
		cfg, err := daemon.LoadConfig()
		if err != nil || cfg.Fleet.Cell == "" {
			return nil
		}
		name = cfg.Fleet.Cell
	}
	target, err := resolveFleetd("")
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		target.base+"/api/fleet/leases?cell="+name, nil)
	if err != nil {
		return nil
	}
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := target.hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var wrap struct {
		Leases []fleetapi.Lease `json:"leases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil
	}
	return wrap.Leases
}

// wakeCell sends a wake via fleetd; unreachable fleetd degrades to
// sending the packet directly from this box (any LAN box can).
func wakeCell(ctx context.Context, out io.Writer, apiFlag, cell string) error {
	target, err := resolveFleetd(apiFlag)
	if err == nil {
		body, _ := json.Marshal(map[string]string{"cell": cell})
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, target.base+"/api/fleet/wake", strings.NewReader(string(body)))
		if rerr == nil {
			req.Header.Set("Content-Type", "application/json")
			if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			resp, derr := target.hc.Do(req)
			if derr == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var wr struct {
						Sent   string `json:"sent"`
						Target string `json:"target"`
					}
					_ = json.NewDecoder(resp.Body).Decode(&wr)
					fmt.Fprintf(out, "wake sent to %s via fleetd (%s → %s)\n", cell, wr.Sent, wr.Target)
					return nil
				}
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return fmt.Errorf("wake %s: HTTP %d: %s", cell, resp.StatusCode, strings.TrimSpace(string(b)))
			}
			err = derr
		} else {
			err = rerr
		}
	}

	// Degraded path: packet from this box, config straight from hosts.yaml.
	hosts, lerr := fleetcfg.Load()
	if lerr != nil || !hosts.HasCells() {
		return fmt.Errorf("fleetd unreachable (%v) and no hosts.yaml wake config for the direct path", err)
	}
	c, ok := hosts.Cells[cell]
	if !ok {
		return fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
	}
	if c.Wake == nil {
		return fmt.Errorf("cells.%s has no wake: config in hosts.yaml", cell)
	}
	if c.Wake.Cmd != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", c.Wake.Cmd)
		if out2, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("wake command failed: %v: %s", err, out2)
		}
		fmt.Fprintf(out, "wake sent to %s directly (cmd)\n", cell)
		return nil
	}
	if c.Wake.MAC == "" {
		return fmt.Errorf("cells.%s has no wake.mac in hosts.yaml", cell)
	}
	mac, err := net.ParseMAC(c.Wake.MAC)
	if err != nil {
		return fmt.Errorf("cells.%s wake.mac %q is invalid: %v", cell, c.Wake.MAC, err)
	}
	target2 := c.Wake.Broadcast
	if target2 == "" {
		target2 = "255.255.255.255:9"
	}
	if err := fleetapi.SendMagicPacket(target2, mac); err != nil {
		return fmt.Errorf("send magic packet: %w", err)
	}
	fmt.Fprintf(out, "wake sent to %s directly (packet → %s)\n", cell, target2)
	return nil
}
