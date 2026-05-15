// Package mcp loads Model Context Protocol server definitions from
// $XDG_CONFIG_HOME/vibe/mcp/<name>.yaml. Each file is a free-form YAML map
// (Spec); vibe is a pass-through and does not interpret the fields. The
// frontend (e.g. opencode) is what reads them.
package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"gopkg.in/yaml.v3"
)

// Spec is a single MCP definition. Vibe treats it as opaque: it decodes the
// YAML into a generic map and hands it to the frontend verbatim. It's a type
// alias rather than a named type so nested maps land as plain map[string]any
// when yaml.v3 decodes recursively.
type Spec = map[string]any

// Load reads <MCPDir>/<name>.yaml and returns its decoded contents. If the
// file is missing, the returned error names the MCP and the path that was
// searched.
func Load(name string) (Spec, error) {
	path := filepath.Join(paths.MCPDir(), name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("mcp %q not found at %s", name, path)
		}
		return nil, fmt.Errorf("read mcp %q: %w", name, err)
	}
	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse mcp %q at %s: %w", name, path, err)
	}
	if spec == nil {
		// An empty YAML file decodes to nil; normalize to an empty map so the
		// caller can range/serialize it without a nil check.
		spec = Spec{}
	}
	return spec, nil
}

// LoadMany loads each named MCP in order and returns a map keyed by name. If
// any MCP fails to load, the returned error enumerates the MCP names that DO
// exist in the directory so the caller can correct typos.
func LoadMany(names []string) (map[string]Spec, error) {
	out := make(map[string]Spec, len(names))
	for _, name := range names {
		spec, err := Load(name)
		if err != nil {
			avail := available()
			if len(avail) == 0 {
				return nil, fmt.Errorf("%w (no mcp definitions found in %s)", err, paths.MCPDir())
			}
			return nil, fmt.Errorf("%w (available: %s)", err, strings.Join(avail, ", "))
		}
		out[name] = spec
	}
	return out, nil
}

// available returns the set of MCP names defined in MCPDir (basename minus
// .yaml extension), sorted. Errors reading the directory are swallowed: the
// list is purely advisory, used to make error messages friendlier.
func available() []string {
	entries, err := os.ReadDir(paths.MCPDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".yaml"))
	}
	sort.Strings(names)
	return names
}
