package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// `vibe fleet doctor` (fleet-control C13): the sit-down-after-two-weeks
// command. fleetd computes the report (§3 of the phase doc — the
// credential that matters is the one FLEETD resolves, not the one this
// shell holds); this file renders it, adds the checks that are only true
// at the client, and degrades to a local-only report when fleetd itself
// is the thing that is down.
//
// Nothing here mutates. There is no --fix and there must not be one.

// exit codes. Not severity-ordered by accident: 1 and 2 are the classic
// "broken" and "suspicious", and 3 says the REPORT is incomplete, which
// is a different fact from the fleet being unwell.
const (
	doctorExitFail    = 1
	doctorExitWarn    = 2
	doctorExitUnknown = 3
)

// errDoctorLevel carries a doctor exit code out of RunE without cobra
// printing an error line — the report above it is the message.
type errDoctorLevel struct{ code int }

func (e errDoctorLevel) Error() string { return fmt.Sprintf("fleet doctor: exit %d", e.code) }

// ExitCode is read by main's error handler (same shape as the pipeline
// doctor's sentinel).
func (e errDoctorLevel) ExitCode() int { return e.code }

func fleetDoctorCmd() *cobra.Command {
	var (
		apiFlag  string
		asJSON   bool
		timeout  time.Duration
		omitOK   bool
		exitZero bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Audit the fleet's wiring: credentials, versions, disks, certs, hygiene.",
		Long: "Answers 'is the fleet still put together correctly' — the question fleet_status " +
			"does not ask. Every check reports OK / WARN / FAIL / UNKNOWN, and UNKNOWN means the " +
			"check could not be evaluated, never that it passed.\n\n" +
			"READ-ONLY: it drains, warms, unloads, probes, wakes and renders nothing, and is safe " +
			"to run mid-incident. Fixes are yours.\n\n" +
			"Exit codes: 0 all clear, 1 a FAIL, 2 a WARN, 3 only UNKNOWNs (the report is " +
			"incomplete).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			target, err := resolveFleetd(apiFlag)
			if err != nil {
				return err
			}
			rep, fetchErr := target.fetchDoctor(ctx)
			if fetchErr != nil {
				rep = degradedDoctor(target, fetchErr)
			}
			clientDoctorChecks(rep)
			rep.Fleetd = target.base

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				renderDoctor(out, rep, omitOK)
			}
			if exitZero {
				return nil
			}
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			switch {
			case rep.Summary.Fail > 0:
				return errDoctorLevel{doctorExitFail}
			case rep.Summary.Warn > 0:
				return errDoctorLevel{doctorExitWarn}
			case rep.Summary.Unknown > 0:
				return errDoctorLevel{doctorExitUnknown}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON (the same document /api/fleet/doctor serves)")
	cmd.Flags().BoolVar(&omitOK, "problems-only", false, "hide the OK block")
	cmd.Flags().BoolVar(&exitZero, "exit-zero", false, "always exit 0 (for a wrapper that reads the output itself)")
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "give up on the whole report after this long")
	return cmd
}

// fetchDoctor GETs the report from the resolved fleetd.
func (t fleetdTarget) fetchDoctor(ctx context.Context) (*fleetapi.DoctorReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/api/fleet/doctor", nil)
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
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rep fleetapi.DoctorReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, fmt.Errorf("decode doctor report: %w", err)
	}
	return &rep, nil
}

// degradedDoctor is the report when fleetd is the thing that is down —
// which is precisely when this command gets run (the fire drill starts
// by killing it). Every server-side check is UNKNOWN with the reason,
// and the checks that are true at the client still run.
func degradedDoctor(target fleetdTarget, fleetdErr error) *fleetapi.DoctorReport {
	rep := &fleetapi.DoctorReport{GeneratedAt: time.Now().UTC()}
	rep.Add(fleetapi.DoctorCheck{
		ID:      "fleetd.reachable",
		Level:   fleetapi.LevelFail,
		Summary: "fleetd did not answer at " + termSafe(target.base),
		Detail:  fleetdErr.Error(),
		Fix:     "inference is unaffected — fleetd is read-and-request-only (design invariant 4). Restart it, then re-run.",
	})
	rep.Add(fleetapi.DoctorCheck{
		ID:      "fleetd.checks",
		Level:   fleetapi.LevelUnknown,
		Summary: "every fleet-side check is unevaluated",
		Detail: "credentials, presence, versions, leases, fingerprints, probes, the ledger and the notifier all live " +
			"at fleetd. None of them was asked.",
	})

	hosts, err := fleetcfg.Load()
	switch {
	case err != nil:
		rep.Add(fleetapi.DoctorCheck{ID: "hosts.yaml", Level: fleetapi.LevelFail,
			Summary: "the cell registry does not parse: " + paths.HostsFile(),
			Detail:  err.Error(),
			Fix:     "fleetd refuses to start on an invalid hosts.yaml, so this is often why it is not coming back."})
		return rep
	case !hosts.HasCells():
		// Missing and present-but-cell-less are different problems and the
		// fix differs, so the message must not collapse them.
		why := "it declares no cells: section"
		if _, statErr := os.Stat(paths.HostsFile()); statErr != nil {
			why = "the file does not exist on this box"
		}
		rep.Add(fleetapi.DoctorCheck{ID: "hosts.yaml", Level: fleetapi.LevelWarn,
			Summary: "no cell registry at " + paths.HostsFile() + ": " + why,
			Detail:  "without it there is no fleet membership to check from here."})
		return rep
	}
	rep.Add(fleetapi.DoctorCheck{ID: "hosts.yaml", Level: fleetapi.LevelOK,
		Summary: fmt.Sprintf("%d cell(s) declared and the file validates", len(hosts.Cells))})

	names := make([]string, 0, len(hosts.Cells))
	for n := range hosts.Cells {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		up, _ := probeCellDirect(hosts.Cells[n].URL)
		if up {
			rep.Add(fleetapi.DoctorCheck{ID: "cell.direct", Cell: n, Level: fleetapi.LevelOK,
				Summary: "llama-swap answers directly (serving is unaffected by fleetd being down)"})
			continue
		}
		rep.Add(fleetapi.DoctorCheck{ID: "cell.direct", Cell: n, Level: fleetapi.LevelWarn,
			Summary: "no answer from " + termSafe(hosts.Cells[n].URL),
			Detail:  "probed directly from this box; intent and presence are unavailable without fleetd."})
	}
	return rep
}

