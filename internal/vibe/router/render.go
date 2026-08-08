// Package router renders the llama-swap config from vibe backend
// definitions, making $XDG_CONFIG_HOME/vibe/backends/ the source of truth
// for what the router on :9000 serves (docs/design/router-lifecycle.md,
// phase A2).
package router

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/supervisor"
)

// Options configure a render pass.
type Options struct {
	// LlamaServerBinary is the llama-server executable written into rendered
	// cmds when a def doesn't pin backend.llama_server.binary. Should be an
	// absolute path: llama-swap runs under its own systemd unit whose $PATH
	// vibe doesn't control.
	LlamaServerBinary string

	// ExtrasPath optionally names a YAML file whose top-level sections merge
	// into the rendered config verbatim. It exists for router entries that
	// have no backend-def representation yet: arbitrary-cmd swap tenants
	// (ComfyUI, the smoke rig's slowmodel), peer cells, and routing groups.
	// Merge is strictly additive — a key that would override anything the
	// renderer emitted is an error, so defs stay the single source of truth
	// for everything they can express. Missing file = no extras. On a
	// --cell front render the extras still merge verbatim; whether they
	// make sense on the adopting front is the adopter's business.
	ExtrasPath string

	// Cell names the fleet cell this render is for (fleet-control C2 §6),
	// resolved by the caller: the --cell flag, else the daemon config's
	// fleet.cell. "" means a render with no fleet identity. Cell "front"
	// renders the peers-only front config (the front owns no models);
	// any other cell renders that cell's defs plus unassigned defs as
	// local models, and defs pinned elsewhere are excluded with a warning
	// rather than silently rendered or silently dropped.
	Cell string

	// Hosts is the parsed hosts.yaml the render validates cell:
	// assignments against and, on the front render, sources peer URLs
	// from. Nil means no fleet file: a front render is impossible (no
	// peer addresses), and cell-carrying defs can only be excluded with a
	// warning, never validated.
	Hosts *fleetcfg.File

	// AliasWinners overrides alias ownership with a map computed by the
	// caller (ResolveAliases) over the DECLARED def set. It exists for the
	// one caller that filters defs BEFORE calling Render — fleetd's
	// presence-derived render loop, which prunes roaming cells and
	// excludes strict fingerprint mismatches. Without it, resolution runs
	// over the surviving defs and an owner's departure hands its alias to
	// a co-claimant on another cell: the id keeps answering and names a
	// different model. Nil means resolve from the defs passed in, which is
	// correct for every caller that hands Render the whole checkout.
	AliasWinners map[string][]string

	// Warnf receives one message per def the cell selection excludes.
	// Nil discards; the CLI wires it to stderr so an excluded def is
	// always visible to whoever ran the render.
	Warnf func(format string, args ...any)
}

const (
	// DefaultTTL is the idle-unload timeout for external LLM backends whose
	// def doesn't set lifecycle.ttl. Two hours matches the A1 hand-written
	// config's choice for the primary coding models: long enough to survive
	// a lunch break, short enough that the 5090 frees itself overnight.
	DefaultTTL = 2 * time.Hour

	// minHealthCheckTimeout is the floor for llama-swap's global
	// healthCheckTimeout (seconds). A cold 30B-class load can exceed
	// llama-swap's 120s default; per-def lifecycle.start_timeout raises the
	// ceiling above this floor.
	minHealthCheckTimeout = 300

	startPort = 5800

	headerComment = "# rendered by vibe router render — do not edit; source: ~/.config/vibe/backends/\n"
)

// swapConfig mirrors the subset of llama-swap's (v239) config schema the
// renderer emits. Field order here is emission order.
type swapConfig struct {
	HealthCheckTimeout   int                   `yaml:"healthCheckTimeout"`
	SendLoadingState     bool                  `yaml:"sendLoadingState"`
	StartPort            int                   `yaml:"startPort"`
	IncludeAliasesInList bool                  `yaml:"includeAliasesInList"`
	Models               map[string]*swapModel `yaml:"models,omitempty"`
	Peers                map[string]*swapPeer  `yaml:"peers,omitempty"`
	Hooks                *swapHooks            `yaml:"hooks,omitempty"`
}

