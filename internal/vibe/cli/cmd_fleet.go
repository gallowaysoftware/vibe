package cli

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetannounce"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
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
		Short: "Fleet announcer (slim cells).",
	}
	cmd.AddCommand(fleetAnnounceCmd())
	return cmd
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
			ann, err := fleetannounce.New(fleetannounce.Config{
				Cell:              cell,
				RegistryURL:       registry,
				TokenFile:         tokenFile,
				LlamaSwapURL:      llamaSwap,
				Defs:              cellDefs,
				LlamaServerBinary: llamaBin,
				IntentPath:        intentPath,
				// A slim cell's tokens count exactly as much as a
				// daemon-bearing cell's; same collector, same state file.
				Usage: usagemeter.Snapshotter(llamaSwap, paths.CellUsageFile()),
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