// clientDoctorChecks adds the checks that are only true HERE. The
// $VIBE_TOKEN one is the live half of C6's NIT-E: the CLI's precedence
// is env-first by design, so an exported token in this shell silently
// replaces every cells.X.token_file for every command run from it.
func clientDoctorChecks(rep *fleetapi.DoctorReport) {
	env := strings.TrimSpace(os.Getenv("VIBE_TOKEN"))
	hosts, err := fleetcfg.Load()
	var withFile []string
	if err == nil && hosts != nil {
		for n, c := range hosts.Cells {
			if c.TokenFile != "" {
				withFile = append(withFile, n)
			}
		}
		sort.Strings(withFile)
	}
	switch {
	case env != "" && len(withFile) > 0:
		rep.Add(fleetapi.DoctorCheck{ID: "auth.client_env", Level: fleetapi.LevelWarn,
			Summary: "$VIBE_TOKEN is set in THIS shell and overrides every per-cell token_file",
			Detail: "the CLI's precedence is env-first on purpose (a human typing one command means the override), so " +
				"`vibe cell drain --cell X` from here ignores cells.X.token_file for: " + strings.Join(withFile, ", ") + ".",
			Fix: "unset VIBE_TOKEN when you want the per-cell credentials."})
	case env != "":
		rep.Add(fleetapi.DoctorCheck{ID: "auth.client_env", Level: fleetapi.LevelOK,
			Summary: "$VIBE_TOKEN is set in this shell and no cell declares a token_file to shadow"})
	default:
		rep.Add(fleetapi.DoctorCheck{ID: "auth.client_env", Level: fleetapi.LevelOK,
			Summary: "no $VIBE_TOKEN in this shell; per-cell token_files and the local token decide"})
	}
}

// renderDoctor prints the report severity-first: a tired human reads
// top-down and stops at the thing that matters. The OK block stays in by
// default — "I checked these fifteen things and they are fine" is half
// of what a sit-down command is for.
func renderDoctor(out io.Writer, rep *fleetapi.DoctorReport, omitOK bool) {
	rep.SortBySeverity()
	where := rep.Fleetd
	if where == "" {
		where = "local daemon"
	}
	fmt.Fprintf(out, "vibe fleet doctor — %s — %s\n", termSafe(where), rep.GeneratedAt.Local().Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(out, "%d cell(s) · %d checks · %d FAIL · %d WARN · %d UNKNOWN · %d OK\n\n",
		rep.Summary.Cells, rep.Summary.Checks, rep.Summary.Fail, rep.Summary.Warn, rep.Summary.Unknown, rep.Summary.OK)
	for _, c := range rep.Checks {
		if omitOK && c.Level == fleetapi.LevelOK {
			continue
		}
		fmt.Fprintf(out, "%-7s %-20s %-12s %s\n",
			strings.ToUpper(string(c.Level)), termSafe(c.ID), termSafe(dash(c.Cell)), termSafe(c.Summary))
		for _, line := range wrapDoctor(c.Detail) {
			fmt.Fprintf(out, "%-7s %-20s %-12s   %s\n", "", "", "", termSafe(line))
		}
		for i, line := range wrapDoctor(c.Fix) {
			// The label prefixes the FIRST line only: repeating it on every
			// wrapped line reads as several fixes.
			label := "     "
			if i == 0 {
				label = "fix: "
			}
			fmt.Fprintf(out, "%-7s %-20s %-12s   %s%s\n", "", "", "", label, termSafe(line))
		}
	}
}

// doctorWrapWidth keeps a detail line inside a standard terminal beside
// the 42-column prefix.
const doctorWrapWidth = 88

func wrapDoctor(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var lines []string
	var cur string
	for _, w := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= doctorWrapWidth:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// doctorExitCode extracts a doctor exit code from an error, for main's
// handler. Returns 0 when err is not one of ours.
func doctorExitCode(err error) int {
	var e errDoctorLevel
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}
