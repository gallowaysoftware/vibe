// Package frontend wires up the frontend tool described by a profile.
//
// Three kinds are supported:
//
//   - external: vibe writes a sidecar config file (e.g. an opencode.json)
//     and surfaces env-var advisories; the user launches the frontend
//     themselves. No teardown needed.
//   - docker-compose: vibe runs `docker compose up -d` against a
//     user-supplied compose file as part of profile activation, polls any
//     configured wait_for endpoints, and runs `docker compose down` on
//     deactivation.
//   - managed: vibe execs a native binary directly (same args/env/workdir
//     model as systemd) and stops it on profile deactivation with the
//     same SIGINT-then-SIGKILL lifecycle the backend supervisor uses for
//     llama-server.
package frontend

import (
	"context"
	"fmt"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// expandArgs expands ${VAR} placeholders in p.Frontend.Args against ctx.
// Shared by the managed driver (which execs the binary itself) and the
// foreground path (which hands expanded args back to the CLI to exec) so
// the two can't drift the way they did when only one of them expanded args
// and the other passed placeholders straight through to the child binary.
func expandArgs(p *profile.Profile, ctx profile.ExpandContext) ([]string, error) {
	args := make([]string, len(p.Frontend.Args))
	for i, a := range p.Frontend.Args {
		ea, err := profile.ExpandPathString(a, ctx)
		if err != nil {
			return nil, fmt.Errorf("expand args[%d] %q: %w", i, a, err)
		}
		args[i] = ea
	}
	return args, nil
}

// Result describes everything the daemon needs to know after a successful
// Activate: any file that was written (for diagnostics / `vibe status`),
// whether the user needs to restart their frontend, the env vars to print,
// and an optional teardown the daemon must invoke on Stop.
type Result struct {
	WroteFile       string
	RestartRequired bool
	// Env is the set of env vars the user should set when launching the
	// external frontend, e.g. OPENCODE_CONFIG=<wrote_file>.
	Env map[string]string

	// Kind is the frontend kind that produced this Result. Used by
	// Deactivate to pick a teardown path; not all kinds need one.
	Kind string

	// Args is p.Frontend.Args with ${VAR} placeholders expanded (e.g.
	// ${MODEL_ALIAS} -> the resolved canonical model id). Populated for
	// kind=managed regardless of foreground/background so callers that
	// exec the binary themselves (`vibe run`, cmd_run.go) don't have to
	// duplicate the expansion the managed driver already does internally —
	// using the profile's own raw Args there passed placeholders straight
	// to the child binary unexpanded (found via omp's "${MODEL_ALIAS}" not
	// found" error, 2026-07-14).
	Args []string

	// teardown, when non-nil, is invoked by Deactivate to undo whatever
	// Activate did (e.g. `docker compose down`). It must be safe to call
	// even when the daemon is shutting down on a cancelled context.
	teardown func(ctx context.Context) error
}

// ActivateWithContext brings up the frontend for p, threading a context
// through to any underlying long-running operations (compose up + wait_for
// polling). For kind=external, this writes the rendered config file. For
// kind=docker-compose, this runs `docker compose up -d` and polls wait_for.
// Callers must call Deactivate on the returned Result when the profile stops.
func ActivateWithContext(reqCtx context.Context, p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	return activate(reqCtx, p, ctx, false)
}

// ActivateForeground is like ActivateWithContext but skips spawning the
// frontend binary on kind=managed — the config file is still rendered and
// env expanded. Callers use this when they intend to exec the frontend
// themselves in their own terminal (`vibe run`). Docker-compose is
// rejected because compose is inherently supervised. External profiles
// don't spawn anything anyway, so they pass through unchanged.
func ActivateForeground(reqCtx context.Context, p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	return activate(reqCtx, p, ctx, true)
}

func activate(reqCtx context.Context, p *profile.Profile, ctx profile.ExpandContext, foreground bool) (*Result, error) {
	switch p.Frontend.Kind {
	case profile.FrontendExternal:
		return activateExternal(p, ctx)
	case profile.FrontendDockerCompose:
		if foreground {
			return nil, fmt.Errorf("foreground mode is not supported for kind=docker-compose")
		}
		return defaultCompose().Activate(reqCtx, p, ctx)
	case profile.FrontendManaged:
		if foreground {
			// Render the config file + env, but don't spawn the binary.
			// `vibe run` will exec the binary in the caller's terminal so
			// TUI frontends like opencode get attached stdio — using its own
			// exec path (cmd_run.go), so it needs the expanded args back
			// rather than expanding them itself a second time.
			resolved, env, err := writeFrontendConfig(p, &ctx)
			if err != nil {
				return nil, err
			}
			args, err := expandArgs(p, ctx)
			if err != nil {
				return nil, err
			}
			return &Result{
				WroteFile:       resolved,
				RestartRequired: p.Frontend.RestartRequired,
				Env:             env,
				Kind:            profile.FrontendManaged,
				Args:            args,
			}, nil
		}
		return defaultManaged().Activate(reqCtx, p, ctx)
	default:
		return nil, fmt.Errorf("frontend.kind %q is not supported yet", p.Frontend.Kind)
	}
}

// Deactivate runs the teardown attached to r (if any). It is safe to call on
// a nil Result and on a Result whose Activate path does not need teardown
// (e.g. external). The provided context bounds the teardown work.
func Deactivate(ctx context.Context, r *Result) error {
	if r == nil || r.teardown == nil {
		return nil
	}
	return r.teardown(ctx)
}
