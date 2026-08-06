package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/modeltry"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// `vibe model try` — fleet-control C18. The weekly membership-churn loop
// (pull, scaffold, render, restart, warm, is it better, keep or delete)
// as one command with a journal behind it.
//
// The command is a SEQUENCER. Every step it runs already existed; what
// this file adds is the order, the two declarations the trial makes to
// the control plane, and the one deferral. The deferral is the whole
// design and it is C14's sentence: applying a def edit rewrites this
// cell's llama-swap config, which `-watch-config` reloads — truncating
// any generation still running at llama-swap's hardcoded 30 s and
// evicting every resident model. So the operator DECLARES the trial by
// typing the command, and OBSERVATION (fleetd's idle window, C4's
// activity fold read through C10's rule) may only delay the apply. There
// is no inference anywhere: nothing here concludes that a quiet box wants
// a config change.
//
// `--now` is C14's `force`, exactly: it is about tonight's conditions,
// never about configuration. It skips the deferral and says what that
// costs; it skips nothing structural.

// trialLeaseTTL is how long the trial's advisory lease lasts. Long
// enough to cover a cold 30B load on both sides plus a human reading the
// report; bounded, because a lease nobody released is a cell that quietly
// stops being probed.
const trialLeaseTTL = 4 * time.Hour

// trialHoldTTL is the C11 hold placed on the INCUMBENT for the duration.
// Without it fleetd's warm-target restore notices the cell went idle
// after the apply, warms the default model, and evicts the candidate
// mid-comparison — the exact case C11 was built for, arriving on a
// fifteen-second ticker.
const trialHoldTTL = 4 * time.Hour

func modelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Evaluate a candidate model on this cell (fleet-control C18).",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(modelTryCmd())
	return cmd
}

func modelTryCmd() *cobra.Command {
	var (
		file, revision, like, as, dest, apiFlag string
		cellFlag                                string
		quiet, waitFor                          time.Duration
		now, dryRun, skipDisk                   bool
		minFreeGB                               float64
	)
	cmd := &cobra.Command{
		Use:   "try <hf-repo>",
		Short: "Pull a candidate model, apply it when this cell is quiet, and measure it against the incumbent.",
		Long: "vibe model try downloads a candidate from HuggingFace, scaffolds a backend def for it\n" +
			"from the def already serving on this box (--like), applies it to THIS cell's llama-swap\n" +
			"once fleetd vouches the cell has been idle, warms both models and prints one measurement\n" +
			"of each with what they do not mean.\n\n" +
			"The trial is a candidate, never fleet config. Its def carries `trial: true`, which the\n" +
			"front render excludes, so the id is reachable on this cell alone — no client anywhere\n" +
			"else can route to it and nothing about the fleet catalog changes. Promotion is a human\n" +
			"git operation: delete the trial line, commit the def.\n\n" +
			"Applying rewrites this cell's llama-swap config, and a `-watch-config` reload evicts every\n" +
			"resident model and truncates any generation still running after 30s. That is why the\n" +
			"apply waits for an observed idle window. --now skips the wait and says what it costs.\n\n" +
			"Re-running the command resumes: every step is journalled, and `vibe model try end` rolls\n" +
			"back whatever the journal says happened.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmdContext(cmd)
			if err := validateTrialGate(quiet, minFreeGB, now); err != nil {
				return err
			}
			runner, err := newTrialRunner(minFreeGB)
			if err != nil {
				return err
			}
			if err := runner.RefuseRemote(cellFlag); err != nil {
				return err
			}
			return runModelTry(ctx, cmd.OutOrStdout(), runner, apiFlag, modeltry.PlanRequest{
				Repo: args[0], File: file, Revision: revision, Like: like, As: as, Dest: dest,
				SkipDiskCheck: skipDisk,
			}, trialGateOpts{quiet: quiet, wait: waitFor, now: now, dryRun: dryRun})
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "the file to pull from the repo (e.g. Qwen3.6-27B-Q5_K_XL.gguf)")
	cmd.Flags().StringVar(&revision, "revision", "", "HuggingFace revision (default: main)")
	cmd.Flags().StringVar(&like, "like", "", "the def whose serving flags the trial copies AND the model it is measured against")
	cmd.Flags().StringVar(&as, "as", "", "trial def name (default: trial-<repo>)")
	cmd.Flags().StringVar(&dest, "dest", "", "directory for the weights (default: beside the --like def's weights)")
	cmd.Flags().StringVar(&cellFlag, "cell", "", "the cell to try on; must be this box's own cell (cross-cell try is not implemented)")
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	cmd.Flags().DurationVar(&quiet, "idle", modeltry.DefaultQuiet, "how long this cell must be observed idle before the config is applied")
	cmd.Flags().DurationVar(&waitFor, "wait", 2*time.Hour, "how long to wait for that idle window (0 waits forever)")
	cmd.Flags().BoolVar(&now, "now", false, "apply immediately, accepting that a reload evicts residents and truncates streams past 30s")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and the def that would be written, then stop")
	cmd.Flags().BoolVar(&skipDisk, "skip-disk-check", false, "pull even when free space cannot be verified against the file size")
	cmd.Flags().Float64Var(&minFreeGB, "min-free", float64(modeltry.DefaultMinFreeBytes)/(1<<30), "GiB that must remain free on the library filesystem after the pull")

	cmd.AddCommand(modelTryStatusCmd(), modelTryEndCmd())
	return cmd
}

func modelTryStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the in-flight trial on this cell and how to end it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			runner, err := newTrialRunner(0)
			if err != nil {
				return err
			}
			t, err := runner.Load()
			if err != nil {
				return err
			}
			if t == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "no trial in flight on %s\n", runner.Cell())
				return nil
			}
			t.RenderStatus(cmd.OutOrStdout())
			return nil
		},
	}
	return cmd
}

func modelTryEndCmd() *cobra.Command {
	var (
		purge   bool
		apiFlag string
	)
	cmd := &cobra.Command{
		Use:   "end",
		Short: "Roll the trial back: drop the def, re-render this cell's config, release the declarations.",
		Long: "end walks the journal backwards. It removes the trial def, re-renders this cell's\n" +
			"llama-swap config without it (falling back to the byte-exact copy banked before the\n" +
			"apply if that render fails for an unrelated reason), releases the lease and the hold\n" +
			"this trial took — and only those — and reports each step.\n\n" +
			"The weights stay on disk unless --purge: re-pulling 20 GB to re-run a trial is the\n" +
			"toil this verb exists to remove.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmdContext(cmd)
			runner, err := newTrialRunner(0)
			if err != nil {
				return err
			}
			t, err := runner.Load()
			if err != nil {
				return err
			}
			if t == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "no trial in flight on %s — nothing to roll back\n", runner.Cell())
				return nil
			}
			return runModelTryEnd(ctx, cmd.OutOrStdout(), runner, t, apiFlag, purge)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the downloaded weights")
	cmd.Flags().StringVar(&apiFlag, "api", "", "fleetd base URL (default: $VIBE_API, hosts.yaml fleetd_url, or the local daemon)")
	return cmd
}

// validateTrialGate refuses the two flag values that silently disable a
// guard instead of relaxing it. Both are decided before anything is
// written, C10's rule: a refusal fleetd would issue anyway is issued
// BEFORE the twenty-minute pull.
//
// `--idle 0` is the important one. The deferral IS this phase — a config
// write truncates streams and evicts residents — and `awaitConds.evalIdle`
// only runs when the window is positive, so a zero would apply as soon as
// the cell answered, with none of `--now`'s sentence about what that
// costs. Skipping the wait is spelled `--now`, exactly once.
func validateTrialGate(quiet time.Duration, minFreeGB float64, now bool) error {
	if quiet <= 0 && !now {
		return fmt.Errorf("--idle %s is not a wait: an idle window of zero applies the config the moment the cell answers, "+
			"which is what --now does — and --now says what it costs. Pass a positive --idle, or pass --now", quiet)
	}
	if minFreeGB < 0 {
		return fmt.Errorf("--min-free %g is negative", minFreeGB)
	}
	if minFreeGB == 0 {
		return fmt.Errorf("--min-free 0 is not \"no floor\" — it is silently the %.0f GiB default, because zero is how the flag says \"unset\". "+
			"Pass a positive floor, or --skip-disk-check to remove it entirely", float64(modeltry.DefaultMinFreeBytes)/(1<<30))
	}
	return nil
}

