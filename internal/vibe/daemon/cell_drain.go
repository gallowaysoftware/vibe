package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// The fleet-control C2 cell verbs. CellDrain/CellResume act on the
// daemon's OWN local cell: remote reach comes from calling a remote
// daemon, never from routing here. One intent writer per invocation
// path: fleetd-invoked (TCP) drains are recorded by fleetd after the RPC
// succeeds; locally-invoked (unix socket) drains are posted to fleetd by
// this daemon, best-effort. A failed drain never records intent.

// remoteInvocationKey marks RPCs that arrived via the TCP listener.
type remoteInvocationKey struct{}

// markRemote wraps the TCP listener's handler chain so CellDrain/
// CellResume can tell fleetd-invoked calls from local unix-socket ones.
func markRemote(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteInvocationKey{}, true)))
	})
}

func isRemoteInvocation(ctx context.Context) bool {
	v, _ := ctx.Value(remoteInvocationKey{}).(bool)
	return v
}

// cellCmdTimeout bounds drain/resume commands: a hung unit stop must
// surface as Unavailable, not a stuck RPC.
const cellCmdTimeout = 60 * time.Second

// CellDrain runs the configured drain command and returns the pre-drain
// report gathered just before it. The report is best-effort by design:
// resident models come from the local llama-swap's /running when it
// answers, in-flight from the fleet watcher's inflight tracking when the
// events stream has reported, and leases from fleetd when a registry is
// configured — absent sources omit the field rather than invent it.
func (d *Daemon) CellDrain(ctx context.Context, req *connect.Request[vibev1.CellDrainRequest]) (*connect.Response[vibev1.CellDrainResponse], error) {
	if d.cfg.CellCmds.Drain == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("this daemon has no cell verbs configured (cell_cmds.drain is unset)"))
	}

	report := &vibev1.CellDrainResponse{ActiveLeases: []*vibev1.LeaseView{}}
	report.ResidentModels = d.probeResidentModels(ctx)
	if d.fleet != nil {
		if n, ok := d.fleet.InFlight("front"); ok {
			n64 := int64(n)
			report.InFlightRequests = &n64
		}
	}
	if d.cfg.Fleet.RegistryURL != "" && d.cfg.Fleet.Cell != "" {
		leases, err := d.fetchCellLeases(ctx)
		if err != nil {
			slog.Warn("pre-drain lease fetch failed; proceeding without the lease list",
				"registry", d.cfg.Fleet.RegistryURL, "err", err)
			report.LeasesUnavailable = true
		} else {
			report.ActiveLeases = leases
		}
	}

	// Quiescence wait: llama-swap's SIGTERM path cancels in-flight
	// streams immediately (v239: CloseStreams runs before the graceful
	// drain), so "let the stream finish" only happens BEFORE the unit
	// stop. Without --wait the drain is immediate and in-flight work
	// dies truncated — the report above is what warned the caller.
	if req.Msg.WaitSeconds > 0 {
		drainCtx, cancel := context.WithTimeout(ctx, time.Duration(req.Msg.WaitSeconds)*time.Second)
		defer cancel()
		if err := d.awaitQuiescence(drainCtx); err != nil {
			return nil, connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("in-flight requests did not finish within %ds: %v (drain NOT run; retry with a longer wait or none)", req.Msg.WaitSeconds, err))
		}
	}

	vctx, cancelVerb := verbCtx(ctx)
	defer cancelVerb()
	if stderr, err := d.cellCmdRunner(vctx, d.cfg.CellCmds.Drain); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("drain command failed: %v: %s", err, strings.TrimSpace(stderr)))
	}

	if !isRemoteInvocation(ctx) {
		d.noteLocalIntent("drained")
		d.postIntentBestEffort(fleetapi.Intent{
			State:  "drained",
			Reason: req.Msg.Reason,
			ETA:    req.Msg.Eta,
		})
	}
	return connect.NewResponse(report), nil
}

// CellResume runs the configured resume command. Models return by JIT on
// next request — resume deliberately does not preload.
func (d *Daemon) CellResume(ctx context.Context, _ *connect.Request[vibev1.CellResumeRequest]) (*connect.Response[vibev1.CellResumeResponse], error) {
	if d.cfg.CellCmds.Resume == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("this daemon has no cell verbs configured (cell_cmds.resume is unset)"))
	}
	vctx, cancelVerb := verbCtx(ctx)
	defer cancelVerb()
	if stderr, err := d.cellCmdRunner(vctx, d.cfg.CellCmds.Resume); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("resume command failed: %v: %s", err, strings.TrimSpace(stderr)))
	}
	if !isRemoteInvocation(ctx) {
		d.noteLocalIntent("serving")
		d.postIntentBestEffort(fleetapi.Intent{State: "serving"})
	}
	return connect.NewResponse(&vibev1.CellResumeResponse{}), nil
}

