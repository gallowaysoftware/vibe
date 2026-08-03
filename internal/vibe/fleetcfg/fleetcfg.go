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
	"io"
	"net"
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
	// Wake carries the cell's Wake-on-LAN config (C2): fleetd sends the
	// magic packet from its LAN position, or shells to Cmd when L2
	// broadcast is unreachable from where fleetd runs (macvlan).
	Wake *Wake `yaml:"wake,omitempty"`
}

// Wake is a cell's Wake-on-LAN record. Always explicit — waking is never
// triggered by a request or a heuristic.
type Wake struct {
	// MAC is the target NIC's hardware address ("aa:bb:cc:dd:ee:ff").
	MAC string `yaml:"mac"`
	// Broadcast overrides the magic packet's destination
	// (default 255.255.255.255:9) — e.g. a directed subnet broadcast
	// when the fleetd box and the cell share a subnet.
	Broadcast string `yaml:"broadcast,omitempty"`
	// Cmd is a per-cell fallback command run INSTEAD of the packet when
	// set — the escape hatch for fleetd network positions that can't
	// reach L2 broadcast (macvlan hides host ports and broadcast domains
	// from containers). Run via sh -c on the fleetd host.
	Cmd string `yaml:"cmd,omitempty"`
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
	// Hosts is fleet.md §4.1's SSH/systemd host inventory: parsed and
	// ignored here so one hosts.yaml can carry both schemas. Declared
	// because KnownFields(true) would otherwise abort fleetd startup on
	// a file that is perfectly valid for the other reader — and turning
	// strict decoding off instead would let a typo'd cell key silently
	// degrade display semantics, which is what it exists to prevent.
	Hosts map[string]yaml.Node `yaml:"hosts,omitempty"`
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
	// Strict decoding: a typo'd key (host_porbe, fleet_url) must fail
	// loudly at load, not silently degrade a cell's display semantics —
	// same discipline as profile.Load's KnownFields. An empty or
	// comment-only file decodes to io.EOF: that is "no fleet config",
	// not a parse error.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
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
		if c.Wake != nil {
			// mac is required unless a fallback cmd replaces the packet;
			// when present it must be a 48-bit MAC — ParseMAC also admits
			// EUI-64/IPoIB forms, which would build a frame no NIC
			// recognizes (a silent no-op, the worst config-error outcome).
			if c.Wake.MAC == "" && c.Wake.Cmd == "" {
				return fmt.Errorf("cells.%s.wake: mac is required (or set cmd for the fallback path)", name)
			}
			if c.Wake.MAC != "" {
				mac, err := net.ParseMAC(c.Wake.MAC)
				if err != nil {
					return fmt.Errorf("cells.%s.wake: mac %q is invalid: %v", name, c.Wake.MAC, err)
				}
				if len(mac) != 6 {
					return fmt.Errorf("cells.%s.wake: mac %q must be a 48-bit MAC (got %d bytes — EUI-64 and IPoIB forms don't make magic packets)", name, c.Wake.MAC, len(mac))
				}
			}
		}
	}
	for id, class := range f.ModelClasses {
		if strings.TrimSpace(id) == "" {
			return errors.New("model_classes: empty model id")
		}
		if strings.TrimSpace(class) == "" {
			return fmt.Errorf("model_classes.%s: empty class", id)
		}
		// Closed vocabulary: warm_model's guard keys on the class, so a
		// typo'd "embeddings" would silently stop gating an embed id.
		if !KnownModelClass(class) {
			return fmt.Errorf("model_classes.%s: class %q is not one of %s", id, class, strings.Join(ModelClasses, ", "))
		}
	}
	return nil
}

// ModelClasses is the closed vocabulary of model_classes values.
// ModelClassChat is the one class warm_model may still poke — it is a
// chat model, which is exactly what a warm request loads.
var ModelClasses = []string{ModelClassChat, "embed", "rerank", "classify", "stt", "tts", "vision"}

// ModelClassChat marks an id that is a normal chat model; listing it
// documents ownership without gating the warm path.
const ModelClassChat = "chat"

// KnownModelClass reports whether class is in the closed vocabulary.
func KnownModelClass(class string) bool {
	for _, c := range ModelClasses {
		if c == class {
			return true
		}
	}
	return false
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
