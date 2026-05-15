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

	// teardown, when non-nil, is invoked by Deactivate to undo whatever
	// Activate did (e.g. `docker compose down`). It must be safe to call
	// even when the daemon is shutting down on a cancelled context.
	teardown func(ctx context.Context) error
}

// Activate brings up the frontend for p. For kind=external, this writes the
// rendered config file. For kind=docker-compose, this runs `docker compose
// up -d` and polls wait_for. Callers must call Deactivate on the returned
// Result when the profile stops.
func Activate(p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	return ActivateWithContext(context.Background(), p, ctx)
}

// ActivateWithContext is like Activate but threads a context through to any
// underlying long-running operations (compose up + wait_for polling).
func ActivateWithContext(reqCtx context.Context, p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	switch p.Frontend.Kind {
	case profile.FrontendExternal:
		return activateExternal(p, ctx)
	case profile.FrontendDockerCompose:
		return defaultCompose().Activate(reqCtx, p, ctx)
	case profile.FrontendManaged:
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
