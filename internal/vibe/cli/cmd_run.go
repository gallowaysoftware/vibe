package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// runCmd is the "do everything in one shot" command: bring up the backend +
// render the frontend config, exec the frontend in this terminal so it can
// own stdio, then stop the profile when the frontend exits. Built for TUI
// frontends like opencode where a daemon-supervised child is invisible.
//
// Only works on kind=managed profiles (the binary path is on the profile).
// kind=external profiles don't carry a binary path so there's nothing to
// exec; kind=docker-compose is inherently supervised by compose itself.
func runCmd() *cobra.Command {
	var noVRAMCheck bool
	cmd := &cobra.Command{
		Use:               "run <profile>",
		Short:             "Start a profile, exec its frontend in the foreground, and stop the profile when the frontend exits.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeProfileNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			// Load the profile locally to get binary/args/workdir. The daemon
			// will load it again from the same on-disk file when we call
			// Start, so this is a cheap pre-flight that also catches "no
			// such profile" before we touch the daemon.
			pPath := filepath.Join(paths.ProfilesDir(), args[0]+".yaml")
			p, err := profile.Load(pPath)
			if err != nil {
				return fmt.Errorf("load profile %s: %w", args[0], err)
			}
			if p.Frontend.Kind != profile.FrontendManaged {
				return fmt.Errorf("`vibe run` requires kind=managed (got %q); use `vibe start` for kind=external profiles", p.Frontend.Kind)
			}
			if p.Frontend.Binary == "" {
				return errors.New("profile is kind=managed but frontend.binary is unset")
			}

			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			if err := pullProfile(ctx, args[0]); err != nil {
				return err
			}

			client := newClient()
			r, err := client.StartWithOptions(ctx, args[0], vibeclient.StartOptions{
				NoVRAMCheck: noVRAMCheck,
				Foreground:  true,
			})
			if err != nil {
				return err
			}

			// Defer-stop in a function so we can also stop on signal /
			// frontend error paths. Use a fresh background context so a
			// canceled parent doesn't skip teardown.
			stop := func() {
				stopCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if _, err := client.Stop(stopCtx); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "vibe: stop profile on exit: %v\n", err)
				}
			}
			defer stop()

			fmt.Printf("started %s (foreground)\n", r.Status.Profile)
			fmt.Printf("  backend: %s\n", r.Status.BackendAddr)
			fmt.Printf("  proxy:   %s\n", r.Status.ProxyAddr)
			if r.Frontend != nil && r.Frontend.WroteFile != "" {
				fmt.Printf("  wrote:   %s\n", r.Frontend.WroteFile)
			}
			fmt.Printf("launching %s — Ctrl+D / quit the frontend to stop the profile\n", p.Frontend.App)

			child := exec.Command(p.Frontend.Binary, p.Frontend.Args...)
			child.Stdin = os.Stdin
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if p.Frontend.Workdir != "" {
				child.Dir = p.Frontend.Workdir
			}
			if r.Frontend != nil && len(r.Frontend.EnvVars) > 0 {
				env := append([]string(nil), os.Environ()...)
				for k, v := range r.Frontend.EnvVars {
					env = append(env, k+"="+v)
				}
				child.Env = env
			}

			// Forward SIGINT/SIGTERM to the child so the user can ^C without
			// the wrapping `vibe run` swallowing the signal. The frontend
			// gets its own chance to exit cleanly; if the operator hits ^C
			// twice we still want the second one to land on the child too.
			sigCh := make(chan os.Signal, 2)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			go func() {
				for sig := range sigCh {
					if child.Process != nil {
						_ = child.Process.Signal(sig)
					}
				}
			}()

			if err := child.Run(); err != nil {
				// A non-zero exit from the frontend is its problem to report;
				// surface the error but still return success-ish so the
				// teardown defer fires cleanly. ExitError specifically is
				// expected (user quit a TUI with q/^C); other errors mean
				// we failed to launch.
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return nil
				}
				return fmt.Errorf("launch %s: %w", p.Frontend.Binary, err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false,
		"Skip the daemon's pre-flight VRAM check against the profile's estimated_vram_gb.")
	return cmd
}