// newTrialRunner resolves everything from the same sources the render
// path uses, so a trial's config write and `vibe router render` can never
// disagree about which cell this box is or which binary it renders.
func newTrialRunner(minFreeGB float64) (*modeltry.Runner, error) {
	cfg, err := daemon.LoadConfig()
	if err != nil {
		return nil, err
	}
	hosts, err := fleetcfg.Load()
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", paths.HostsFile(), err)
	}
	if cfg.Fleet.Cell == "" {
		return nil, fmt.Errorf("this box declares no fleet.cell in %s — a trial is placed on a named cell, and an unnamed box has nowhere to put one", paths.ConfigFile())
	}
	var minFree int64
	if minFreeGB > 0 {
		minFree = int64(minFreeGB * (1 << 30))
	}
	return modeltry.New(modeltry.Options{
		Cell:              cfg.Fleet.Cell,
		Hosts:             hosts,
		BackendsDir:       paths.BackendsDir(),
		ConfigPath:        llamaSwapConfigPath(),
		ExtrasPath:        routerExtrasPath(),
		LlamaServerBinary: resolveLlamaServerBinary(""),
		LlamaSwapURL:      fmt.Sprintf("http://127.0.0.1:%d", cfg.ProxyPort),
		StatePath:         paths.ModelTrialFile(),
		ProbeStatePath:    paths.CellProbeFile(),
		HTTP:              &http.Client{},
		DiskFree:          diskFreeBytes,
		MinFreeBytes:      minFree,
	})
}

// diskFreeBytes is the CLI's free-space probe. It reuses the daemon's
// capacity reader rather than a second statfs — the number an operator
// sees in `vibe fleet doctor`'s disk check and the number this refusal is
// computed from must be the same number.
func diskFreeBytes(dir string) (int64, error) {
	target := dir
	for {
		if _, err := os.Stat(target); err == nil {
			break
		}
		parent := filepath.Dir(target)
		if parent == target {
			return 0, fmt.Errorf("no existing directory on the path %s", dir)
		}
		target = parent
	}
	cap := daemon.FleetDiskCapacity(target)
	if cap == nil || cap.DiskFreeGB <= 0 {
		return 0, fmt.Errorf("statfs on %s reported nothing", target)
	}
	return int64(cap.DiskFreeGB * (1 << 30)), nil
}

// trialGateOpts are the deferral knobs.
type trialGateOpts struct {
	quiet  time.Duration
	wait   time.Duration
	now    bool
	dryRun bool
}

