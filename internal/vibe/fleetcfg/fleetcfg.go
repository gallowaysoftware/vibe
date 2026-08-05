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
	// Power is the cell's declared wattage (C7b §4). Absent means the
	// savings screen renders an em dash for this cell's electricity and
	// labels its net "net (power not counted)" — never a zero.
	Power *Power `yaml:"power,omitempty"`
	// CapitalCost is what this cell's hardware cost, in the same currency
	// as the price table (USD). Absent means NO payback bar for the cell:
	// not 0%, not infinity, not an invented denominator.
	//
	// For dual-use hardware the convention is the UPGRADE DELTA — the
	// card's price minus what a gaming-adequate card would have cost.
	// Usage-weighted attribution (serving hours over powered hours) is
	// rejected by name: llama-swap holding a model resident means an idle
	// cell attributes ~90% of the GPU to AI for sitting there, so the
	// metric rewards idling.
	CapitalCost float64 `yaml:"capital_cost,omitempty"`
	// CapitalNote is REQUIRED alongside CapitalCost and is rendered beside
	// the bar. A capital number with no stated basis is unarguable, and
	// this screen's whole value is being arguable.
	CapitalNote string `yaml:"capital_note,omitempty"`
}

// Power is a cell's declared power draw. Declared is sufficient because
// ±40% on a term that lands around 12% of net savings is ±5% of the
// answer — inside the noise of the equivalence choice (72x) and the
// cache tier (5x). Measurement is genuinely unattractive: nvidia-smi has
// no cumulative energy field on query-gpu, RAPL is root-only post
// PLATYPUS, and the Apple path needs cgo into a private framework.
type Power struct {
	// Source is the discriminator. Only "declared" is BUILT. "nvidia_smi"
	// and "ha_entity" are named here as the future values so the field
	// shape does not change when one of them lands — Home Assistant's
	// GET /api/states/<entity> is the one honest measurement path and is
	// stdlib-trivial.
	Source string `yaml:"source"`
	// WattsIdle is the draw while a model is resident but idle; WattsBusy
	// while generating. They are billed separately, because a cell
	// holding a warm model 20h/day is mostly reporting its idle constant
	// — and C4's warm targets deliberately increase exactly that.
	WattsIdle float64 `yaml:"watts_idle"`
	WattsBusy float64 `yaml:"watts_busy"`
}

// PowerSources. Only PowerDeclared is implemented.
const (
	PowerDeclared  = "declared"
	PowerNvidiaSMI = "nvidia_smi"
	PowerHAEntity  = "ha_entity"
)

// Pricing is the fleet-level half of the savings screen's config
// (C7b §1). Everything here is a claim the fleet's owner is making, not
// one this repo ships: there is deliberately NO default twin mapping and
// NO default frontier mapping.
type Pricing struct {
	// ElectricityPricePerKWh prices the energy term (reference example
	// 0.15). Zero means electricity is not counted and the page says so.
	ElectricityPricePerKWh float64 `yaml:"electricity_price_per_kwh,omitempty"`
	// Models maps a LOCAL model id (the canonical backend def name C7a
	// keys the ledger on) to its pricing claim.
	Models map[string]ModelPricing `yaml:"models,omitempty"`
	// Frontier is the optional "if I'd used a frontier model instead"
	// row. It requires a written rationale, which is rendered beside the
	// number: a shipped default mapping would be an unearned claim by
	// this repo, while a config field with a rationale is the owner's
	// claim — and a claim someone can argue with.
	Frontier *Frontier `yaml:"frontier,omitempty"`
}

// ModelPricing is one local model's pricing claim.
type ModelPricing struct {
	// Twin is the same open-weight model as a real host spells it
	// (e.g. "Qwen/Qwen3-Coder-30B-A3B-Instruct"). The headline is the
	// MEDIAN across hosts that serve it, rendered as a range. Comparing
	// against a frontier model instead moves the answer about 72x.
	Twin string `yaml:"twin,omitempty"`
	// PricedAs is an exact price-table model id, for ids that ARE a
	// hosted model rather than a local one: cloud_peer traffic through
	// the front is a bill reconstruction, not a counterfactual.
	PricedAs string `yaml:"priced_as,omitempty"`
	// Counterfactual scales the notional cost by the tier the work would
	// really have run at: interactive (1.0), batch (0.5 — Anthropic's
	// Batch API is a flat 50%, and overnight sweeps are batch-shaped), or
	// free (0.0). Default interactive.
	Counterfactual string `yaml:"counterfactual,omitempty"`
}

// Counterfactual tiers.
const (
	CounterfactualInteractive = "interactive"
	CounterfactualBatch       = "batch"
	CounterfactualFree        = "free"
)