type swapModel struct {
	Cmd     string   `yaml:"cmd"`
	Aliases []string `yaml:"aliases,omitempty"`
	// UseModelName overrides the model name llama-swap forwards upstream.
	// Emitted only for mlx_server tenants: mlx_lm.server answers to the
	// filesystem path it was launched with and treats anything else as a
	// model to fetch from HuggingFace, so the client-facing id has to be
	// translated at the router the same way vibe's own proxy translates it.
	UseModelName string `yaml:"useModelName,omitempty"`
	// Proxy/CheckEndpoint are emitted only for non-llama_server tenants
	// (comfyui): llama-server entries use llama-swap's ${PORT} defaults.
	Proxy         string `yaml:"proxy,omitempty"`
	CheckEndpoint string `yaml:"checkEndpoint,omitempty"`
	// TTL is always emitted (never omitted): 0 is llama-swap's "never
	// unload", which must stay distinguishable from "field absent".
	TTL      int  `yaml:"ttl"`
	Unlisted bool `yaml:"unlisted,omitempty"`
}

type swapPeer struct {
	Proxy  string   `yaml:"proxy"`
	APIKey string   `yaml:"apiKey,omitempty"`
	Models []string `yaml:"models"`
}

type swapHooks struct {
	OnStartup struct {
		Preload []string `yaml:"preload"`
	} `yaml:"on_startup"`
}

// LoadDefs loads every backend definition under dir, sorted by name. Any
// invalid def is a hard error: a render that silently skipped a broken def
// would drop a served model from the router config.
func LoadDefs(dir string) ([]*profile.BackendDef, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read backends dir %s: %w", dir, err)
	}
	var defs []*profile.BackendDef
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ext)
		def, err := profile.LoadBackendFrom(dir, name)
		if err != nil {
			return nil, err
		}
		// LoadBackendFrom accepts an empty name: field; the renderer keys
		// models by name, so pin it from the filename.
		if def.Name == "" {
			def.Name = name
		}
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs, nil
}

