package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
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
	var session string
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
				return fmt.Errorf("load profile %q: %w (run `vibe profile list` to see available)", args[0], err)
			}
			if p.Frontend.Kind != profile.FrontendManaged {
				return fmt.Errorf("`vibe run` requires kind=managed (got %q); use `vibe start` for kind=external profiles", p.Frontend.Kind)
			}
			if p.Frontend.Binary == "" {
				return fmt.Errorf("profile %q is kind=managed but frontend.binary is unset; set frontend.binary in %s", args[0], pPath)
			}

			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			client := newClient()
			// Idempotent no-op, matching `vibe start`: a foreground Start
			// against an already-held active slot would only be rejected
			// with "stop first". We don't launch the frontend either — this
			// invocation didn't start the profile, so it must not stop it
			// when the frontend exits.
			if st := runningStatus(ctx, client, args[0]); st != nil {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%s already running (backend: %s)\n", st.Profile, st.BackendAddr)
				fmt.Fprintln(out, "frontend not launched — `vibe stop` first to run it fresh")
				return nil
			}
			if err := pullProfile(ctx, cmd.OutOrStdout(), args[0]); err != nil {
				return err
			}

			cancelTail := startProgressTail(cmd.OutOrStdout())
			defer cancelTail()
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
			if r.Status.ProxyAddr != "" {
				fmt.Printf("  proxy:   %s\n", r.Status.ProxyAddr)
			}
			if r.Frontend != nil && r.Frontend.WroteFile != "" {
				fmt.Printf("  wrote:   %s\n", r.Frontend.WroteFile)
			}
			fmt.Printf("launching %s — Ctrl+D / quit the frontend to stop the profile\n", p.Name)

			// Resume a specific frontend session when requested. Both managed
			// coding frontends we ship (pi, opencode) accept `--session <id>`
			// to continue an existing session by id; append it after the
			// profile's static args so a profile-level arg can't clobber it.
			//
			// Use the daemon's expanded args (r.Frontend.Args), not the
			// locally-loaded p.Frontend.Args — the local copy still has raw
			// ${VAR} placeholders (e.g. ${MODEL_ALIAS}) since only the
			// daemon has the ExpandContext to resolve them. Passing the raw
			// profile's args here reached the child binary unexpanded and
			// failed at first use (found via omp's "${MODEL_ALIAS} not
			// found" error, 2026-07-14). Fall back to the raw args only if
			// an old daemon binary hasn't been reinstalled yet.
			var childArgs []string
			if r.Frontend != nil && len(r.Frontend.Args) > 0 {
				childArgs = append([]string(nil), r.Frontend.Args...)
			} else {
				childArgs = append([]string(nil), p.Frontend.Args...)
			}
			if session != "" {
				childArgs = append(childArgs, "--session", session)
			}

			child := exec.Command(p.Frontend.Binary, childArgs...)
			child.Stdin = os.Stdin
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if p.Frontend.Workdir != "" {
				child.Dir = p.Frontend.Workdir
			}
			if r.Frontend != nil && len(r.Frontend.EnvVars) > 0 {
				env := append([]string(nil), os.Environ()...)
				keys := make([]string, 0, len(r.Frontend.EnvVars))
				for k := range r.Frontend.EnvVars {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					env = append(env, k+"="+r.Frontend.EnvVars[k])
				}
				child.Env = env
			}

			// Forward SIGINT/SIGTERM to the child so the user can ^C without
			// the wrapping `vibe run` swallowing the signal. The frontend
			// gets its own chance to exit cleanly; if the operator hits ^C
			// twice we still want the second one to land on the child too.
			// Notify before Start so a ^C during spawn buffers here instead
			// of killing vibe with the default disposition (which would skip
			// the deferred profile stop); the forwarding goroutine only
			// launches after Start, when child.Process is immutable, so it
			// never races with Start's write.
			sigCh := make(chan os.Signal, 2)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			if err := child.Start(); err != nil {
				return fmt.Errorf("launch %s: %w", p.Frontend.Binary, err)
			}
			go func() {
				for sig := range sigCh {
					_ = child.Process.Signal(sig)
				}
			}()

			if err := child.Wait(); err != nil {
				// A non-zero exit from the frontend is its problem to report;
				// surface the error but still return success-ish so the
				// teardown defer fires cleanly. ExitError specifically is
				// expected (user quit a TUI with q/^C).
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return nil
				}
				return fmt.Errorf("wait for %s: %w", p.Frontend.Binary, err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false, noVRAMCheckUsage)
	cmd.Flags().StringVar(&session, "session", "", "resume a specific frontend session by id (passed to pi/opencode as --session <id>)")
	return cmd
}
