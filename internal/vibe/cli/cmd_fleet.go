package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetannounce"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/modelprobe"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/prices"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"github.com/gallowaysoftware/vibe/internal/vibe/usagemeter"
)

// `vibe fleet announce` is the slim announcer (fleet-control C3 §2): a
// flag-configured foreground loop for cells that run llama-swap without
// a full vibe daemon (the heavy cell). Same code path as the daemon's
// announce loop; runs as a trivial systemd unit. Commissioning a new
// cell = this + a hosts.yaml entry + the registry URL.

func fleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Fleet doctor, announcer (slim cells), notifications, the state mirror, and price tooling.",
	}
	cmd.AddCommand(fleetAnnounceCmd())
	cmd.AddCommand(fleetPricesCmd())
	cmd.AddCommand(fleetNotifyCmd())
	cmd.AddCommand(fleetDoctorCmd())
	cmd.AddCommand(fleetMirrorCmd())
	return cmd
}

// `vibe fleet prices vendor` is the C7b §2 refresh path: a dev-only
// subcommand a human runs on an internet-connected box in a checkout.
// The savings screen must render on a LAN with no internet and CI never
// has network, so the price table is a vendored artifact — this is how
// it gets rewritten.
func fleetPricesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prices",
		Short: "The vendored model price table (dev tooling).",
	}
	cmd.AddCommand(fleetPricesVendorCmd(), fleetPricesShowCmd())
	return cmd
}