// awaitQuiescence polls the fleet watcher's inflight tracking until the
// local cell reports zero in-flight requests. The count is SSE-driven
// (llama-swap's inflight frames), so an unreachable or non-reporting
// cell reads as "never reported" — that case returns immediately:
// draining a cell we can't measure is the operator's explicit choice
// (they set --wait; the absence of data must not hang the verb).
func (d *Daemon) awaitQuiescence(ctx context.Context) error {
	if d.fleet == nil {
		return nil
	}
	if _, reported := d.fleet.InFlight("front"); !reported {
		slog.Info("no inflight report from the local cell; proceeding without waiting")
		return nil
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		n, _ := d.fleet.InFlight("front")
		if n == 0 {
			return nil
		}
		slog.Info("drain waiting for in-flight requests", "in_flight", n)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// verbCtx builds the context every cell verb runs under: detached from
// the RPC's cancellation (a client disconnecting mid-verb must not
// SIGKILL a unit stop in flight — the verb is exactly as atomic as the
// process regime makes it, and the caller's presence changes nothing
// about that), bounded by cellCmdTimeout.
func verbCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cellCmdTimeout)
}

// runCellCmd executes a cell verb via sh -c, returning captured stderr for
// the error path. Assigned to Daemon.cellCmdRunner in New so tests can
// script verb outcomes.
func runCellCmd(ctx context.Context, cmd string) (string, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	err := c.Run()
	return stderr.String(), err
}

// probeResidentModels lists models the local llama-swap currently has
// loaded (any non-stopped state). An absent or non-llama-swap router
// degrades to an empty list — /running is llama-swap's endpoint.
func (d *Daemon) probeResidentModels(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var wrap struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(d.cfg.ProxyPort))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/running", nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil
	}
	var out []string
	for _, m := range wrap.Running {
		if m.State != "stopped" {
			out = append(out, m.Model)
		}
	}
	return out
}

// fetchCellLeases pulls this cell's active advisory leases from fleetd —
// the "would I strand a batch job?" half of the pre-drain report. Bounded
// like the other pre-drain probes: a wedged fleetd must degrade the
// report (leases_unavailable), never block the verb.
func (d *Daemon) fetchCellLeases(ctx context.Context) ([]*vibev1.LeaseView, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	base := strings.TrimRight(d.cfg.Fleet.RegistryURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/fleet/leases?cell="+url.QueryEscape(d.cfg.Fleet.Cell), nil)
	if err != nil {
		return nil, err
	}
	if tok := d.fleetdToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET leases: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var wrap struct {
		Leases []fleetapi.Lease `json:"leases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	out := make([]*vibev1.LeaseView, 0, len(wrap.Leases))
	for _, l := range wrap.Leases {
		out = append(out, &vibev1.LeaseView{
			Cell:      l.Cell,
			Model:     l.Model,
			Holder:    l.Holder,
			Note:      l.Note,
			ExpiresAt: timestamppb.New(l.ExpiresAt),
		})
	}
	return out, nil
}

// postIntentBestEffort records intent at fleetd after a successful
// locally-invoked drain/resume. Best-effort by design (the C2 rule):
// fleetd being down must not fail the verb itself, so failures log and
// move on — a missing entry degrades the display to DRAINED?, which a
// human can correct with one POST.
//
// When this daemon announces (C3), the POST is skipped: the announce
// echo IS the cell-side record, and a POST would stamp a NEWER request
// than the cell's own — a ghost 'requested, awaiting ack' that only
// the next heartbeat could clear.
func (d *Daemon) postIntentBestEffort(intent fleetapi.Intent) {
	if d.announce != nil {
		return
	}
	if d.cfg.Fleet.RegistryURL == "" || d.cfg.Fleet.Cell == "" {
		slog.Info("no fleet registry configured; intent not recorded", "state", intent.State)
		return
	}
	intent.Since = time.Now().UTC()
	body, err := json.Marshal(map[string]any{
		"cell":   d.cfg.Fleet.Cell,
		"state":  intent.State,
		"reason": intent.Reason,
		"eta":    intent.ETA,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.cfg.Fleet.RegistryURL, "/")+"/api/fleet/intent",
		bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := d.fleetdToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("intent post failed (fleetd unreachable?)", "state", intent.State, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		slog.Warn("intent post rejected", "state", intent.State, "status", resp.StatusCode, "body", strings.TrimSpace(string(b)))
	}
}

// fleetdToken reads the fleetd bearer token from the configured file.
// Read per call so a rotated token needs no daemon restart.
func (d *Daemon) fleetdToken() string {
	if d.cfg.Fleet.TokenFile == "" {
		return ""
	}
	data, err := os.ReadFile(d.cfg.Fleet.TokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
