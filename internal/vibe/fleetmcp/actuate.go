package fleetmcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// The C2 actuation tools. fleetd drives a cell by calling THAT cell's
// daemon (daemon_url + token_file from hosts.yaml) — remote reach is a
// client call, not routing — and writes intent at fleetd only after
// success (the fleetd-invoked writer of the one-writer rule).

// cellClient builds a vibeclient for the named cell's daemon. Token
// resolution: cells.X.token_file (a path — the value never enters a
// repo; an unreadable one is a typed error, not a silent 401) →
// $VIBE_TOKEN → the local token file (right only when X is the local
// box). A cell without daemon_url gets (nil, nil): the announce path is
// its actuation channel (C3 — daemon_url is an optimization, not a
// requirement).
//
// The order deliberately DIVERGES from the CLI's (cmd_cell_actuate.go),
// which puts $VIBE_TOKEN first: a human typing one command means the
// override, but in a long-lived fleetd one env var must not silently
// void every per-cell token_file in hosts.yaml.
func (s *Server) cellClient(cell string) (*vibeclient.Client, error) {
	c, ok := s.hosts.Cells[cell]
	if !ok {
		return nil, fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
	}
	if c.DaemonURL == "" {
		return nil, nil
	}
	envToken := strings.TrimSpace(os.Getenv("VIBE_TOKEN"))
	var token string
	switch {
	case c.TokenFile != "":
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("cells.%s token_file %s: %v", cell, c.TokenFile, err)
		}
		token = strings.TrimSpace(string(data))
		s.warnTokenShadowOnce(cell, envToken != "")
	case envToken != "":
		token = envToken
	default:
		token = vibeclient.ResolveToken()
	}
	return vibeclient.NewWithToken(c.DaemonURL, token), nil
}

// warnTokenShadowOnce logs once when both $VIBE_TOKEN and a per-cell
// token_file are present, so the precedence is discoverable from a log
// rather than from a 401.
func (s *Server) warnTokenShadowOnce(cell string, envSet bool) {
	if !envSet {
		return
	}
	s.tokenWarnOnce.Do(func() {
		slog.Info("both $VIBE_TOKEN and cells.*.token_file are set; the per-cell file wins in fleetd", "cell", cell)
	})
}

// drainViaAnnounce records the drain request for the announce path:
// the cell reconciles it on its next heartbeat (executing its own
// cell_cmds), and surfaces show "drain requested, awaiting cell ack"
// until the echo lands. Used when the cell has no daemon_url — after
// C3, commissioning a cell never requires fleetd to reach it inbound.
func (s *Server) drainViaAnnounce(cell, reason, eta string) (string, error) {
	if _, err := s.fleet.SetIntent(cell, "drained", reason, eta); err != nil {
		return "", fmt.Errorf("record drain request: %v", err)
	}
	announcing := false
	if p := s.fleet.PresenceFor(cell); p != nil && p.Announcing {
		announcing = true
	}
	if !announcing {
		return fmt.Sprintf("Drain requested for %s (reason: %s) — but the cell is not announcing, so the request sits pending until it does. Consider cell_cmds via a local daemon or SSH.", cell, reason), nil
	}
	return fmt.Sprintf("Drain requested for %s via announce (reason: %s). The cell executes on its next heartbeat (≤ its announce interval) and echoes back; status shows 'requested' until the ack. A human at the box can override with `vibe cell resume`.", cell, reason), nil
}

