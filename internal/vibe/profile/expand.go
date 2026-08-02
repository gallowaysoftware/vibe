package profile

import (
	"fmt"
	"regexp"
)

// ExpandContext supplies values that ${VAR} placeholders in a frontend's
// write_file, env, or template can reference.
type ExpandContext struct {
	VibeAPI      string // OpenAI-compatible inference URL
	ModelAlias   string
	ModelContext int
	WriteFile    string // resolved write_file path; set by Activate after expanding write_file itself
	VibeStateDir string // $XDG_STATE_HOME/vibe
	// VibeSearch is the fleet's retrieval-plane base URL (search + fetch),
	// from `search_url` in the daemon config. Empty when the operator has
	// not deployed one — see optionalVars for why that is not the same as
	// expanding to "".
	VibeSearch string
}

// optionalVars are template variables that exist as a concept but may be
// unconfigured. They are left OUT of the expansion map when empty so a
// profile referencing one fails loudly instead of rendering an empty string
// into a harness config — a client silently pointed at "" is far harder to
// diagnose than a start that refuses with a reason.
var optionalVars = map[string]string{
	"VIBE_SEARCH":   "set search_url in the vibe daemon config (~/.config/vibe/config.yaml) to the retrieval plane's base URL",
	"MODEL_ALIAS":   "this backend serves no single model id (a cloud_peer with more than one entry in models:); name the model literally in the template",
	"MODEL_CONTEXT": "this backend declares no context window; set backend.cloud_peer.context, or write the window literally in the template",
}

func (c ExpandContext) vars() map[string]any {
	v := map[string]any{
		"VIBE_API":       c.VibeAPI,
		"WRITE_FILE":     c.WriteFile,
		"VIBE_STATE_DIR": c.VibeStateDir,
	}
	// The model vars are optional for the same reason VIBE_SEARCH is. Every
	// backend used to guarantee both, so rendering the zero value was
	// harmless; a cloud_peer need not, and "" / 0 reaching a harness config
	// means an unroutable model id or a context window of zero — failures
	// that surface far from their cause.
	if c.ModelAlias != "" {
		v["MODEL_ALIAS"] = c.ModelAlias
	}
	if c.ModelContext != 0 {
		v["MODEL_CONTEXT"] = c.ModelContext
	}
	if c.VibeSearch != "" {
		v["VIBE_SEARCH"] = c.VibeSearch
	}
	return v
}

// unknownVarError distinguishes "you typo'd a variable name" from "this
// variable is real but not configured on this machine", which are different
// problems with different fixes.
func unknownVarError(name string) error {
	if hint, ok := optionalVars[name]; ok {
		return fmt.Errorf("template variable ${%s} is not configured: %s", name, hint)
	}
	return fmt.Errorf("unknown template variable ${%s}", name)
}

var (
	varRE      = regexp.MustCompile(`\$\{([A-Z_]+)\}`)
	wholeVarRE = regexp.MustCompile(`^\$\{([A-Z_]+)\}$`)
)

// ExpandTemplate returns a deep copy of tmpl with all ${VAR} placeholders
// substituted. When a string consists of exactly one ${VAR} reference, the
// variable's native type is preserved (so an int stays an int); otherwise
// placeholders are substituted as strings. Unknown variables are an error.
// Package-level (not a Profile method) because a frontend can render several
// files (frontend.write_files), each with its own template map.
func ExpandTemplate(tmpl map[string]any, ctx ExpandContext) (map[string]any, error) {
	out, err := expandValue(tmpl, ctx.vars())
	if err != nil {
		return nil, err
	}
	if out == nil {
		return map[string]any{}, nil
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expanded template root is not a map (got %T)", out)
	}
	return m, nil
}

// ExpandEnv returns the frontend env map with values expanded against ctx.
// Keys are not expanded (they're env var names, fixed at profile-write time).
func (p *Profile) ExpandEnv(ctx ExpandContext) (map[string]string, error) {
	if len(p.Frontend.Env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(p.Frontend.Env))
	vars := ctx.vars()
	for k, v := range p.Frontend.Env {
		ev, err := expandString(v, vars)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out[k] = fmt.Sprintf("%v", ev)
	}
	return out, nil
}

// ExpandPathString expands ${VAR} placeholders in s and returns the result as a
// string. Useful for callers that know the result must be a string (e.g.
// write_file).
func ExpandPathString(s string, ctx ExpandContext) (string, error) {
	v, err := expandString(s, ctx.vars())
	if err != nil {
		return "", err
	}
	if str, ok := v.(string); ok {
		return str, nil
	}
	return fmt.Sprintf("%v", v), nil
}

func expandValue(v any, vars map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return expandString(x, vars)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			ek, err := expandString(k, vars)
			if err != nil {
				return nil, err
			}
			ekStr, ok := ek.(string)
			if !ok {
				return nil, fmt.Errorf("map key %q expanded to non-string %T", k, ek)
			}
			ev, err := expandValue(vv, vars)
			if err != nil {
				return nil, err
			}
			out[ekStr] = ev
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			ev, err := expandValue(vv, vars)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	default:
		return v, nil
	}
}

func expandString(s string, vars map[string]any) (any, error) {
	if m := wholeVarRE.FindStringSubmatch(s); m != nil {
		val, ok := vars[m[1]]
		if !ok {
			return nil, unknownVarError(m[1])
		}
		return val, nil
	}
	var unknown string
	out := varRE.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		val, ok := vars[name]
		if !ok {
			unknown = name
			return match
		}
		return fmt.Sprintf("%v", val)
	})
	if unknown != "" {
		return nil, unknownVarError(unknown)
	}
	return out, nil
}