// Render produces the llama-swap config for the given backend defs.
//
// Selection rule: every def with backend.external: true is rendered —
// llama_server defs become models: entries keyed by def name (the canonical
// model id), cloud_peer defs become peers: stanzas. Everything else
// (non-external defs, vibe-supervised services) is ignored; those stay
// vibe-managed. External tabby_api defs are rejected: nothing serves them
// yet, and silently dropping a router-served model would strand its clients.
//
// Fleet selection (fleet-control C2 §6) refines which LLM defs render when
// Options.Cell is set. Rendering for the front cell produces a peers-only
// config: every cell-assigned LLM def joins its cell's peers: stanza (proxy
// from hosts.yaml, models = def name + resolved aliases), unassigned defs
// are excluded with a warning (the front owns no models), and cloud_peer
// defs render subject to the same cell: placement (unassigned renders
// everywhere; assigned renders on its cell, or on the front when assigned
// to it — the front owns the fleet's shared cloud ids). Rendering for any
// other cell renders that cell's defs plus unassigned defs as local
// models: entries; defs pinned to a different cell are excluded with a
// warning. A def whose cell: is not in hosts.yaml is a render error in
// every mode — the name can only be a typo or stale membership, and either
// way the model would silently strand.
func Render(defs []*profile.BackendDef, opts Options) (string, error) {
	defs = slices.Clone(defs)
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	warn := opts.Warnf
	if warn == nil {
		warn = func(string, ...any) {}
	}
	front := opts.Cell == fleetcfg.FrontCell
	if front && !opts.Hosts.HasCells() {
		// The front render's whole content is peer stanzas; without
		// hosts.yaml there are no peer addresses to point them at.
		return "", fmt.Errorf("--cell %s requires hosts.yaml with a cells: section (peer URLs come from fleet membership)", fleetcfg.FrontCell)
	}
	if opts.Cell != "" && !front && opts.Hosts.HasCells() {
		if _, ok := opts.Hosts.Cells[opts.Cell]; !ok {
			// A typo'd --cell would otherwise render an eerily empty
			// config that --check then can't distinguish from truth.
			return "", fmt.Errorf("cell %q is not in hosts.yaml", opts.Cell)
		}
	}

	var modelDefs, peerDefs []*profile.BackendDef
	for _, def := range defs {
		if !def.Backend.External {
			continue
		}
		switch {
		case def.Backend.LlamaServer != nil, def.Backend.ComfyUI != nil, def.Backend.MLXServer != nil:
			if def.Cell != "" && opts.Hosts.HasCells() {
				if _, ok := opts.Hosts.Cells[def.Cell]; !ok {
					return "", fmt.Errorf("backend %s: cell %q is not in hosts.yaml (fix the def or the fleet membership)", def.Name, def.Cell)
				}
			}
			switch {
			case front && def.Trial:
				// fleet-control C18. A trial def is a candidate under
				// evaluation on ONE cell; putting it in the front's peer
				// map would make it routable fleet-wide — the "no silent
				// catalog change" invariant, spent on a model nobody
				// decided to run.
				//
				// The exclusion lives HERE rather than in the writer
				// because there are three renderers (the CLI, fleetd's
				// presence loop, RenderToFile) and only one Render. On the
				// reference fleet the trial def is in the cell's checkout
				// and fleetd's is a different one, so this is unreachable;
				// it is exactly reachable on a single box where fleetd and
				// the cell share $XDG_CONFIG_HOME/vibe/backends, which is
				// the shape `scripts/fleetlab` and every new adopter has.
				warn("backend %s is a trial def (trial: true); excluded from the front render — a trial is evaluated on its own cell and never becomes fleet catalog (promote it by deleting the trial: line and committing the def)", def.Name)
				continue
			case front && def.Cell == fleetcfg.FrontCell:
				// A model def pinned to the front would self-peer: the
				// front proxying to itself is a loop, and the front owns
				// no models by design. Exclude loudly.
				warn("backend %s is assigned to the front cell itself; excluded (the front owns no models — a self-peer is a proxy loop)", def.Name)
				continue
			case front && def.Cell == "":
				warn("backend %s has no cell: assignment; excluded from the front render (the front owns no models)", def.Name)
				continue
			case !front && def.Cell != "" && def.Cell != opts.Cell:
				if opts.Cell == "" {
					warn("backend %s declares cell: %q but this render has no fleet cell (no --cell, no fleet.cell in the daemon config); excluded rather than rendered where it doesn't belong", def.Name, def.Cell)
				} else {
					warn("backend %s is assigned to cell %q; excluded from the render for cell %q", def.Name, def.Cell, opts.Cell)
				}
				continue
			}
			modelDefs = append(modelDefs, def)
		case def.Backend.CloudPeer != nil:
			// Cloud peers are their own stanzas, but placement still
			// follows cell:: an assigned cloud peer renders on its cell's
			// render and on the front render ONLY when assigned to the
			// front (the front owns the fleet's shared cloud ids — a
			// cell-scoped cloud peer must not surface there twice).
			// Unassigned renders everywhere, the status quo for
			// single-box setups.
			if def.Cell != "" && opts.Hosts.HasCells() {
				if _, ok := opts.Hosts.Cells[def.Cell]; !ok {
					return "", fmt.Errorf("backend %s: cell %q is not in hosts.yaml (fix the def or the fleet membership)", def.Name, def.Cell)
				}
			}
			switch {
			case front && def.Cell != "" && def.Cell != fleetcfg.FrontCell:
				// Another cell's cloud peer; the front only carries the
				// fleet's shared (unassigned) and front-assigned cloud ids.
				continue
			case !front && def.Cell != "" && def.Cell != opts.Cell:
				if opts.Cell == "" {
					warn("cloud backend %s declares cell: %q but this render has no fleet cell; excluded rather than rendered where it doesn't belong", def.Name, def.Cell)
				} else {
					warn("cloud backend %s is assigned to cell %q; excluded from the render for cell %q", def.Name, def.Cell, opts.Cell)
				}
				continue
			}
			peerDefs = append(peerDefs, def)
		case def.Backend.TabbyAPI != nil:
			return "", fmt.Errorf("backend %s: external tabby_api defs are not supported by the renderer yet (serve it via llama_server or keep it vibe-supervised)", def.Name)
		default:
			return "", fmt.Errorf("backend %s: external backend kind is not renderable", def.Name)
		}
	}

	// Resolution runs over every DECLARED claimant, not the survivors of
	// the cell/trial selection above: a def excluded from this render is
	// still the fleet's declared owner of its aliases, and resolving over
	// the survivors would transfer the alias to whoever is left instead of
	// dropping it. An exclusion must remove an alias from the catalog,
	// never repoint it at a different model.
	aliases := opts.AliasWinners
	if aliases == nil {
		var err error
		aliases, err = resolveAliases(aliasClaimants(defs))
		if err != nil {
			return "", err
		}
	}

	cfg := swapConfig{
		HealthCheckTimeout:   minHealthCheckTimeout,
		SendLoadingState:     true,
		StartPort:            startPort,
		IncludeAliasesInList: true,
	}

	var preload []string
	if front {
		// The front owns no models: every assigned def becomes a models
		// entry under its cell's peer stanza, keyed by cell name so
		// fleetd and the render agree on the peer's identity.
		for _, def := range modelDefs {
			if cfg.Peers == nil {
				cfg.Peers = map[string]*swapPeer{}
			}
			p := cfg.Peers[def.Cell]
			if p == nil {
				p = &swapPeer{Proxy: opts.Hosts.Cells[def.Cell].URL}
				cfg.Peers[def.Cell] = p
			}
			p.Models = append(p.Models, def.Name)
			p.Models = append(p.Models, aliases[def.Name]...)
		}
	} else {
		for _, def := range modelDefs {
			if cfg.Models == nil {
				cfg.Models = map[string]*swapModel{}
			}
			m := &swapModel{
				Aliases: aliases[def.Name],
				TTL:     int(DefaultTTL / time.Second),
			}
			if c := def.Backend.ComfyUI; c != nil {
				// ComfyUI as a swap tenant (design §16): fixed port from the def
				// (its clients dial /upstream/<id>, so ${PORT} indirection buys
				// nothing), health on /system_stats, cmd re-creates ComfyUISpec's
				// workdir semantics via cd+exec since llama-swap has no workdir.
				cmd, err := comfyuiCmd(def)
				if err != nil {
					return "", err
				}
				m.Cmd = cmd
				m.Proxy = fmt.Sprintf("http://127.0.0.1:%d", c.Port)
				m.CheckEndpoint = "/system_stats"
			} else {
				cmd, err := ModelCmd(def, opts)
				if err != nil {
					return "", err
				}
				m.Cmd = cmd
				if mx := def.Backend.MLXServer; mx != nil {
					// Clients address this by def name; the process only
					// answers to its snapshot path.
					m.UseModelName = mx.ModelDir
				}
			}
			if lc := def.Lifecycle; lc != nil {
				if lc.TTL != nil {
					m.TTL = lc.TTL.Seconds()
				}
				if lc.Preload {
					preload = append(preload, def.Name)
				}
				if lc.StartTimeout != nil && lc.StartTimeout.Seconds() > cfg.HealthCheckTimeout {
					cfg.HealthCheckTimeout = lc.StartTimeout.Seconds()
				}
				// lc.EvictCost is deliberately not rendered: it maps to
				// llama-swap matrix mode (evict_costs), which this renderer
				// doesn't emit until multi-model co-residency lands (A6).
			}
			if def.Router != nil && def.Router.Unlisted {
				m.Unlisted = true
			}
			cfg.Models[def.Name] = m
		}
	}

	for _, def := range peerDefs {
		if cfg.Peers == nil {
			cfg.Peers = map[string]*swapPeer{}
		}
		if _, clash := cfg.Peers[def.Name]; clash {
			// Only reachable on the front render, where a cloud_peer def
			// named after a fleet cell would silently overwrite the
			// cell's peer stanza (or vice versa).
			return "", fmt.Errorf("cloud_peer backend %s collides with the peer stanza for fleet cell %q; rename the def", def.Name, def.Name)
		}
		cp := def.Backend.CloudPeer
		p := &swapPeer{Proxy: cp.BaseURL, Models: slices.Clone(cp.Models)}
		if cp.APIKeyEnv != "" {
			// ${env.VAR} keeps the key out of the rendered file; the value
			// lives in the llama-swap unit's EnvironmentFile.
			p.APIKey = "${env." + cp.APIKeyEnv + "}"
		}
		cfg.Peers[def.Name] = p
	}

	if len(preload) > 0 {
		h := &swapHooks{}
		h.OnStartup.Preload = preload
		cfg.Hooks = h
	}

	if err := checkCatalogIDsUnique(cfg); err != nil {
		return "", err
	}

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal llama-swap config: %w", err)
	}
	if opts.ExtrasPath != "" {
		merged, err := mergeExtras(body, opts.ExtrasPath)
		if err != nil {
			return "", err
		}
		body = merged
	}
	return headerComment + string(body), nil
}

