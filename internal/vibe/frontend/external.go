package frontend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gallowaysoftware/vibe/internal/vibe/mcp"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// activateExternal renders frontend.template (with ${VAR} substitution and
// any frontend.mcps merged in) to frontend.write_file. The user is then
// responsible for launching their tool against the resulting config.
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

	if len(p.Frontend.MCPs) > 0 {
		if _, exists := expanded["mcp"]; exists {
			return nil, fmt.Errorf("frontend.template already defines top-level %q key; cannot merge with frontend.mcps", "mcp")
		}
		specs, err := mcp.LoadMany(p.Frontend.MCPs)
		if err != nil {
			return nil, fmt.Errorf("load mcps: %w", err)
		}
		mcpBlock := make(map[string]any, len(specs))
		for name, spec := range specs {
			mcpBlock[name] = spec
		}
		expanded["mcp"] = mcpBlock
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
		Kind:            profile.FrontendExternal,
	}, nil
}
