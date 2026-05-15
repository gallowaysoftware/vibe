package profile

import (
	"fmt"
	"regexp"
)

// ExpandContext supplies values that ${VAR} placeholders in a frontend template
// can reference.
type ExpandContext struct {
	VibeAPI      string
	ModelAlias   string
	ModelContext int
}

var (
	varRE      = regexp.MustCompile(`\$\{([A-Z_]+)\}`)
	wholeVarRE = regexp.MustCompile(`^\$\{([A-Z_]+)\}$`)
)

// ExpandTemplate returns a deep copy of the frontend template with all ${VAR}
// placeholders substituted. When a string consists of exactly one ${VAR}
// reference, the variable's native type is preserved (so an int stays an int);
// otherwise placeholders are substituted as strings. Unknown variables are an
// error.
func (p *Profile) ExpandTemplate(ctx ExpandContext) (map[string]any, error) {
	vars := map[string]any{
		"VIBE_API":      ctx.VibeAPI,
		"MODEL_ALIAS":   ctx.ModelAlias,
		"MODEL_CONTEXT": ctx.ModelContext,
	}
	out, err := expandValue(p.Frontend.Template, vars)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expanded template root is not a map (got %T)", out)
	}
	return m, nil
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
			return nil, fmt.Errorf("unknown template variable ${%s}", m[1])
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
		return nil, fmt.Errorf("unknown template variable ${%s}", unknown)
	}
	return out, nil
}
