package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// runningStatus returns the status entry for `name` when it is already up
// and ready — as the active profile or as a service — mirroring the reuse
// condition vibeclient.ensureActive applies. Nil means "not running" (an
// unready same-name active also returns nil; callers fall through to Start,
// which reports the real state).
func runningStatus(ctx context.Context, c *vibeclient.Client, name string) *vibev1.Status {
	active, services, err := c.StatusFull(ctx)
	if err != nil {
		return nil
	}
	if active != nil && active.Running && active.Ready && active.Profile == name {
		return active
	}
	for _, svc := range services {
		if svc != nil && svc.Profile == name && svc.Ready {
			return svc
		}
	}
	return nil
}

// loadLocalProfile pre-flights a start/pull/run target against the local
// config dir so a typo'd name fails fast with the `vibe profile list` hint
// instead of auto-spawning the daemon as a side effect and surfacing a
// wire-level not_found. The daemon re-resolves the same name from the same
// on-disk files, so this rejects nothing the daemon would accept: names
// backing only a backends/<name>.yaml (valid activation targets with no
// profile) pass through with a nil profile.
func loadLocalProfile(name string) (*profile.Profile, string, error) {
	pPath := filepath.Join(paths.ProfilesDir(), name+".yaml")
	p, err := profile.Load(pPath)
	if err == nil {
		return p, pPath, nil
	}
	if _, backendErr := profile.LoadBackend(name); backendErr == nil {
		return nil, "", nil
	}
	return nil, "", fmt.Errorf("load profile %q: %w (run `vibe profile list` to see available)", name, err)
}

func startCmd() *cobra.Command {
	var noVRAMCheck bool
	cmd := &cobra.Command{
		Use:               "start <profile>",
		Short:             "Start a profile (auto-spawns the daemon if needed).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeProfileNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if _, _, err := loadLocalProfile(args[0]); err != nil {
				return err
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			client := newClient()
			// Idempotent no-op when the requested profile is already up and
			// ready: re-running `vibe start chat` should confirm, not exit
			// non-zero with "stop first". The frontend block can't be
			// reproduced here (env exports / wrote-files come only from a
			// real Start RPC), so point at stop+start for a re-render.
			if st := runningStatus(ctx, client, args[0]); st != nil {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%s already running\n", st.Profile)
				fmt.Fprintf(out, "  backend: %s\n", st.BackendAddr)
				if st.ProxyAddr != "" {
					fmt.Fprintf(out, "  proxy:   %s\n", st.ProxyAddr)
				}
				fmt.Fprintln(out, "  (use `vibe stop` + `vibe start` to re-render frontend config)")
				return nil
			}
			// Idempotent: PHASE_DONE immediately if the profile has no HF block
			// or the model is already cached at the right size.
			if err := pullProfile(ctx, cmd.OutOrStdout(), args[0]); err != nil {
				return err
			}
			// Tail the daemon log during the Start RPC so the user sees
			// vram check, model load, backend-ready, compose-up, etc.
			// land in real time instead of staring at silence.
			cancelTail := startProgressTail(cmd.OutOrStdout())
			defer cancelTail()
			r, err := client.StartWithOptions(ctx, args[0], vibeclient.StartOptions{
				NoVRAMCheck: noVRAMCheck,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "started %s\n", r.Status.Profile)
			fmt.Fprintf(out, "  backend: %s\n", r.Status.BackendAddr)
			// Service-mode profiles don't go through the proxy — the
			// daemon returns ProxyAddr empty in that case. Skip the
			// line to avoid surfacing a misleading "proxy: " line for
			// a sidecar that callers reach via BackendAddr directly.
			if r.Status.ProxyAddr != "" {
				fmt.Fprintf(out, "  proxy:   %s\n", r.Status.ProxyAddr)
			}
			if r.Frontend != nil {
				// external = config rendered, user launches the binary;
				// managed / docker-compose = vibe also launched the binary.
				// The label + trailing note differ accordingly. The profile
				// name doubles as the human-readable frontend label here —
				// the historical `frontend.app` cosmetic field was dropped.
				external := r.Frontend.Kind == "external"
				if external {
					fmt.Fprintf(out, "  frontend: %s (external — launch yourself)\n", r.Status.Profile)
				} else {
					fmt.Fprintf(out, "  frontend: %s (running)\n", r.Status.Profile)
				}
				if r.Frontend.Url != "" && !external {
					fmt.Fprintf(out, "  browser: %s\n", r.Frontend.Url)
				}
				if r.Frontend.WroteFile != "" {
					fmt.Fprintf(out, "  wrote:    %s\n", r.Frontend.WroteFile)
				}
				if len(r.Frontend.EnvVars) > 0 {
					if external {
						fmt.Fprintln(out, "  to use:")
					} else {
						fmt.Fprintln(out, "  env (already applied to the running frontend):")
					}
					keys := make([]string, 0, len(r.Frontend.EnvVars))
					for k := range r.Frontend.EnvVars {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						fmt.Fprintf(out, "    export %s=%q\n", k, r.Frontend.EnvVars[k])
					}
				}
				if external && r.Frontend.RestartRequired {
					fmt.Fprintf(out, "  note: %s does not hot-reload — start (or relaunch) it with the env above\n", r.Status.Profile)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false, noVRAMCheckUsage)
	return cmd
}