func runModelTry(ctx context.Context, out io.Writer, runner *modeltry.Runner, apiFlag string, req modeltry.PlanRequest, gate trialGateOpts) error {
	t, def, err := runner.Plan(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "trial %s on %s\n", t.Def, t.Cell)
	fmt.Fprintf(out, "  candidate:   %s / %s\n", t.Repo, t.File)
	fmt.Fprintf(out, "  modelled on: %s (its flags, its comparison)\n", t.Incumbent)
	fmt.Fprintf(out, "  weights ->   %s\n", t.WeightsPath)
	fmt.Fprintf(out, "  def ->       %s\n", t.DefPath)
	fmt.Fprintf(out, "  config ->    %s (a reload evicts residents and truncates streams past 30s)\n\n", t.ConfigPath)

	if gate.dryRun {
		yamlDef, derr := modeltry.PreviewDef(t, def)
		if derr != nil {
			return derr
		}
		fmt.Fprintln(out, "--dry-run: the def that would be written:")
		fmt.Fprintln(out)
		fmt.Fprintln(out, yamlDef)
		// Close the journal Plan opened — and ONLY one Plan opened. A
		// --dry-run that leaves a fresh journal behind makes the very next
		// real invocation resume a trial the operator never started; a
		// --dry-run that closes a RESUMED one performs the whole rollback
		// (drop the def, re-render the cell's config, evict every resident
		// model) under a flag whose entire promise is that nothing happens.
		// `End` works from any state, so the decision cannot live there.
		closed, err := runner.DiscardIfFresh(ctx, t)
		if err != nil {
			return err
		}
		if !closed {
			fmt.Fprintf(out, "a trial of %s is already in flight at state %s — --dry-run left it exactly as it was.\n", t.Def, t.State)
			fmt.Fprintln(out, "`vibe model try status` shows it; `vibe model try end` rolls it back.")
			return nil
		}
		fmt.Fprintln(out, "nothing was downloaded, nothing on this box changed, and no trial is open.")
		fmt.Fprintln(out, "re-run without --dry-run to perform it.")
		return nil
	}

	fmt.Fprintln(out, "fetching...")
	if err := runner.Fetch(ctx, t, progressPrinter(out)); err != nil {
		return err
	}
	if err := runner.Stage(t, def); err != nil {
		return err
	}
	fmt.Fprintf(out, "staged %s\n", t.DefPath)

	target, terr := resolveFleetd(apiFlag)
	if terr != nil {
		return terr
	}
	if err := trialDeclareAndWait(ctx, out, runner, target, t, gate); err != nil {
		return err
	}

	res, err := runner.Apply(ctx, t)
	if err != nil {
		return err
	}
	if res.Diff != "" {
		fmt.Fprint(out, res.Diff)
	}
	fmt.Fprintf(out, "applied: %s\n", res.Note)
	if !res.Live {
		fmt.Fprintln(out, "\nthe trial is NOT live. The config is written and the journal is open:")
		fmt.Fprintln(out, "  apply it:    systemctl --user restart llama-swap   (this evicts whatever is resident)")
		fmt.Fprintln(out, "  then:        vibe model try <same args>            (resumes at the measurement)")
		fmt.Fprintln(out, "  or undo it:  vibe model try end")
		return nil
	}

	fmt.Fprintln(out, "warming both models and measuring (this is the slow part)...")
	cmp, err := runner.Measure(ctx, t)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	cmp.Render(out)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "the trial is still applied on this cell. `vibe model try status` for the next commands.")
	return nil
}