// toolDrainCell drives the drain: interactive RPC when the cell has a
// daemon_url, else the desired-intent announce path (the cell picks the
// request up next heartbeat and reconciles — the human at the box can
// still override it). Intent records at fleetd after success on both
// paths (the fleetd-invoked writer of the one-writer rule).
func (s *Server) toolDrainCell(ctx context.Context, cell, reason, eta string, waitSeconds int32) (string, error) {
	client, err := s.cellClient(cell)
	if err != nil {
		return "", err
	}
	if client == nil {
		return s.drainViaAnnounce(cell, reason, eta)
	}
	report, err := client.CellDrain(ctx, reason, eta, waitSeconds)
	if err != nil {
		return "", fmt.Errorf("drain %s: %v", cell, err)
	}
	if _, err := s.fleet.SetIntent(cell, "drained", reason, eta); err != nil {
		// The drain happened; the record failing is a status-surface
		// problem, not a reason to misreport the verb. Loud either way.
		return "", fmt.Errorf("drained %s but failed to record intent at fleetd: %v", cell, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Drained %s", cell)
	if reason != "" {
		fmt.Fprintf(&b, " (reason: %s", reason)
		if eta != "" {
			fmt.Fprintf(&b, ", eta %s", eta)
		}
		b.WriteString(")")
	}
	b.WriteString(". Intent recorded.\nPre-drain report:")
	if len(report.ResidentModels) > 0 {
		fmt.Fprintf(&b, "\n- resident at drain: %s", strings.Join(report.ResidentModels, ", "))
	} else {
		b.WriteString("\n- no resident models")
	}
	if report.InFlightRequests != nil {
		fmt.Fprintf(&b, "\n- in-flight requests at drain: %d", *report.InFlightRequests)
	}
	switch {
	case report.WaitStatus == nil || *report.WaitStatus == fleetapi.DrainWaitNotRequested:
	case *report.WaitStatus == fleetapi.DrainWaitSkippedNoInflight:
		b.WriteString("\n- WARNING: the requested wait was SKIPPED (the cell never reported in-flight counts); the drain ran immediately and cancelled any streams")
	case *report.WaitStatus == fleetapi.DrainWaitWaited:
		b.WriteString("\n- waited for in-flight requests to reach zero before draining")
	}
	if report.LeasesUnavailable {
		b.WriteString("\n- WARNING: lease list was unavailable — stranded-work check could not run")
	}
	for _, l := range report.ActiveLeases {
		b.WriteString("\n- " + leaseLine(l))
	}
	return b.String(), nil
}

// leaseLine renders one pre-drain lease row. A C11 hold rides this list
// under the reserved holder (the RPC report has no hold flag — C11 added
// no proto field), and "hold holds glm-5" tells an agent nothing;
// "somebody is mid-evaluation on this box" is the sentence the pre-drain
// report exists for. Split out so the rendering has a test that does not
// need a live cell daemon.
func leaseLine(l *vibev1.LeaseView) string {
	note := l.Note
	if note == "" {
		note = "(no note)"
	}
	at := l.ExpiresAt.AsTime().Local().Format("15:04")
	if l.Holder == fleetapi.HoldHolder {
		return fmt.Sprintf("HELD: %s — %s (hold expires %s; the drain proceeds and evicts it)", l.Model, note, at)
	}
	return fmt.Sprintf("lease: %s holds %s — %s (expires %s)", l.Holder, l.Model, note, at)
}

// toolResumeCell drives the resume RPC, or the desired-intent announce
// path when the cell has no daemon_url. Clears intent at fleetd.
func (s *Server) toolResumeCell(ctx context.Context, cell string) (string, error) {
	client, err := s.cellClient(cell)
	if err != nil {
		return "", err
	}
	if client == nil {
		if _, err := s.fleet.SetIntent(cell, "serving", "", ""); err != nil {
			return "", fmt.Errorf("record resume request: %v", err)
		}
		return fmt.Sprintf("Resume requested for %s via announce (the cell resumes on its next heartbeat).", cell), nil
	}
	if err := client.CellResume(ctx); err != nil {
		return "", fmt.Errorf("resume %s: %v", cell, err)
	}
	if _, err := s.fleet.SetIntent(cell, "serving", "", ""); err != nil {
		return "", fmt.Errorf("resumed %s but failed to clear intent at fleetd: %v", cell, err)
	}
	return fmt.Sprintf("Resumed %s. Models return by JIT on next request; intent cleared.", cell), nil
}

// toolWakeCell shares the HTTP endpoint's exact delivery path (packet,
// or the per-cell fallback command when L2 is unreachable from fleetd).
func (s *Server) toolWakeCell(ctx context.Context, cell string) (string, error) {
	resp, err := s.fleet.SendWake(ctx, cell)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Wake sent to %s (%s → %s). Give it a minute, then check fleet_status.", cell, resp.Sent, resp.Target), nil
}

// toolRenderFront dry-runs `vibe router render --cell front` in-process:
// same renderer, same inputs, no shell-out. Returns the unified diff
// against the mounted live config, or the full render when fleetd can't
// see the front's file. Apply stays deliberately absent: fleetd's
// presence-driven render loop OWNS the write path, and two writers to
// one -watch-config file is exactly what its atomic-write contract
// forbids. This tool is the "what would change" question, not a lever.
func (s *Server) toolRenderFront(ctx context.Context, dryRun *bool) (string, error) {
	_ = ctx // the dry-run path has no cancellation semantics of its own
	if dryRun != nil && !*dryRun {
		return "", fmt.Errorf("render_front is dry-run-only: the presence-driven render loop owns the write path (a second writer breaks the atomic-write contract)")
	}
	var warnings []string
	opts := router.Options{
		Cell:              fleetcfg.FrontCell,
		Hosts:             s.hosts,
		LlamaServerBinary: s.llamaBinary,
		// Same extras default as the CLI (missing file = none), so the
		// dry-run diff can't disagree with a CLI render that merged them.
		ExtrasPath: filepath.Join(paths.ConfigHome(), "router-extras.yaml"),
		Warnf: func(format string, args ...any) {
			warnings = append(warnings, fmt.Sprintf(format, args...))
		},
	}
	var b strings.Builder
	if s.frontConfig != "" {
		out, err := router.RenderToFile(s.backendsDir, s.frontConfig, opts, false)
		if err != nil {
			return "", fmt.Errorf("render front: %v", err)
		}
		if !out.Changed {
			b.WriteString("No drift: the live front config already matches the render.\n")
		} else {
			b.WriteString("DRY RUN — diff against the live front config (not applied):\n")
			b.WriteString(out.Diff)
		}
	} else {
		defs, err := router.LoadDefs(s.backendsDir)
		if err != nil {
			return "", err
		}
		rendered, err := router.Render(defs, opts)
		if err != nil {
			return "", fmt.Errorf("render front: %v", err)
		}
		b.WriteString("DRY RUN — rendered front config (no live path mounted for a diff):\n")
		b.WriteString(rendered)
	}
	for _, w := range warnings {
		b.WriteString("\nwarning: " + w)
	}
	return b.String(), nil
}
