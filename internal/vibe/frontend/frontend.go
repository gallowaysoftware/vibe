// Package frontend wires up the frontend tool described by a profile. In
// Phase 1 only the `external` kind is supported: vibe writes a config file
// for a tool the user launches themselves, plus an env-var advisory.
package frontend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

type Result struct {
	WroteFile       string
	RestartRequired bool
	// Env is the set of env vars the user should set when launching the
	// external frontend, e.g. OPENCODE_CONFIG=<wrote_file>.
	Env map[string]string
}

func Activate(p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	switch p.Frontend.Kind {
	case profile.FrontendExternal:
		return activateExternal(p, ctx)
	default:
		return nil, fmt.Errorf("frontend.kind %q is not supported yet", p.Frontend.Kind)
	}
}

func activateExternal(p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	// Resolve write_file first; it may reference ${VIBE_STATE_DIR} or other
	// template variables.
	resolved, err := profile.ExpandPathString(p.Frontend.WriteFile, ctx)
	if err != nil {
		return nil, fmt.Errorf("expand write_file: %w", err)
	}
	ctx.WriteFile = resolved

	env, err := p.ExpandEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("expand env: %w", err)
	}

	expanded, err := p.ExpandTemplate(ctx)
	if err != nil {
		return nil, fmt.Errorf("expand template: %w", err)
	}
	body, err := json.MarshalIndent(expanded, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir for %s: %w", resolved, err)
	}
	if err := os.WriteFile(resolved, append(body, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", resolved, err)
	}
	return &Result{
		WroteFile:       resolved,
		RestartRequired: p.Frontend.RestartRequired,
		Env:             env,
	}, nil
}