// trialDeclareAndWait is the control-plane half: the two declarations and
// the one deferral, in the order that makes each of them mean something.
//
// The lease goes on BEFORE the wait's claim moment and the hold before
// the apply, because both exist to protect the window that starts at the
// apply. The wait goes between them and the write, because it is the only
// thing standing between a declared config change and somebody's
// truncated generation.
// Each declaration is written to the JOURNAL the moment it is taken, not
// at the end of the sequence: everything after this point can fail (the
// bank, the render, the write), and a declaration that only exists in
// this process's memory is one `vibe model try end` will never release.
// Its cost is four hours of a suspended warm policy, plus a re-run that
// parks forever on `--unleased` against the trial's own hold.
func trialDeclareAndWait(ctx context.Context, out io.Writer, runner *modeltry.Runner, target fleetdTarget, t *modeltry.Trial, gate trialGateOpts) error {
	if t.State != modeltry.StateStaged {
		return nil // already applied; the declarations were made then
	}
	cond := awaitConds{
		wantUp:   true,
		idle:     gate.quiet,
		unleased: true,
		lease:    leaseClaim{holder: modeltry.TrialLeaseHolder},
	}
	if gate.now {
		// C14's force: about tonight's conditions, never about
		// configuration. It buys past the deferral and says what that
		// costs; it buys past nothing structural.
		fmt.Fprintf(out, "--now: applying without an idle window. Any generation still running on %s after 30s\n", t.Cell)
		fmt.Fprintln(out, "       will be truncated and every resident model will be evicted.")
	} else {
		fmt.Fprintf(out, "waiting for %s to be observed idle for %s (fleetd's evidence, not a guess)...\n", t.Cell, gate.quiet)
		if _, err := awaitCell(ctx, out, target, t.Cell, cond, gate.wait, 5*time.Second); err != nil {
			return fmt.Errorf("%w\n\nthe trial is staged and nothing on this box is serving it yet: re-run to keep waiting, "+
				"pass --now to apply anyway (it truncates streams past 30s and evicts residents), or `vibe model try end` to back it out", err)
		}
	}

	// The lease: a plain C2 declaration that this cell has a consumer.
	// C8's probes and the warm schedules already skip a leased cell, so
	// the measurement is not competing with the fleet's own traffic.
	if err := postTrialLease(ctx, target, t.Cell, t.Def); err != nil {
		return fmt.Errorf("could not declare the trial's lease on %s (%w) — refusing to apply undeclared: "+
			"a config rewrite nothing announced is invisible to the pre-drain report that exists to protect running work", t.Cell, err)
	}
	t.Leased = true
	if err := runner.Save(t); err != nil {
		return fmt.Errorf("declared the lease on %s/%s but could not record it in the journal (%w) — `vibe model try end` would not release it; release it by hand (`vibe cell lease` semantics: cell=%s model=%s holder=%s) or wait %s for it to expire",
			t.Cell, t.Def, err, t.Cell, t.Def, modeltry.TrialLeaseHolder, trialLeaseTTL)
	}
	fmt.Fprintf(out, "declared: lease %s on %s/%s\n", modeltry.TrialLeaseHolder, t.Cell, t.Def)

	// The hold: C11, on the INCUMBENT, so fleetd's warm-target restore
	// does not reload it the moment the cell looks idle and evict the
	// candidate mid-comparison.
	held, err := trialTakeHold(ctx, target, t.Cell, t.Incumbent)
	switch {
	case err != nil:
		fmt.Fprintf(out, "warning: no hold on %s/%s (%v) — if a warm target is configured for this cell,\n", t.Cell, t.Incumbent, err)
		fmt.Fprintln(out, "         fleetd may reload the incumbent mid-trial and evict the candidate. The numbers would still be real; they would just be measuring a different afternoon.")
	case held:
		t.Held = true
		if serr := runner.Save(t); serr != nil {
			return fmt.Errorf("declared the hold on %s/%s but could not record it in the journal (%w) — `vibe model try end` would not release it; release it with `vibe cell hold --release` or wait %s",
				t.Cell, t.Incumbent, serr, trialHoldTTL)
		}
		fmt.Fprintf(out, "declared: hold on %s/%s for the duration (fleetd's warm policy will not evict the candidate)\n", t.Cell, t.Incumbent)
	default:
		fmt.Fprintf(out, "a hold already exists on %s/%s — left alone, and this trial will not release it\n", t.Cell, t.Incumbent)
	}
	return nil
}

func postTrialLease(ctx context.Context, target fleetdTarget, cell, model string) error {
	return target.postFleet(ctx, "/api/fleet/lease", map[string]string{
		"cell": cell, "model": model, "holder": modeltry.TrialLeaseHolder,
		"note": "vibe model try: evaluating a candidate on this cell",
		"ttl":  trialLeaseTTL.String(),
	}, nil)
}

