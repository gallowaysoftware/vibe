package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// backendCmd groups commands over reusable backend definitions
// ($XDG_CONFIG_HOME/vibe/backends/<name>.yaml). Backends are what vamp
// capabilities resolve to, so "which model does capability X actually
// run?" starts here instead of cat-ing YAML in two directories.
func backendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backend",
		Short: "Manage reusable backend definitions.",
		Long: "vibe backend groups commands that operate on backend YAML files " +
			"under $XDG_CONFIG_HOME/vibe/backends/. A backend is a named " +
			"model-server spec shared by profiles (via backend_ref) and " +
			"targeted directly by vamp capabilities.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(backendListCmd())
	cmd.AddCommand(backendShowCmd())
	cmd.AddCommand(backendStartCmd())
	return cmd
}

// backendListCmd reads the backends dir straight off disk — like `vibe
// list`, asking "which backends exist" must never spawn the daemon.
func backendListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every backend YAML with name + mode + kind + referencing profiles.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return renderBackendList(cmd.OutOrStdout())
		},
	}
}

// renderBackendList prints one aligned row per backend definition. Invalid
// files get an `(invalid: ...)` note rather than aborting the listing, so
// one broken file doesn't hide the rest. The USED BY column lists the
// profiles whose backend_ref points at each backend; "-" marks
// capability-only (or stale) backends.
func renderBackendList(out io.Writer) error {
	names, err := profile.ListBackends()
	if err != nil {
		return fmt.Errorf("read %s: %w", paths.BackendsDir(), err)
	}
	if len(names) == 0 {
		fmt.Fprintf(out, "no backends yet (define one under %s)\n", paths.BackendsDir())
		return nil
	}
	usedBy := backendRefsIn(paths.ProfilesDir())
	fmt.Fprintf(out, "%-22s  %-10s  %-24s  %s\n", "NAME", "MODE", "BACKEND", "USED BY")
	for _, n := range names {
		def, err := profile.LoadBackend(n)
		if err != nil {
			fmt.Fprintf(out, "%-22s  %-10s  %-24s  (invalid: %v)\n", n, "?", "?", err)
			continue
		}
		mode := def.Mode
		if mode == "" {
			mode = profile.ModeActive
		}
		kind := backendKind(def.Backend)
		if def.Backend.External {
			// The single most useful fact about an external backend at a
			// glance: `vibe start` won't launch this — the router serves it.
			kind += " (external)"
		}
		refs := "-"
		if len(usedBy[n]) > 0 {
			refs = strings.Join(usedBy[n], ", ")
		}
		fmt.Fprintf(out, "%-22s  %-10s  %-24s  %s\n", n, mode, kind, refs)
	}
	return nil
}

func backendKind(b profile.Backend) string {
	switch {
	case b.LlamaServer != nil:
		return "llama_server"
	case b.TabbyAPI != nil:
		return "tabby_api"
	case b.HTTPServer != nil:
		return "http_server"
	case b.ComfyUI != nil:
		return "comfyui"
	case b.CloudPeer != nil:
		return "cloud_peer"
	case b.MLXServer != nil:
		return "mlx_server"
	}
	return "?"
}

// backendRefsIn returns, per backend name, the profiles under dir whose
// backend_ref points at it. Decoded leniently with a tiny non-strict
// struct instead of profile.Load, because Load resolves refs eagerly and
// fails on exactly the profiles a diagnostic caller cares about (a ref to
// a broken or deleted backend).
func backendRefsIn(dir string) map[string][]string {
	refs := map[string][]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return refs
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p struct {
			BackendRef string `yaml:"backend_ref"`
		}
		if yaml.Unmarshal(raw, &p) != nil || p.BackendRef == "" {
			continue
		}
		refs[p.BackendRef] = append(refs[p.BackendRef], strings.TrimSuffix(e.Name(), ".yaml"))
	}
	return refs
}

// backendShowCmd mirrors `vibe profile show` for backends: full
// load-pipeline output (tilde expansion, validation) instead of raw cat.
func backendShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print the loaded + validated backend YAML for <name>.",
		Long: "show reads $XDG_CONFIG_HOME/vibe/backends/<name>.yaml, runs it " +
			"through the backend loader (tilde expansion, validation), and " +
			"prints the result. Errors out on missing files or validation " +
			"failures so it doubles as a lint check.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBackendNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			def, err := profile.LoadBackend(name)
			if err != nil {
				return fmt.Errorf("backend %q failed to load: %w (run `vibe backend list` to see available)", name, err)
			}
			raw, err := yaml.Marshal(def)
			if err != nil {
				return fmt.Errorf("marshal backend: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# %s\n# loaded from %s\n%s",
				name, filepath.Join(paths.BackendsDir(), name+".yaml"), raw)
			return nil
		},
	}
}

// backendStartCmd activates a backend with NO frontend — the daemon
// synthesizes a frontend-less profile whose name IS the backend name
// (StartRequest.backend), so this is the CLI face of what vamp capability
// resolution does. `vibe start` stays profile-only: keeping the two nouns
// on separate commands enforces the daemon's profile/backend XOR
// structurally instead of via flag validation. No pull step — `vibe pull`
// is profile-keyed, so missing weights surface as a daemon start error.
func backendStartCmd() *cobra.Command {
	var noVRAMCheck bool
	cmd := &cobra.Command{
		Use:               "start <name>",
		Short:             "Activate a backend with no frontend (auto-spawns the daemon if needed).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBackendNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			cancelTail := startProgressTail(cmd.OutOrStdout())
			defer cancelTail()
			r, err := newClient().StartBackendOpts(ctx, args[0], noVRAMCheck)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "started backend %s\n", r.Status.Profile)
			fmt.Fprintf(out, "  backend: %s\n", r.Status.BackendAddr)
			if r.Status.ProxyAddr != "" {
				fmt.Fprintf(out, "  proxy:   %s\n", r.Status.ProxyAddr)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false, noVRAMCheckUsage)
	return cmd
}

// completeBackendNames mirrors completeProfileNames for backend
// definitions: only names that load + validate are suggested. Completion
// is a UX nicety, not a diagnostic — `vibe doctor` surfaces broken ones.
func completeBackendNames(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := profile.ListBackends()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	valid := make([]string, 0, len(names))
	for _, n := range names {
		if _, err := profile.LoadBackend(n); err != nil {
			continue
		}
		valid = append(valid, n)
	}
	return valid, cobra.ShellCompDirectiveNoFileComp
}
