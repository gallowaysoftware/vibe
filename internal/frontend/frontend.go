// Package frontend wires up the frontend tool described by a profile. In
// Phase 1 only the `external` kind is supported: vibe writes a config file
// for a tool the user launches themselves.
package frontend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gallowaysoftware/vibe/internal/profile"
)

type Result struct {
	WroteFile       string
	RestartRequired bool
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
	expanded, err := p.ExpandTemplate(ctx)
	if err != nil {
		return nil, fmt.Errorf("expand template: %w", err)
	}
	body, err := json.MarshalIndent(expanded, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.Frontend.WriteFile), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir for %s: %w", p.Frontend.WriteFile, err)
	}
	if err := os.WriteFile(p.Frontend.WriteFile, append(body, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", p.Frontend.WriteFile, err)
	}
	return &Result{
		WroteFile:       p.Frontend.WriteFile,
		RestartRequired: p.Frontend.RestartRequired,
	}, nil
}