func fleetPricesVendorCmd() *cobra.Command {
	var (
		out              string
		effectiveFrom    string
		modelsDev        string
		litellm          string
		maxDisagreements int
		ratio            float64
		dryRun           bool
	)
	cmd := &cobra.Command{
		Use:   "vendor",
		Short: "Refresh the vendored price table from models.dev + LiteLLM (needs network).",
		Long: "Fetch both upstream price tables (both MIT), prune and normalize them to USD " +
			"per million tokens, cross-check them against each other, and append a new dated " +
			"snapshot to the vendored artifact. Dated snapshots are load-bearing: a day bucket " +
			"is priced at the rates in effect on that day, so a price cut never retroactively " +
			"erases money that was not spent at the time.\n\n" +
			"Run this from a vibe checkout on a box with internet. CI never has network and " +
			"never runs it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if out == "" {
				out = filepath.Join("internal", "vibe", "prices", prices.TableFile)
			}
			var prev *prices.Table
			if data, err := os.ReadFile(out); err == nil {
				prev, err = prices.Parse(data)
				if err != nil {
					return fmt.Errorf("read %s: %w", out, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read %s: %w", out, err)
			} else {
				return fmt.Errorf("%s does not exist: run this from a vibe checkout, or pass --out", out)
			}

			cfg := prices.VendorConfig{
				ModelsDevURL:     modelsDev,
				LiteLLMURL:       litellm,
				EffectiveFrom:    effectiveFrom,
				MaxDisagreements: maxDisagreements,
				Ratio:            ratio,
			}
			table, rep, err := cfg.Vendor(ctx, prev)
			if err != nil {
				return err
			}
			data, err := prices.Encode(table)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "snapshot %s (%s): %d rows\n", rep.EffectiveFrom, snapshotKind(rep.Base), rep.Rows)
			fmt.Fprintf(w, "  added %d · changed %d · removed %d · unchanged %d\n",
				len(rep.Added), len(rep.Changed), len(rep.Removed), rep.Unchanged)
			for _, s := range rep.Sources {
				fmt.Fprintf(w, "  source %s (%s) commit %s\n", s.Name, s.Licence, orDash(s.Commit))
			}
			for prov, n := range rep.Excluded {
				fmt.Fprintf(w, "  excluded %d rows from %s (redistribution not permitted)\n", n, prov)
			}
			if len(rep.Disagreements) > 0 {
				fmt.Fprintf(w, "  %d cross-source disagreements past %gx — those rows were DROPPED\n", len(rep.Disagreements), ratio)
			}
			for _, warn := range rep.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "  warning: %s\n", warn)
			}
			for _, line := range rep.Changed {
				fmt.Fprintf(w, "  ~ %s\n", line)
			}
			if dryRun {
				fmt.Fprintf(w, "dry run: %s not written (%d bytes)\n", out, len(data))
				return nil
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(w, "wrote %s (%d bytes)\n", out, len(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "path to the vendored artifact (default internal/vibe/prices/prices.json)")
	cmd.Flags().StringVar(&effectiveFrom, "effective-from", "", "date the new snapshot takes effect (YYYY-MM-DD, default today UTC)")
	cmd.Flags().StringVar(&modelsDev, "models-dev-url", prices.DefaultModelsDevURL, "models.dev api.json URL")
	cmd.Flags().StringVar(&litellm, "litellm-url", prices.DefaultLiteLLMURL, "LiteLLM model_prices_and_context_window.json URL")
	cmd.Flags().IntVar(&maxDisagreements, "max-disagreements", 0, "how many >ratio cross-source price conflicts to tolerate (they are dropped either way; 0 fails the run)")
	cmd.Flags().Float64Var(&ratio, "ratio", 2, "cross-source disagreement bound")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the summary without writing the artifact")
	return cmd
}

func fleetPricesShowCmd() *cobra.Command {
	var day string
	cmd := &cobra.Command{
		Use:   "show [model]",
		Short: "What the vendored table says a model costs (as of a day).",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			table, err := prices.Embedded()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if table.Example {
				fmt.Fprintln(w, "EXAMPLE DATA — these are synthetic prices, not real rates.")
			}
			fmt.Fprintf(w, "as of %s (oldest snapshot %s, %d snapshots)\n", table.AsOf(), table.Oldest(), len(table.Snapshots))
			if len(args) == 0 {
				return nil
			}
			if day == "" {
				day = table.AsOf()
			}
			res := table.At(day)
			rows := res.Hosts(args[0], false)
			if len(rows) == 0 {
				fmt.Fprintf(w, "%s: no host in the table serves it\n", args[0])
				return nil
			}
			fmt.Fprintf(w, "%s priced at %s: %d hosts\n", args[0], res.EffectiveFrom, len(rows))
			for _, r := range rows {
				fmt.Fprintf(w, "  %-24s %-44s in %8.4f  cache %8.4f  out %8.4f  %s\n",
					r.Provider, r.Model, r.In, r.CacheRead, r.Out, openLabel(r.Open))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&day, "day", "", "price it as of this day (YYYY-MM-DD, default the newest snapshot)")
	return cmd
}

func snapshotKind(base bool) string {
	if base {
		return "base"
	}
	return "overlay"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func openLabel(open bool) string {
	if open {
		return "open-weights"
	}
	return ""
}

func fleetAnnounceCmd() *cobra.Command {
	var (
		cell       string
		registry   string
		tokenFile  string
		llamaSwap  string
		llamaBin   string
		intentPath string
	)
	cmd := &cobra.Command{
		Use:   "announce",
		Short: "Announce this cell to fleetd (foreground loop for slim cells).",
		Long: "Heartbeat what this box serves to fleetd: models + flag fingerprints + " +
			"intent echo, with desired intent and commands piggybacked on responses. " +
			"Commissioning a new cell = this unit + a hosts.yaml entry + the registry URL. " +
			"An unreachable registry never affects serving (quiet backoff).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if cell == "" || registry == "" {
				return fmt.Errorf("--cell and --registry are required")
			}
			defs, err := router.LoadDefs(paths.BackendsDir())
			if err != nil {
				return fmt.Errorf("load backend defs: %w", err)
			}
			var cellDefs []*profile.BackendDef
			for _, def := range defs {
				if def.Cell == "" || def.Cell == cell {
					cellDefs = append(cellDefs, def)
				}
			}
			if intentPath == "" {
				intentPath = paths.CellIntentFile()
			}
			probes, runProbe := modelprobe.Hooks(llamaSwap, paths.CellProbeFile(), cellDefs, llamaBin)
			ann, err := fleetannounce.New(fleetannounce.Config{
				Cell:              cell,
				RegistryURL:       registry,
				TokenFile:         tokenFile,
				LlamaSwapURL:      llamaSwap,
				Defs:              cellDefs,
				LlamaServerBinary: llamaBin,
				IntentPath:        intentPath,
				// A slim cell reports its build and def checkout like any
				// other (C13): def-SHA parity across the fleet is worthless
				// when the cell most likely to drift is the one that never
				// says which checkout it has. Same producer as the daemon's.
				Versions: func() *fleetapi.AnnounceVersions { return daemon.FleetVersions(paths.BackendsDir(), llamaSwap) },
				// Disk only — a box with no daemon has no VRAM probe wired,
				// and an invented zero would read as a full card.
				Capacity: func() *fleetapi.AnnounceCapacity {
					return daemon.FleetDiskCapacity(filepath.Dir(paths.CellUsageFile()))
				},
				// A slim cell's tokens count exactly as much as a
				// daemon-bearing cell's; same collector, same state file.
				Usage: usagemeter.Snapshotter(llamaSwap, paths.CellUsageFile()),
				// Same for its probes: a slim cell is probeable exactly like
				// a daemon-bearing one (C8 §1 — the announcer is the prober).
				Probes:   probes,
				RunProbe: runProbe,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "announcing %s to %s (ctrl-C to stop)\n", cell, registry)
			runErr := ann.Run(ctx)
			// A clean undock: the final heartbeat says goodbye so fleetd
			// prunes this cell now instead of waiting out stale_after. Its
			// own context, because ctx is what just died.
			wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := ann.Withdraw(wctx); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "withdraw announce failed (fleetd will prune on staleness): %v\n", err)
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&cell, "cell", "", "this box's cell name in hosts.yaml (required)")
	cmd.Flags().StringVar(&registry, "registry", "", "fleetd base URL (required)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to fleetd's bearer token")
	cmd.Flags().StringVar(&llamaSwap, "llama-swap", "http://"+net.JoinHostPort("127.0.0.1", "9000"), "local llama-swap base URL")
	cmd.Flags().StringVar(&llamaBin, "llama-server", "", "llama-server path rendered into fingerprints (must match the cell's render)")
	// Two announcers on one box must not share an intent echo file: the
	// second would read the first's state as its own.
	cmd.Flags().StringVar(&intentPath, "intent-file", "", "where this announcer stores its local intent echo (default: the per-user cell intent file)")
	return cmd
}