// checkCatalogIDsUnique refuses a config that advertises one client-facing
// id from two entries.
//
// resolveAliases is the alias half of this rule and cannot be the whole of
// it: it sees defs, not the rendered catalog, so it says nothing about a
// llama_server def NAMED after a peer's model id (no alias involved), nor
// about two cloud peers listing the same id. Both render two entries under
// one id in silence, and llama-swap then serves whichever won — which is
// the "advertise an id nothing can route to" failure with the sign flipped:
// the id resolves, just not reliably to the thing the operator meant.
//
// It runs over the rendered cfg rather than over defs on purpose. That is
// the artifact clients actually see, so this catches the class rather than
// the two paths into it that are known today.
//
// Sorted iteration: a map-order-dependent error message would name a
// different pair on different runs, and an error a human cannot reproduce
// is most of the way to no error at all.
func checkCatalogIDsUnique(cfg swapConfig) error {
	owner := map[string]string{}
	claim := func(id, by string) error {
		if prev, dup := owner[id]; dup {
			return fmt.Errorf("rendered catalog advertises %q twice (%s and %s): llama-swap would serve whichever entry wins, so the id resolves to a model nobody chose", id, prev, by)
		}
		owner[id] = by
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
		if err := claim(name, "models."+name); err != nil {
			return err
		}
		for _, a := range cfg.Models[name].Aliases {
			if err := claim(a, "alias of models."+name); err != nil {
				return err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Peers)) {
		for _, id := range cfg.Peers[name].Models {
			if err := claim(id, "peers."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergeExtras folds the extras file's top-level sections into the rendered
// config. models/peers-style map sections merge per key; scalar or absent
// sections are set whole. Any key the renderer already emitted is an error:
// extras extend the config, they never silently override the defs.
func mergeExtras(rendered []byte, path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rendered, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read router extras %s: %w", path, err)
	}
	var base map[string]any
	if err := yaml.Unmarshal(rendered, &base); err != nil {
		return nil, fmt.Errorf("reparse rendered config for extras merge: %w", err)
	}
	var extras map[string]any
	if err := yaml.Unmarshal(raw, &extras); err != nil {
		return nil, fmt.Errorf("parse router extras %s: %w", path, err)
	}
	for section, val := range extras {
		existing, present := base[section]
		if !present {
			base[section] = val
			continue
		}
		baseMap, baseOK := existing.(map[string]any)
		extraMap, extraOK := val.(map[string]any)
		if !baseOK || !extraOK {
			return nil, fmt.Errorf("router extras %s: section %q already rendered from backend defs and cannot be overridden", path, section)
		}
		for k, v := range extraMap {
			if _, dup := baseMap[k]; dup {
				return nil, fmt.Errorf("router extras %s: %s.%s collides with a rendered backend def — express it in the def instead", path, section, k)
			}
			baseMap[k] = v
		}
	}
	out, err := yaml.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	return out, nil
}

// ResolveAliases computes alias ownership over a whole backend-def
// checkout, for a caller that filters defs before rendering and would
// otherwise let an excluded def's alias fall to a co-claimant. The result
// goes back in as Options.AliasWinners. An unresolvable collision is an
// error here exactly as it is inside Render: a fleet whose alias has two
// claimants and no declared owner must be fixed, and letting it resolve
// itself the moment one claimant leaves is the repoint this returns an
// error to prevent.
func ResolveAliases(defs []*profile.BackendDef) (map[string][]string, error) {
	return resolveAliases(aliasClaimants(defs))
}

// aliasClaimants selects the defs whose ids the router serves directly —
// the same set Render turns into models: entries — and, separately, the
// catalog ids a cloud peer serves, keyed to the def that declares them.
//
// cloud_peer defs are still not CLAIMANTS: Render emits no aliases for a
// peer stanza (`p := &swapPeer{Proxy: cp.BaseURL, Models: cp.Models}`), so
// letting a peer win an alias would take that alias off the def that would
// actually have served it and advertise nothing in its place.
//
// But their model ids are RESERVED, which is what was missing. A cloud
// peer's catalog ids are its cloud_peer.models entries, never its def name
// (AGENTS.md, PR #14) — and this function keyed everything off def.Name, so
// a llama_server def whose alias equalled a peer's model id sailed through
// resolution and rendered two entries claiming one client-facing id, with
// no error. Reserving those ids sends that collision down the same path an
// alias equal to a def NAME already takes.
//
// Why an error rather than router.alias_owner disambiguation: alias_owner
// arbitrates between two claimants of the same ALIAS. It has never had
// anything to say about an alias that equals a canonical id — an id is not
// a claim, it is the thing claims are measured against, and `names[a]` has
// always been an unresolvable error for exactly that reason. A peer's model
// ids are canonical in precisely that sense, so they get the same treatment
// with a message that names the peer.
func aliasClaimants(defs []*profile.BackendDef) (claimants []*profile.BackendDef, peerIDs map[string]string) {
	claimants = make([]*profile.BackendDef, 0, len(defs))
	peerIDs = map[string]string{}
	for _, d := range defs {
		if !d.Backend.External {
			continue
		}
		if cp := d.Backend.CloudPeer; cp != nil {
			for _, id := range cp.Models {
				if _, taken := peerIDs[id]; !taken {
					peerIDs[id] = d.Name
				}
			}
			continue
		}
		if d.Backend.LlamaServer != nil || d.Backend.ComfyUI != nil || d.Backend.MLXServer != nil {
			claimants = append(claimants, d)
		}
	}
	return claimants, peerIDs
}

// resolveAliases computes each rendered model's alias list and enforces the
// collision rule: an alias claimed by more than one def is an error unless
// exactly one claimant sets router.alias_owner: true — that def keeps the
// alias and the others render without it.
//
// Default claim when router.aliases is absent: the def's llama_server alias
// field — the model id clients used before the router existed, kept so their
// configs stay valid. A def's own name is never an alias (the models: key
// already serves it), and an alias equal to another def's name is an
// unresolvable error: model ids are canonical and always win.
func resolveAliases(modelDefs []*profile.BackendDef, peerIDs map[string]string) (map[string][]string, error) {
	names := make(map[string]bool, len(modelDefs))
	for _, def := range modelDefs {
		names[def.Name] = true
	}

	type claim struct {
		def   *profile.BackendDef
		owner bool
	}
	claims := map[string][]claim{}
	claimed := map[string][]string{} // def name -> requested aliases, in order
	for _, def := range modelDefs {
		var want []string
		if def.Router != nil && len(def.Router.Aliases) > 0 {
			want = def.Router.Aliases
		} else if ls := def.Backend.LlamaServer; ls != nil && ls.Alias != "" {
			want = []string{ls.Alias}
		} else if mx := def.Backend.MLXServer; mx != nil && mx.Alias != "" {
			want = []string{mx.Alias}
		}
		owner := def.Router != nil && def.Router.AliasOwner
		for _, a := range want {
			if a == def.Name {
				continue
			}
			if names[a] {
				other := a
				return nil, fmt.Errorf("backend %s: alias %q collides with backend def %q (def names are canonical model ids and cannot be claimed as aliases)", def.Name, a, other)
			}
			if peer, taken := peerIDs[a]; taken {
				// Rendering both would put two entries in the catalog under
				// one client-facing id — a models: alias and a peers: model —
				// and llama-swap would route it to whichever won. An id that
				// resolves to whichever entry wins is the same failure as an
				// id nothing can route to; it just takes longer to notice.
				return nil, fmt.Errorf("backend %s: alias %q collides with cloud peer %s's model id (a peer's catalog ids are its cloud_peer.models entries and are canonical, exactly like def names; rename the alias or drop the id from the peer)", def.Name, a, peer)
			}
			claims[a] = append(claims[a], claim{def: def, owner: owner})
			claimed[def.Name] = append(claimed[def.Name], a)
		}
	}

	winner := map[string]string{} // alias -> def name that keeps it
	for alias, cs := range claims {
		if len(cs) == 1 {
			winner[alias] = cs[0].def.Name
			continue
		}
		var owners, all []string
		for _, c := range cs {
			all = append(all, c.def.Name)
			if c.owner {
				owners = append(owners, c.def.Name)
			}
		}
		sort.Strings(all)
		switch len(owners) {
		case 1:
			winner[alias] = owners[0]
		case 0:
			return nil, fmt.Errorf("alias %q is claimed by backends %s; set router.alias_owner: true on exactly one of them", alias, strings.Join(all, ", "))
		default:
			sort.Strings(owners)
			return nil, fmt.Errorf("alias %q has multiple owners (%s) among claimants %s; router.alias_owner: true may be set on only one", alias, strings.Join(owners, ", "), strings.Join(all, ", "))
		}
	}

	out := map[string][]string{}
	for _, def := range modelDefs {
		for _, a := range claimed[def.Name] {
			if winner[a] == def.Name {
				out[def.Name] = append(out[def.Name], a)
			}
		}
	}
	return out, nil
}

// modelCmd renders the llama-server invocation for a def by reusing
// profile.LlamaServerSpec — the exact spec the vibe daemon would launch —
// so router-served and vibe-supervised starts can never drift flag-by-flag.
// The daemon-picked port becomes llama-swap's ${PORT} macro.
// ModelCmd renders the llama-swap cmd for one model def. Exported so the
// announce client fingerprints the exact argv the router renders — a hash
// computed any other way would be a second convention.
func ModelCmd(def *profile.BackendDef, opts Options) (string, error) {
	p := &profile.Profile{Name: def.Name, Backend: def.Backend}
	var (
		spec supervisor.LaunchSpec
		err  error
	)
	if def.Backend.MLXServer != nil {
		// Same builder the daemon uses, so a def launched locally while
		// disconnected and the same def spawned by the router carry
		// byte-identical flags.
		spec, err = profile.MLXServerSpec(p, 0)
	} else if def.Backend.LlamaServer == nil {
		// Callers reach here from announce-supplied model ids (fleetd's
		// fingerprint pass), so the def kind is attacker-influenced input,
		// not a local invariant: return an error rather than dereference.
		return "", fmt.Errorf("backend %s: ModelCmd requires llama_server or mlx_server", def.Name)
	} else {
		bin := opts.LlamaServerBinary
		if b := def.Backend.LlamaServer.Binary; b != "" {
			bin = b
		}
		if bin == "" {
			bin = "llama-server"
		}
		spec, err = profile.LlamaServerSpec(p, bin, 0)
	}
	if err != nil {
		return "", fmt.Errorf("backend %s: %w", def.Name, err)
	}
	args := slices.Clone(spec.Args)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--port" {
			args[i+1] = "${PORT}"
		}
	}
	// One flag (plus its values) per line: llama-swap joins the lines back
	// into one argv, and the block form keeps config diffs per-flag.
	var b strings.Builder
	b.WriteString(spec.Binary)
	for _, tok := range args {
		if strings.HasPrefix(tok, "--") {
			b.WriteString("\n")
		} else {
			b.WriteString(" ")
		}
		b.WriteString(quoteArg(tok))
	}
	return b.String(), nil
}

// comfyuiCmd renders the swap-tenant launch for a ComfyUI def. It mirrors
// profile.ComfyUISpec's choices (defaults, arg order) but must wrap them in
// sh -c "cd … && exec …": ComfyUI resolves models/ and custom_nodes/ relative
// to its checkout, and llama-swap has no workdir field.
func comfyuiCmd(def *profile.BackendDef) (string, error) {
	c := def.Backend.ComfyUI
	if c.Dir == "" {
		return "", fmt.Errorf("backend %s: comfyui.dir is required to render a router entry", def.Name)
	}
	if c.Port <= 0 {
		return "", fmt.Errorf("backend %s: comfyui.port must be fixed (>0) for a router entry — the proxy target can't be random", def.Name)
	}
	python := c.Python
	if python == "" {
		python = "python3"
	}
	listen := c.Listen
	if listen == "" {
		listen = "127.0.0.1"
	}
	// Dir/Python arrive tilde-expanded from LoadBackend normalization.
	args := []string{"main.py", "--listen", listen, "--port", fmt.Sprint(c.Port)}
	args = append(args, c.ExtraArgs...)
	var b strings.Builder
	fmt.Fprintf(&b, "sh -c \"cd %s && exec %s", c.Dir, python)
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(a)
	}
	b.WriteString("\"")
	return b.String(), nil
}

// quoteArg double-quotes an argv token that would otherwise be split or
// mangled by llama-swap's shell-words cmd parsing. The common case (paths,
// flags, numbers) passes through untouched to keep the config readable.
func quoteArg(tok string) string {
	if !strings.ContainsAny(tok, " \t\"'\\") {
		return tok
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(tok) + `"`
}

// Outcome reports what a RenderToFile pass found and did.
type Outcome struct {
	Rendered string
	Changed  bool
	// Diff is a unified diff from the on-disk config to the rendered one;
	// empty when Changed is false.
	Diff string
}

// RenderToFile renders the defs under backendsDir and compares against
// configPath. When write is true and the content differs, the file is
// replaced atomically (temp file + rename in the same directory, so a
// concurrently-restarting llama-swap never reads a half-written config).
func RenderToFile(backendsDir, configPath string, opts Options, write bool) (Outcome, error) {
	defs, err := LoadDefs(backendsDir)
	if err != nil {
		return Outcome{}, err
	}
	rendered, err := Render(defs, opts)
	if err != nil {
		return Outcome{}, err
	}
	prev, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return Outcome{}, fmt.Errorf("read %s: %w", configPath, err)
	}
	out := Outcome{Rendered: rendered, Changed: string(prev) != rendered}
	if !out.Changed {
		return out, nil
	}
	out.Diff = UnifiedDiff(configPath, configPath+" (rendered)", string(prev), rendered)
	if write {
		if err := writeFileAtomic(configPath, []byte(rendered)); err != nil {
			return out, err
		}
	}
	return out, nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