// Multiplier is the counterfactual's scale factor. An unset tier is
// interactive, the most expensive and therefore the reading most likely
// to be argued with.
func (m ModelPricing) Multiplier() float64 {
	switch m.Counterfactual {
	case CounterfactualBatch:
		return 0.5
	case CounterfactualFree:
		return 0
	default:
		return 1
	}
}

// Frontier is the optional frontier comparable.
type Frontier struct {
	Model string `yaml:"model"`
	// Rationale is REQUIRED and rendered on screen beside the number.
	Rationale string `yaml:"rationale"`
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
	// Pricing is the savings screen's equivalence and energy config
	// (C7b). Absent means the screen renders tokens and no money.
	Pricing *Pricing `yaml:"pricing,omitempty"`
}

// CredentialPreference selects which of the two correct precedences a
// caller applies when both $VIBE_TOKEN and a per-cell token_file could
// answer. Both orders are right for their context and neither is
// discoverable from a 401, which is why they live here as named values
// instead of as two hand-rolled switch statements (fleet-control C13).
type CredentialPreference int

const (
	// PreferCellFile is the LONG-LIVED process order (fleetd): the
	// per-cell token_file wins and $VIBE_TOKEN is only the fallback for
	// cells that declare none. One env var in a process that outlives
	// every command must not silently void every per-cell credential in
	// hosts.yaml (C6's NIT-E).
	PreferCellFile CredentialPreference = iota
	// PreferEnv is the INTERACTIVE order (the CLI): an explicit
	// $VIBE_TOKEN wins, because exporting it for one command is exactly
	// how an operator says "use this one".
	PreferEnv
)

// Credential kinds — the machine-readable half of a resolution, so a
// report can group by origin without parsing prose.
const (
	CredCellFile = "token_file"
	CredEnv      = "env"
	CredLocal    = "local"
)

// Credential is one resolved cell credential. Source is the ORIGIN, for
// diagnostics and error messages; it never carries the token value.
type Credential struct {
	Token  string
	Kind   string
	Source string
}

// CellCredential resolves the bearer a caller should present to one
// cell's daemon. localToken supplies the local box's own control-plane
// token as the last resort (a func so the file read only happens when
// that branch is actually taken); nil means "no local fallback".
//
// An unreadable or empty token_file is a typed error rather than a
// silent fallthrough: both turn a typo into an opaque 401 from a remote
// box, which is the failure C6's MIN-P fixed for the CLI and this makes
// uniform.
func (f *File) CellCredential(name, envToken string, pref CredentialPreference, localToken func() string) (Credential, error) {
	if f == nil {
		return Credential{}, fmt.Errorf("unknown cell %q (no hosts.yaml)", name)
	}
	c, ok := f.Cells[name]
	if !ok {
		return Credential{}, fmt.Errorf("unknown cell %q (not in hosts.yaml)", name)
	}
	envToken = strings.TrimSpace(envToken)
	if pref == PreferEnv && envToken != "" {
		return Credential{Token: envToken, Kind: CredEnv, Source: "$VIBE_TOKEN"}, nil
	}
	if c.TokenFile != "" {
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return Credential{}, fmt.Errorf("read cells.%s.token_file %s: %w", name, c.TokenFile, err)
		}
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return Credential{}, fmt.Errorf("cells.%s.token_file %s is empty", name, c.TokenFile)
		}
		return Credential{Token: tok, Kind: CredCellFile, Source: "cells." + name + ".token_file (" + c.TokenFile + ")"}, nil
	}
	if envToken != "" {
		return Credential{Token: envToken, Kind: CredEnv, Source: "$VIBE_TOKEN"}, nil
	}
	var tok string
	if localToken != nil {
		tok = strings.TrimSpace(localToken())
	}
	return Credential{Token: tok, Kind: CredLocal, Source: "this box's own control-plane token"}, nil
}

