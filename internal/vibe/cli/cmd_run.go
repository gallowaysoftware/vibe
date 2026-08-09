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
		Use:   "run <profile> [-- <frontend args>]",
		Short: "Start a profile, exec its frontend in the foreground, and stop the profile when the frontend exits.",
		Example: `  vibe run omp                # plain launch
  vibe run omp -- -c          # forward -c/--continue (or any other frontend-native flag) straight to the binary
  vibe run omp -- -r abc123`,
		// Exactly one profile name is required; anything after a literal
		// `--` is opaque to vibe and forwarded verbatim to the frontend
		// binary (frontends have their own flag surfaces — e.g. omp's
		// -c/--continue, -r/--resume — that vibe shouldn't have to know
		// about or keep in sync with).
		Args: func(cmd *cobra.Command, args []string) error {
			return validateRunArgs(cmd.ArgsLenAtDash(), args)
		},
		ValidArgsFunction: completeProfileNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}

			profileName, passthroughArgs := splitRunArgs(cmd.ArgsLenAtDash(), args)

			// Load the profile locally to get binary/args/workdir. The daemon
			// will load it again from the same on-disk file when we call
			// Start, so this is a cheap pre-flight that also catches "no
			// such profile" before we touch the daemon.
			pPath := filepath.Join(paths.ProfilesDir(), profileName+".yaml")
			p, err := profile.Load(pPath)
			if err != nil {
				return fmt.Errorf("load profile %q: %w (run `vibe profile list` to see available)", profileName, err)
			}
			if p.Frontend.Kind != profile.FrontendManaged {
				return fmt.Errorf("`vibe run` requires kind=managed (got %q); use `vibe start` for kind=external profiles", p.Frontend.Kind)
			}
			if p.Frontend.Binary == "" {
				return fmt.Errorf("profile %q is kind=managed but frontend.binary is unset; set frontend.binary in %s", profileName, pPath)
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
			if st := runningStatus(ctx, client, profileName); st != nil {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%s already running (backend: %s)\n", st.Profile, st.BackendAddr)
				fmt.Fprintln(out, "frontend not launched — `vibe stop` first to run it fresh")
				return nil
			}
			if err := pullProfile(ctx, cmd.OutOrStdout(), profileName); err != nil {
				return err
			}

			cancelTail := startProgressTail(cmd.OutOrStdout())
			defer cancelTail()
			r, err := client.StartWithOptions(ctx, profileName, vibeclient.StartOptions{
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
			// `--` passthrough last: it's the most specific thing the user
			// typed, so it should win if the frontend does last-flag-wins.
			childArgs = append(childArgs, passthroughArgs...)

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

			return frontendExitError(p.Frontend.Binary, child.Wait())
		},
	}
	cmd.Flags().BoolVar(&noVRAMCheck, "no-vram-check", false, noVRAMCheckUsage)
	cmd.Flags().StringVar(&session, "session", "", "resume a specific frontend session by id, pi/opencode's own flag spelling (passed through as --session <id>); other frontends' session flags go after -- instead, e.g. 'vibe run omp -- -r <id>'")
	return cmd
}

// frontendExitError maps the frontend's Wait result onto the wrapper's
// own exit status. `vibe run` is a WRAPPER — README documents it as the
// thing a launcher is pointed at — and root.go's rule applies to it
// exactly as it applies to `vibe cell drain --until-exit`: a wrapper that
// swallows its child's status is a wrapper a launcher cannot be pointed
// at, because Steam, a .desktop Exec= and every `&&` in a shell all read
// the code.
//
// It used to `return nil` on any *exec.ExitError, with a comment claiming
// two things that were both false: that it surfaced the error (it printed
// nothing), and that nil was needed "so the teardown defer fires cleanly"
// (Go runs deferred functions on an error return too — the deferred stop
// above fires either way, and the C24 sibling drainUntilExit has returned
// exitCodeError from this exact position since it shipped, with
// TestC24WrapperPropagatesTheChildsStatus asserting drains=1 resumes=1 on
// that path). The C24 bug flattened every wrapped failure to 1; this one
// flattened it to 0, which is worse: the `&&` chain PROCEEDS.
//
// exitCodeError is what cli.ExitCode reads; a signal death reports
// ExitCode() == -1, which is not a status any process may exit with, so
// it falls through to the ordinary "vibe: …" line and 1.
//
// Pulled out of the RunE closure for the same reason validateRunArgs was:
// so it is unit-testable against a real *exec.ExitError without a full
// cobra/daemon round trip.
func frontendExitError(binary string, err error) error {
	if err == nil {
		return nil
	}
	// ExitError specifically is expected (the user quit a TUI with q/^C),
	// and its code is the frontend's own answer — pass it on rather than
	// inventing one.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitCodeError{msg: binary, code: exitErr.ExitCode()}
	}
	return fmt.Errorf("wait for %s: %w", binary, err)
}

// validateRunArgs enforces exactly one profile name before a `--`, or
// exactly one arg total when there is no `--`. dashAt is cmd.ArgsLenAtDash()
// (-1 when the command line had no literal `--`). Pulled out of the Args
// closure so the dash-boundary logic is unit-testable without a full
// cobra/daemon round trip.
func validateRunArgs(dashAt int, args []string) error {
	if dashAt >= 0 {
		if dashAt != 1 {
			return fmt.Errorf("accepts 1 arg (profile name) before --, received %d", dashAt)
		}
		return nil
	}
	return cobra.ExactArgs(1)(nil, args)
}

// splitRunArgs separates the profile name from any `--`-forwarded frontend
// args, mirroring validateRunArgs's notion of where the boundary is.
func splitRunArgs(dashAt int, args []string) (profileName string, passthroughArgs []string) {
	if dashAt >= 0 {
		return args[0], args[dashAt:]
	}
	return args[0], nil
}
