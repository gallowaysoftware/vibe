package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// routerCmd groups commands over the llama-swap router config
// (docs/design/router-lifecycle.md). The backend defs under
// $XDG_CONFIG_HOME/vibe/backends/ are the source of truth; the router config
// is a rendered artifact.
func routerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "router",
		Short: "Manage the llama-swap router config rendered from backend definitions.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(routerRenderCmd())
	return cmd
}

func routerRenderCmd() *cobra.Command {
	var (
		check    bool
		toStdout bool
		llamaBin string
		extras   string
		cell     string
		outPath  string
	)
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render the llama-swap config from external backend definitions.",
		Long: "vibe router render turns every backend def with backend.external: true " +
			"into the llama-swap config the router on the proxy port serves " +
			"(llama_server defs become models: entries, cloud_peer defs become peers: " +
			"stanzas) and writes it atomically to " + llamaSwapConfigPath() + ", " +
			"printing a unified diff of what changed. llama-swap is never restarted " +
			"automatically; apply a changed config with `systemctl --user restart llama-swap`. " +
			"With --cell the render targets a fleet cell instead (fleet-control C2 §6): " +
			"--cell front renders the peers-only front config from hosts.yaml, any other " +
			"cell renders that cell's defs plus unassigned defs. A render for a cell that " +
			"isn't this box's requires --out or --stdout so it can never overwrite the " +
			"local llama-swap config.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, hosts, configPath, err := planRender(cell, outPath, toStdout)
			if err != nil {
				return err
			}
			opts := router.Options{
				LlamaServerBinary: resolveLlamaServerBinary(llamaBin),
				ExtrasPath:        extras,
				Cell:              target,
				Hosts:             hosts,
				Warnf: func(format string, args ...any) {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: "+format+"\n", args...)
				},
			}
			return runRouterRender(cmd.OutOrStdout(), paths.BackendsDir(), configPath, opts, check, toStdout)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "print the diff and exit 1 on drift without writing")
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "print the rendered config to stdout instead of writing the file")
	cmd.Flags().StringVar(&llamaBin, "llama-server", "", "llama-server binary path written into rendered cmds (default: daemon config llama_binary, then ~/.local/bin/llama-server)")
	cmd.Flags().StringVar(&extras, "extras", routerExtrasPath(), "YAML file merged verbatim into the render for entries defs can't express (smoke models, peer cells, groups); additive only, missing file is fine; on a front render what the adopter does with the merged sections is the adopter's business")
	cmd.Flags().StringVar(&cell, "cell", "", "fleet cell to render for (default: the daemon config's fleet.cell); \"front\" renders the peers-only front config")
	cmd.Flags().StringVar(&outPath, "out", "", "destination path (default: the local llama-swap config); required with --cell for a non-local cell unless --stdout")
	return cmd
}

// planRender resolves the render's target cell, fleet membership, and
// destination. The target cell defaults to the daemon config's fleet.cell
// so a fleet box renders its own cell's config with no flag. Output safety:
// an explicit --cell naming a NON-LOCAL cell must carry --out or --stdout —
// the local llamaSwapConfigPath() must never receive another cell's render,
// or this box's router would start serving a foreign cell's peer map.
func planRender(cellFlag, outPath string, toStdout bool) (target string, hosts *fleetcfg.File, configPath string, err error) {
	dCfg, err := daemon.LoadConfig()
	if err != nil {
		return "", nil, "", err
	}
	localCell := dCfg.Fleet.Cell
	if cellFlag != "" && cellFlag != localCell && !toStdout && outPath == "" {
		return "", nil, "", fmt.Errorf("--cell %q is not this box's cell (fleet.cell %q); refusing to write the local llama-swap config — pass --out <path> or --stdout", cellFlag, localCell)
	}
	hosts, err = fleetcfg.Load()
	if err != nil {
		return "", nil, "", err
	}
	target = cellFlag
	if target == "" {
		target = localCell
	}
	configPath = outPath
	if configPath == "" {
		configPath = llamaSwapConfigPath()
	}
	return target, hosts, configPath, nil
}

func runRouterRender(out io.Writer, backendsDir, configPath string, opts router.Options, check, toStdout bool) error {
	if toStdout {
		defs, err := router.LoadDefs(backendsDir)
		if err != nil {
			return err
		}
		rendered, err := router.Render(defs, opts)
		if err != nil {
			return err
		}
		fmt.Fprint(out, rendered)
		return nil
	}
	outcome, err := router.RenderToFile(backendsDir, configPath, opts, !check)
	if err != nil {
		return err
	}
	if !outcome.Changed {
		fmt.Fprintf(out, "%s is up to date\n", configPath)
		return nil
	}
	fmt.Fprint(out, outcome.Diff)
	if check {
		return fmt.Errorf("%s is out of date with %s (run `vibe router render`)", configPath, backendsDir)
	}
	fmt.Fprintf(out, "wrote %s\n", configPath)
	fmt.Fprintln(out, "apply it: systemctl --user restart llama-swap")
	return nil
}

// llamaSwapConfigPath is the rendered config's destination —
// $XDG_CONFIG_HOME/llama-swap/config.yaml, where the llama-swap systemd unit
// reads it. Not under paths.ConfigHome(): the file belongs to llama-swap,
// vibe only writes it.
func llamaSwapConfigPath() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "llama-swap", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "llama-swap", "config.yaml")
}

// routerExtrasPath is the default extras file, kept beside the backend defs
// (it is vibe-owned input, unlike the rendered llama-swap output).
func routerExtrasPath() string {
	return filepath.Join(paths.ConfigHome(), "router-extras.yaml")
}

// resolveLlamaServerBinary picks the llama-server path rendered into cmds:
// explicit flag, then the daemon config's llama_binary, then the conventional
// install symlink when it exists. The bare-name fallback keeps rendering
// usable, but llama-swap's unit $PATH may not resolve it — hence the
// preference for absolute paths.
func resolveLlamaServerBinary(flag string) string {
	if flag != "" {
		return flag
	}
	if cfg, err := daemon.LoadConfig(); err == nil && cfg.LlamaBinary != "" {
		return cfg.LlamaBinary
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "llama-server")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "llama-server"
}
