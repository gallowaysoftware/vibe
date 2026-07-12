// Package router renders the llama-swap config from vibe backend
// definitions, making $XDG_CONFIG_HOME/vibe/backends/ the source of truth
// for what the router on :9000 serves (docs/design/router-lifecycle.md,
// phase A2).
package router

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// Options configure a render pass.
type Options struct {
	// LlamaServerBinary is the llama-server executable written into rendered
	// cmds when a def doesn't pin backend.llama_server.binary. Should be an
	// absolute path: llama-swap runs under its own systemd unit whose $PATH
	// vibe doesn't control.
	LlamaServerBinary string
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
func Render(defs []*profile.BackendDef, opts Options) (string, error) {
	defs = slices.Clone(defs)
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	var modelDefs, peerDefs []*profile.BackendDef
	for _, def := range defs {
		if !def.Backend.External {
			continue
		}
		switch {
		case def.Backend.LlamaServer != nil:
			modelDefs = append(modelDefs, def)
		case def.Backend.CloudPeer != nil:
			peerDefs = append(peerDefs, def)
		case def.Backend.TabbyAPI != nil:
			return "", fmt.Errorf("backend %s: external tabby_api defs are not supported by the renderer yet (serve it via llama_server or keep it vibe-supervised)", def.Name)
		default:
			return "", fmt.Errorf("backend %s: external backend kind is not renderable", def.Name)
		}
	}

	aliases, err := resolveAliases(modelDefs)
	if err != nil {
		return "", err
	}

	cfg := swapConfig{
		HealthCheckTimeout:   minHealthCheckTimeout,
		SendLoadingState:     true,
		StartPort:            startPort,
		IncludeAliasesInList: true,
	}

	var preload []string
	for _, def := range modelDefs {
		if cfg.Models == nil {
			cfg.Models = map[string]*swapModel{}
		}
		cmd, err := modelCmd(def, opts)
		if err != nil {
			return "", err
		}
		m := &swapModel{
			Cmd:     cmd,
			Aliases: aliases[def.Name],
			TTL:     int(DefaultTTL / time.Second),
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

	for _, def := range peerDefs {
		if cfg.Peers == nil {
			cfg.Peers = map[string]*swapPeer{}
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

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal llama-swap config: %w", err)
	}
	return headerComment + string(body), nil
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
func resolveAliases(modelDefs []*profile.BackendDef) (map[string][]string, error) {
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
		} else if a := def.Backend.LlamaServer.Alias; a != "" {
			want = []string{a}
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
func modelCmd(def *profile.BackendDef, opts Options) (string, error) {
	bin := opts.LlamaServerBinary
	if b := def.Backend.LlamaServer.Binary; b != "" {
		bin = b
	}
	if bin == "" {
		bin = "llama-server"
	}
	p := &profile.Profile{Name: def.Name, Backend: def.Backend}
	spec, err := profile.LlamaServerSpec(p, bin, 0)
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
