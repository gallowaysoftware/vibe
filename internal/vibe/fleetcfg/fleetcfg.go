// Package fleetcfg loads $XDG_CONFIG_HOME/vibe/hosts.yaml — the fleet's
// single source of cell membership (fleet-control C1, design doc §4).
// Every fleet-aware component reads this one file: fleetd builds its
// multi-cell registry from it, the CLI reads it on any box for the
// degraded fallback and daemon_url lookups, and the C2 front render
// derives peer addresses from it. Do not introduce a second cell list
// anywhere.
//
// The file is deliberately dependency-free to parse (yaml.v3 only) so
// both the daemon and the CLI can load it without dragging daemon
// internals in. Presence of the file does NOT make a daemon fleetd —
// the registry role is the daemon config's fleet_registry flag; this
// package only describes membership.
package fleetcfg

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"gopkg.in/yaml.v3"
)

// Class qualifies how a cell's absence is interpreted (design doc §4):
// when alarm is appropriate and, from C3, what the front render does
// with the cell's catalog entries on absence.
type Class string

const (
	// ClassAlwaysOn cells are expected up; absence means something is
	// wrong (alarm), and their catalog ids hold on absence.
	ClassAlwaysOn Class = "always_on"
	// ClassOpportunistic cells may be off or reclaimed; absence is
	// normal (no alarm), catalog ids still hold.
	ClassOpportunistic Class = "opportunistic"
	// ClassRoaming cells leave the building; absence is normal, and C3
	// prunes their catalog entries on withdraw/staleness.
	ClassRoaming Class = "roaming"
)

// FrontCell is the registry key the warm path and the C2 render address
// by name; a cells: section without it fails to load.
const FrontCell = "front"

// Cell is one fleet member's static membership record.
type Cell struct {
	// URL is the cell's llama-swap base (probed via /running and
	// /v1/models).
	URL string `yaml:"url"`
	// Class drives display/alarm semantics now, catalog policy in C3.
	Class Class `yaml:"class"`
	// HostProbe is an optional host:port for a plain TCP dial,
	// distinguishing "host up, cell down" from "host down".
	HostProbe string `yaml:"host_probe,omitempty"`
	// DaemonURL is the cell's vibe daemon control plane (C2 remote
	// drain/resume; harmless before that).
	DaemonURL string `yaml:"daemon_url,omitempty"`
	// TokenFile is the path to that cell daemon's bearer token. The
	// path is config; the token value never enters a repo.
	TokenFile string `yaml:"token_file,omitempty"`
}

// File is the parsed hosts.yaml.
type File struct {
	// FleetdURL is where CLI commands find fleetd (see cmd_cell's
	// resolution order).
	FleetdURL string `yaml:"fleetd_url,omitempty"`
	// Cells maps cell name to its membership record.
	Cells map[string]Cell `yaml:"cells,omitempty"`
	// ModelClasses pins non-chat model ids to their class (e.g.
	// "bge-embed: embed"). warm_model refuses to JIT-poke a listed
	// embed/rerank id — warming one means its pinned cell is
	// misconfigured, and a chat completion would load it for nothing.
	// Membership is config; the class of a model is part of it.
	ModelClasses map[string]string `yaml:"model_classes,omitempty"`
}

// Load reads paths.HostsFile(). A missing file is not an error — it
// returns (nil, nil), and callers fall back to single-box behavior.
func Load() (*File, error) { return LoadFrom(paths.HostsFile()) }

// LoadFrom reads and validates a hosts.yaml at an explicit path.
func LoadFrom(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name := range f.Cells {
		c := f.Cells[name]
		c.TokenFile = expandTilde(c.TokenFile)
		f.Cells[name] = c
	}
	return &f, nil
}

// HasCells reports whether the file declares a cells: section — the
// signal that distinguishes a fleet-aware box from a single-box one.
func (f *File) HasCells() bool { return f != nil && len(f.Cells) > 0 }

func (f *File) validate() error {
	if len(f.Cells) == 0 {
		return nil
	}
	if _, ok := f.Cells[FrontCell]; !ok {
		return fmt.Errorf("cells: section requires a cell named %q (the warm path and front render address it by name)", FrontCell)
	}
	for name, c := range f.Cells {
		if c.URL == "" {
			return fmt.Errorf("cells.%s: url is required", name)
		}
		switch c.Class {
		case ClassAlwaysOn, ClassOpportunistic, ClassRoaming:
		default:
			return fmt.Errorf("cells.%s: class must be one of %q, %q, %q (got %q)", name, ClassAlwaysOn, ClassOpportunistic, ClassRoaming, c.Class)
		}
	}
	for id, class := range f.ModelClasses {
		if strings.TrimSpace(id) == "" {
			return errors.New("model_classes: empty model id")
		}
		if strings.TrimSpace(class) == "" {
			return fmt.Errorf("model_classes.%s: empty class", id)
		}
	}
	return nil
}

// expandTilde expands a leading "~/" against the user's home directory.
// Same anchored-prefix-only shape as the profile package's helper; a bad
// path fails at use time with the unexpanded form visible.
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + string(os.PathSeparator) + p[2:]
}
