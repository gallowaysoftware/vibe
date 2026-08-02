package fleetmcp

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// The C2 actuation tools. fleetd drives a cell by calling THAT cell's
// daemon (daemon_url + token_file from hosts.yaml) — remote reach is a
// client call, not routing — and writes intent at fleetd only after the
// RPC succeeds (the fleetd-invoked writer of the one-writer rule).

// cellClient builds a vibeclient for the named cell's daemon. Token
// resolution mirrors the CLI's documented order: $VIBE_TOKEN (explicit
// override) → cells.X.token_file (a path — the value never enters a
// repo; an unreadable one is a typed error, not a silent 401) → the
// local token file (right only when X is the local box). A cell without
// daemon_url is a typed error: C2 actuation needs the cell's control
// plane (C3's piggyback removes the requirement; until then daemon_url
// is how you reach it).
func (s *Server) cellClient(cell string) (*vibeclient.Client, error) {
	c, ok := s.hosts.Cells[cell]
	if !ok {
		return nil, fmt.Errorf("unknown cell %q (not in hosts.yaml)", cell)
	}
	if c.DaemonURL == "" {
		return nil, fmt.Errorf("cells.%s has no daemon_url in hosts.yaml — actuation needs the cell's control plane", cell)
	}
	token := strings.TrimSpace(os.Getenv("VIBE_TOKEN"))
	if token == "" && c.TokenFile != "" {
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("cells.%s token_file %s: %v", cell, c.TokenFile, err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		token = vibeclient.ResolveToken()
	}
	return vibeclient.NewWithToken(c.DaemonURL, token), nil
}

// toolDrainCell drives the drain RPC and, on success, records intent at
// fleetd — one writer, and only after the drain actually happened. The
// pre-drain report comes back as text so the agent can relay lease
// warnings before a human confirms.
func (s *Server) toolDrainCell(ctx context.Context, cell, reason, eta string, waitSeconds int32) (string, error) {
	client, err := s.cellClient(cell)
	if err != nil {
		return "", err
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
	if report.LeasesUnavailable {
		b.WriteString("\n- WARNING: lease list was unavailable — stranded-work check could not run")
	}
	for _, l := range report.ActiveLeases {
		note := l.Note
		if note == "" {
			note = "(no note)"
		}
		fmt.Fprintf(&b, "\n- lease: %s holds %s — %s (expires %s)", l.Holder, l.Model, note,
			l.ExpiresAt.AsTime().Local().Format("15:04"))
	}
	return b.String(), nil
}

// toolResumeCell drives the resume RPC and clears intent at fleetd.
func (s *Server) toolResumeCell(ctx context.Context, cell string) (string, error) {
	client, err := s.cellClient(cell)
	if err != nil {
		return "", err
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
// see the front's file. Apply is deliberately absent in C2 — the
// presence-driven render loop (C3) owns the write path and its mount
// contract.
func (s *Server) toolRenderFront(ctx context.Context, dryRun *bool) (string, error) {
	_ = ctx // the dry-run path has no cancellation semantics of its own
	if dryRun != nil && !*dryRun {
		return "", fmt.Errorf("render_front is dry-run-only in C2 — the presence-driven apply path arrives with C3")
	}
	var warnings []string
	opts := router.Options{
		Cell:              fleetcfg.FrontCell,
		Hosts:             s.hosts,
		LlamaServerBinary: s.llamaBinary,
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