// trialTakeHold places a C11 hold on the incumbent, but only when there
// is not one already. An existing hold is the operator's declaration with
// the operator's note and the operator's expiry; re-issuing would
// overwrite all three, and this trial's `end` would then delete a hold
// somebody else still means. Reported false so `end` knows not to.
func trialTakeHold(ctx context.Context, target fleetdTarget, cell, model string) (bool, error) {
	snap, err := target.fetchState(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range snap.Cells {
		if c.Name != cell {
			continue
		}
		for _, l := range c.Leases {
			if l.Holder == fleetapi.HoldHolder && l.Model == model {
				return false, nil
			}
		}
	}
	err = target.postFleet(ctx, "/api/fleet/lease", map[string]any{
		"cell": cell, "model": model, "holder": fleetapi.HoldHolder, "hold": true,
		"note": "vibe model try: comparison in progress against this model",
		"ttl":  trialHoldTTL.String(),
	}, nil)
	if err != nil {
		return false, err
	}
	return true, nil
}

func runModelTryEnd(ctx context.Context, out io.Writer, runner *modeltry.Runner, t *modeltry.Trial, apiFlag string, purge bool) error {
	// The declarations come off first and independently of the file
	// rollback: a fleetd that is down must not leave a stranded config,
	// and a config rollback that fails must not leave the fleet's warm
	// policy suspended for four hours either.
	if t.Leased || t.Held {
		target, terr := resolveFleetd(apiFlag)
		if terr != nil {
			fmt.Fprintf(out, "warning: cannot reach fleetd to release this trial's declarations (%v) — they expire on their own (lease %s, hold %s)\n",
				terr, trialLeaseTTL, trialHoldTTL)
		} else {
			if t.Leased {
				reportRelease(out, "lease", t.Cell, t.Def, deleteLease(ctx, target, t.Cell, t.Def, modeltry.TrialLeaseHolder))
			}
			if t.Held {
				reportRelease(out, "hold", t.Cell, t.Incumbent, deleteLease(ctx, target, t.Cell, t.Incumbent, fleetapi.HoldHolder))
			}
		}
	}
	rep, err := runner.End(ctx, t, purge)
	for _, s := range rep.Steps {
		fmt.Fprintf(out, "  %s\n", s)
	}
	if rep.RestoredFromBackup {
		fmt.Fprintln(out, "  (the re-render failed for a reason unrelated to the trial; check `vibe router render --check`)")
	}
	// Printed whenever there is one: `stillListed` reports an UNREADABLE
	// catalog as not-listed with a note, and swallowing the note there
	// would let "I could not check" read as "it is gone" — the reading
	// this repo has shipped six times.
	if rep.Note != "" {
		fmt.Fprintf(out, "  %s\n", rep.Note)
	}
	if err != nil {
		return fmt.Errorf("the rollback did not complete: %w", err)
	}
	fmt.Fprintf(out, "trial closed on %s.\n", t.Cell)
	return nil
}

func reportRelease(out io.Writer, kind, cell, model string, err error) {
	if err != nil {
		fmt.Fprintf(out, "  warning: could not release the %s on %s/%s (%v) — it expires on its own\n", kind, cell, model, err)
		return
	}
	fmt.Fprintf(out, "  released the %s on %s/%s\n", kind, cell, model)
}

// deleteLease is the DELETE half of C2's lease endpoint. It reports the
// server's `existed` as a no-op rather than an error: a lease that
// expired on its own during a long trial is the normal case, and calling
// that a failed rollback would teach an operator to ignore the line.
func deleteLease(ctx context.Context, target fleetdTarget, cell, model, holder string) error {
	body, err := json.Marshal(map[string]string{"cell": cell, "model": model, "holder": holder})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target.base+"/api/fleet/lease", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := target.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return errors.New(termSafe(strings.TrimSpace(string(payload))))
	}
	return nil
}

// progressPrinter renders the download. hfdownload throttles callbacks to
// ~4/s; this collapses them to one line per whole percent so a 20 GB pull
// does not produce ten thousand lines in a CI log or an agent transcript.
func progressPrinter(out io.Writer) func(downloaded, total int64) {
	last := -1
	return func(downloaded, total int64) {
		if total <= 0 {
			return
		}
		pct := int(downloaded * 100 / total)
		if pct == last {
			return
		}
		last = pct
		if pct%5 != 0 && pct != 100 {
			return
		}
		fmt.Fprintf(out, "  %3d%%  %.1f / %.1f GiB\n", pct, float64(downloaded)/(1<<30), float64(total)/(1<<30))
	}
}