// ModelPricingFor returns the pricing claim for a local model id.
func (f *File) ModelPricingFor(model string) (ModelPricing, bool) {
	if f == nil || f.Pricing == nil {
		return ModelPricing{}, false
	}
	mp, ok := f.Pricing.Models[model]
	return mp, ok
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
	for name, c := range f.Cells {
		if err := validatePower(name, c.Power); err != nil {
			return err
		}
		switch {
		case c.CapitalCost < 0:
			return fmt.Errorf("cells.%s: capital_cost must not be negative", name)
		case c.CapitalCost > 0 && strings.TrimSpace(c.CapitalNote) == "":
			return fmt.Errorf("cells.%s: capital_cost requires capital_note (what the number covers — e.g. \"dual-use GPU, upgrade delta over a gaming-adequate card\"); it is rendered beside the payback bar", name)
		case c.CapitalCost == 0 && strings.TrimSpace(c.CapitalNote) != "":
			return fmt.Errorf("cells.%s: capital_note without capital_cost (no capital number means no payback bar at all)", name)
		}
	}
	if err := f.validatePricing(); err != nil {
		return err
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

// WarmClassRefusal reports why a CHAT-completion warm must not be fired
// at model, or "" when it may be. It lives here, on the file that carries
// the declaration, because every warm in the fleet is the same verb and
// the guard has to be the same sentence: fleetmcp's `warm_model` (the
// operator's verb) and the three automated producers — C4's warm-target
// restore, C4's warm schedule, C14's post-wake warms — plus the piggyback
// queue they all fall back to. `warm_model` alone honoured it until the
// lab watched a `warm_schedule` fire five chat completions at an
// embed-class id the same fleetd had just refused by hand.
//
// There is deliberately no class-dispatched warm body here. C8's prober
// does have an embed path, so "warm an embedding model" is a coherent
// want — but it needs the right verb per class (rerank takes a query plus
// documents, stt multipart audio, tts a voice), and on the two classes
// with an obvious body the warm stops being free: an embed warm is a
// fully metered request on the `embed` basis, where C7a has no `poke_req`
// equivalent to keep it out of the billable figures. A 96-a-day schedule
// would quietly inflate an append-only ledger. That is a feature with a
// phase doc, not a line in a bug fix; until it exists the honest answer
// is to refuse and say which config is wrong.
func (f *File) WarmClassRefusal(model string) string {
	if f == nil {
		return ""
	}
	class, pinned := f.ModelClasses[model]
	if !pinned || class == ModelClassChat {
		return ""
	}
	return fmt.Sprintf("%s is %s-class (per hosts.yaml model_classes), not chat: "+
		"warming it with a chat completion would load it for nothing — its pinned cell's "+
		"config is what needs fixing", model, class)
}

func validatePower(cell string, p *Power) error {
	if p == nil {
		return nil
	}
	switch p.Source {
	case PowerDeclared:
	case PowerNvidiaSMI, PowerHAEntity:
		return fmt.Errorf("cells.%s.power.source %q is a named future value, not implemented: use %q "+
			"(C7b ships no power sampler — declared wattage is ±40%% on a ~12%% term, which is inside the noise of the equivalence choice)",
			cell, p.Source, PowerDeclared)
	case "":
		return fmt.Errorf("cells.%s.power.source is required (only %q is implemented)", cell, PowerDeclared)
	default:
		return fmt.Errorf("cells.%s.power.source must be %q (got %q)", cell, PowerDeclared, p.Source)
	}
	if p.WattsIdle < 0 || p.WattsBusy < 0 {
		return fmt.Errorf("cells.%s.power: watts must not be negative", cell)
	}
	if p.WattsIdle == 0 && p.WattsBusy == 0 {
		return fmt.Errorf("cells.%s.power: declared power needs watts_idle and/or watts_busy (a zero-watt cell is not a measurement, it is a missing one)", cell)
	}
	if p.WattsBusy > 0 && p.WattsBusy < p.WattsIdle {
		return fmt.Errorf("cells.%s.power: watts_busy (%g) is below watts_idle (%g)", cell, p.WattsBusy, p.WattsIdle)
	}
	return nil
}

func (f *File) validatePricing() error {
	p := f.Pricing
	if p == nil {
		return nil
	}
	if p.ElectricityPricePerKWh < 0 {
		return errors.New("pricing.electricity_price_per_kwh must not be negative")
	}
	for id, mp := range p.Models {
		if strings.TrimSpace(id) == "" {
			return errors.New("pricing.models: empty model id")
		}
		switch mp.Counterfactual {
		case "", CounterfactualInteractive, CounterfactualBatch, CounterfactualFree:
		default:
			return fmt.Errorf("pricing.models.%s.counterfactual must be one of %q, %q, %q (got %q)",
				id, CounterfactualInteractive, CounterfactualBatch, CounterfactualFree, mp.Counterfactual)
		}
		if strings.TrimSpace(mp.Twin) == "" && strings.TrimSpace(mp.PricedAs) == "" {
			return fmt.Errorf("pricing.models.%s: set twin (the same open-weight model as a real host spells it) or priced_as (an exact price-table id)", id)
		}
	}
	if fr := p.Frontier; fr != nil {
		if strings.TrimSpace(fr.Model) == "" {
			return errors.New("pricing.frontier.model is required when a frontier comparable is declared")
		}
		if strings.TrimSpace(fr.Rationale) == "" {
			return errors.New("pricing.frontier.rationale is required: the frontier row is a claim about work you would actually have paid a frontier model to do, " +
				"and it renders beside the number so it can be argued with")
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
