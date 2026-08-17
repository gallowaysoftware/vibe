package router

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// warnCapture collects the exclusion warnings a render emits so tests can
// assert the "never silently" half of the cell-selection contract.
type warnCapture struct{ msgs []string }

func (w *warnCapture) warnf(format string, args ...any) {
	w.msgs = append(w.msgs, fmt.Sprintf(format, args...))
}

func (w *warnCapture) contains(subs ...string) bool {
	for _, m := range w.msgs {
		all := true
		for _, s := range subs {
			if !strings.Contains(m, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// testHosts parses a hosts.yaml fixture through fleetcfg's real loader so
// the render validates against the same membership fleetd would use.
func testHosts(t *testing.T, yamlText string) *fleetcfg.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := fleetcfg.LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom hosts fixture: %v", err)
	}
	return f
}

const cellTestHosts = `
cells:
  front: {url: "http://front.lan:9000", class: always_on}
  gpu1:  {url: "http://gpu1.lan:9000", class: always_on}
  gpu2:  {url: "http://gpu2.lan:9000", class: opportunistic}
`

func parseRendered(t *testing.T, out string) parsedConfig {
	t.Helper()
	var cfg parsedConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered output is not YAML: %v\n%s", err, out)
	}
	return cfg
}

// Gate-6 matrix: local renders (a non-front cell, or no fleet identity).

func TestRenderCell_UnassignedDefRendersLocally(t *testing.T) {
	defs := []*profile.BackendDef{llamaDef("m1", "m1-alias")}
	var warns warnCapture
	out, err := Render(defs, Options{
		LlamaServerBinary: testBinary,
		Cell:              "gpu1",
		Hosts:             testHosts(t, cellTestHosts),
		Warnf:             warns.warnf,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg := parseRendered(t, out)
	if _, ok := cfg.Models["m1"]; !ok {
		t.Errorf("unassigned def must render as a local model, models = %v", keys(cfg.Models))
	}
	if len(warns.msgs) != 0 {
		t.Errorf("unexpected warnings: %v", warns.msgs)
	}
}

func TestRenderCell_MatchingCellRendersLocally(t *testing.T) {
	def := llamaDef("m1", "m1-alias")
	def.Cell = "gpu1"
	var warns warnCapture
	out, err := Render([]*profile.BackendDef{def}, Options{
		LlamaServerBinary: testBinary,
		Cell:              "gpu1",
		Hosts:             testHosts(t, cellTestHosts),
		Warnf:             warns.warnf,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg := parseRendered(t, out)
	if _, ok := cfg.Models["m1"]; !ok {
		t.Errorf("def assigned to this cell must render, models = %v", keys(cfg.Models))
	}
	if len(warns.msgs) != 0 {
		t.Errorf("unexpected warnings: %v", warns.msgs)
	}
}

func TestRenderCell_OtherCellExcludedWithWarning(t *testing.T) {
	mine := llamaDef("mine", "")
	mine.Cell = "gpu1"
	theirs := llamaDef("theirs", "")
	theirs.Cell = "gpu2"
	var warns warnCapture
	out, err := Render([]*profile.BackendDef{mine, theirs}, Options{
		LlamaServerBinary: testBinary,
		Cell:              "gpu1",
		Hosts:             testHosts(t, cellTestHosts),
		Warnf:             warns.warnf,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg := parseRendered(t, out)
	if _, ok := cfg.Models["theirs"]; ok {
		t.Error("def assigned to gpu2 must not render on gpu1")
	}
	if _, ok := cfg.Models["mine"]; !ok {
		t.Error("def assigned to gpu1 must render on gpu1")
	}
	if !warns.contains("theirs", "gpu2", "gpu1") {
		t.Errorf("warning must name the def and both cells, got %v", warns.msgs)
	}
}

func TestRenderCell_NoFleetIdentityExcludesAssigned(t *testing.T) {
	// fleet.cell unset and no --cell: a def carrying cell: must be
	// excluded with a warning, never silently rendered into a local
	// config it doesn't belong to.
	assigned := llamaDef("assigned", "")
	assigned.Cell = "gpu1"
	plain := llamaDef("plain", "")
	var warns warnCapture
	out, err := Render([]*profile.BackendDef{assigned, plain}, Options{
		LlamaServerBinary: testBinary,
		Warnf:             warns.warnf,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg := parseRendered(t, out)
	if _, ok := cfg.Models["assigned"]; ok {
		t.Error("cell-assigned def must not render without a fleet identity")
	}
	if _, ok := cfg.Models["plain"]; !ok {
		t.Error("unassigned def must still render")
	}
	if !warns.contains("assigned", "gpu1") {
		t.Errorf("warning must name the def and its cell, got %v", warns.msgs)
	}
}

func TestRenderCell_UnknownCellIsRenderError(t *testing.T) {
	def := llamaDef("m1", "")
	def.Cell = "nowhere"
	_, err := Render([]*profile.BackendDef{def}, Options{
		LlamaServerBinary: testBinary,
		Cell:              "gpu1",
		Hosts:             testHosts(t, cellTestHosts),
	})
	if err == nil || !strings.Contains(err.Error(), "m1") || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("unknown cell must be a render error naming the def and the cell, got %v", err)
	}
}

func TestRenderCell_TargetCellNotInHosts(t *testing.T) {
	_, err := Render([]*profile.BackendDef{llamaDef("m1", "")}, Options{
		LlamaServerBinary: testBinary,
		Cell:              "typo-cell",
		Hosts:             testHosts(t, cellTestHosts),
	})
	if err == nil || !strings.Contains(err.Error(), "typo-cell") {
		t.Fatalf("rendering for a cell hosts.yaml doesn't know must fail, got %v", err)
	}
}

// Gate-6 matrix: the front render.

func TestRenderCell_FrontPeersOnly(t *testing.T) {
	a := llamaDef("qwen-a", "qwen")
	a.Cell = "gpu1"
	a.Router = &profile.RouterOpts{AliasOwner: true}
	b := llamaDef("qwen-a-tools", "qwen")
	b.Cell = "gpu1"
	c := llamaDef("gemma-b", "gemma-it")
	c.Cell = "gpu2"
	unassigned := llamaDef("stray", "")
	cloud := &profile.BackendDef{
		Name: "anthropic",
		Backend: profile.Backend{
			External: true,
			CloudPeer: &profile.CloudPeerBackend{
				BaseURL:   "https://api.anthropic.com",
				APIKeyEnv: "ANTHROPIC_API_KEY",
				Models:    []string{"claude-sonnet-5"},
			},
		},
	}
	var warns warnCapture
	out, err := Render([]*profile.BackendDef{a, b, c, unassigned, cloud}, Options{
		LlamaServerBinary: testBinary,
		Cell:              fleetcfg.FrontCell,
		Hosts:             testHosts(t, cellTestHosts),
		Warnf:             warns.warnf,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg := parseRendered(t, out)

	if cfg.Models != nil {
		t.Errorf("front render must have no models: stanza, got %v", keys(cfg.Models))
	}
	wantPeers := []string{"anthropic", "gpu1", "gpu2"}
	if got := slices.Sorted(maps.Keys(cfg.Peers)); !equalSlices(got, wantPeers) {
		t.Fatalf("peers = %v, want %v", got, wantPeers)
	}
	if got := cfg.Peers["gpu1"].Proxy; got != "http://gpu1.lan:9000" {
		t.Errorf("gpu1 proxy = %q, want the hosts.yaml url", got)
	}
	// The alias-winner union: qwen-a keeps "qwen" (alias_owner), the tools
	// variant renders without it — same resolution as a local render.
	if got, want := cfg.Peers["gpu1"].Models, []string{"qwen-a", "qwen", "qwen-a-tools"}; !equalSlices(got, want) {
		t.Errorf("gpu1 models = %v, want %v", got, want)
	}
	if got, want := cfg.Peers["gpu2"].Models, []string{"gemma-b", "gemma-it"}; !equalSlices(got, want) {
		t.Errorf("gpu2 models = %v, want %v", got, want)
	}
	cp := cfg.Peers["anthropic"]
	if cp.Proxy != "https://api.anthropic.com" || cp.APIKey != "${env.ANTHROPIC_API_KEY}" || !equalSlices(cp.Models, []string{"claude-sonnet-5"}) {
		t.Errorf("cloud peer stanza drifted: %+v", cp)
	}
	if !warns.contains("stray", "front") {
		t.Errorf("unassigned def must be excluded with a warning, got %v", warns.msgs)
	}
	// Top-level fields match a local render's shape (the front's
	// llama-swap speaks to the same clients); the floor stands because
	// no local start_timeout can raise it.
	if cfg.HealthCheckTimeout != minHealthCheckTimeout || !cfg.SendLoadingState || cfg.StartPort != startPort || !cfg.IncludeAliasesInList {
		t.Errorf("front top-level fields = %+v, want the standard floor shape", cfg)
	}
}

// Cloud peers follow cell: placement: assigned-to-a-cell renders on that
// cell's render only; the front carries just unassigned and
// front-assigned cloud ids (the front owns the fleet's shared clouds).
func TestRenderCell_CloudPeerPlacement(t *testing.T) {
	var warns warnCapture
	cloudFor := func(name, cell string) *profile.BackendDef {
		d := &profile.BackendDef{
			Name: name,
			Backend: profile.Backend{
				External:  true,
				CloudPeer: &profile.CloudPeerBackend{BaseURL: "https://api.example.com", Models: []string{"m"}},
			},
		}
		d.Cell = cell
		return d
	}
	defFor := func(name, cell string) *profile.BackendDef {
		d := llamaDef(name, "")
		d.Cell = cell
		return d
	}

	t.Run("cell-assigned cloud excluded from front render", func(t *testing.T) {
		defs := []*profile.BackendDef{defFor("m1", "gpu1"), cloudFor("anthropic", "gpu2"), cloudFor("kimi", fleetcfg.FrontCell)}
		var warns warnCapture
		out, err := Render(defs, Options{
			LlamaServerBinary: testBinary,
			Cell:              fleetcfg.FrontCell,
			Hosts:             testHosts(t, cellTestHosts),
			Warnf:             warns.warnf,
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		cfg := parseRendered(t, out)
		if _, ok := cfg.Peers["anthropic"]; ok {
			t.Errorf("gpu2's cloud peer must not surface on the front render")
		}
		if _, ok := cfg.Peers["kimi"]; !ok {
			t.Errorf("front-assigned cloud peer must render on the front")
		}
	})

	t.Run("cloud renders on its own cell", func(t *testing.T) {
		defs := []*profile.BackendDef{defFor("m1", "gpu2"), cloudFor("anthropic", "gpu2")}
		var warns warnCapture
		out, err := Render(defs, Options{
			LlamaServerBinary: testBinary,
			Cell:              "gpu2",
			Hosts:             testHosts(t, cellTestHosts),
			Warnf:             warns.warnf,
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		cfg := parseRendered(t, out)
		if _, ok := cfg.Peers["anthropic"]; !ok {
			t.Errorf("a cell's cloud peer must render on its own render")
		}
	})

	t.Run("other cell's cloud excluded with warning", func(t *testing.T) {
		defs := []*profile.BackendDef{defFor("m1", "gpu1"), cloudFor("anthropic", "gpu2")}
		var warns warnCapture
		out, err := Render(defs, Options{
			LlamaServerBinary: testBinary,
			Cell:              "gpu1",
			Hosts:             testHosts(t, cellTestHosts),
			Warnf:             warns.warnf,
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		cfg := parseRendered(t, out)
		if _, ok := cfg.Peers["anthropic"]; ok {
			t.Errorf("gpu2's cloud peer must not render on gpu1")
		}
		if !warns.contains("anthropic", "gpu2", "gpu1") {
			t.Errorf("exclusion must warn, got %v", warns.msgs)
		}
	})

	t.Run("front-assigned LLM def self-peers (excluded with warning)", func(t *testing.T) {
		d := llamaDef("front-local", "")
		d.Cell = fleetcfg.FrontCell
		out, err := Render([]*profile.BackendDef{d}, Options{
			LlamaServerBinary: testBinary,
			Cell:              fleetcfg.FrontCell,
			Hosts:             testHosts(t, cellTestHosts),
			Warnf:             warns.warnf,
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		cfg := parseRendered(t, out)
		if _, ok := cfg.Peers[fleetcfg.FrontCell]; ok {
			t.Errorf("a front-assigned model def must not self-peer: %v", cfg.Peers)
		}
		if !warns.contains("front-local", "front owns no models") && !warns.contains("front-local", "proxy loop") {
			t.Errorf("self-peer exclusion must warn, got %v", warns.msgs)
		}
	})

	t.Run("unassigned cloud renders everywhere", func(t *testing.T) {
		for _, cell := range []string{"gpu1", fleetcfg.FrontCell} {
			defs := []*profile.BackendDef{cloudFor("kimi", "")}
			if cell != fleetcfg.FrontCell {
				defs = append(defs, llamaDef("m1", ""))
			} else {
				d := llamaDef("m1", "")
				d.Cell = "gpu1"
				defs = append(defs, d)
			}
			out, err := Render(defs, Options{
				LlamaServerBinary: testBinary,
				Cell:              cell,
				Hosts:             testHosts(t, cellTestHosts),
			})
			if err != nil {
				t.Fatalf("Render(%s): %v", cell, err)
			}
			cfg := parseRendered(t, out)
			if _, ok := cfg.Peers["kimi"]; !ok {
				t.Errorf("unassigned cloud peer must render on %s", cell)
			}
		}
	})
}

func TestRenderCell_FrontRequiresHosts(t *testing.T) {
	def := llamaDef("m1", "")
	def.Cell = "gpu1"
	_, err := Render([]*profile.BackendDef{def}, Options{
		LlamaServerBinary: testBinary,
		Cell:              fleetcfg.FrontCell,
	})
	if err == nil || !strings.Contains(err.Error(), "hosts.yaml") {
		t.Fatalf("front render without hosts.yaml must fail, got %v", err)
	}
}

// TestRenderCell_FrontParity renders --cell front from defs+hosts describing
// a live fleet (2 cells x 3 LLM defs, incl. an alias-winner pair, plus 1
// cloud_peer) and proves the output is semantically the hand-maintained
// front config an operator would write: same peer set, same model+alias
// union per peer, same cloud peers. Ordering and comments may differ.
func TestRenderCell_FrontParity(t *testing.T) {
	dir := t.TempDir()
	defsYAML := map[string]string{
		"qwen3.6-27b": `
name: qwen3.6-27b
cell: gpu1
backend:
  external: true
  llama_server:
    path: ~/models/qwen3.6-27b.gguf
    huggingface: {repo: unsloth/qwen3.6, file: qwen3.6-27b.gguf}
    alias: qwen3.6-27b-mtp-q6_k
    context: 131072
router: {alias_owner: true}
`,
		"qwen3.6-27b-tools": `
name: qwen3.6-27b-tools
cell: gpu1
backend:
  external: true
  llama_server:
    path: ~/models/qwen3.6-27b.gguf
    huggingface: {repo: unsloth/qwen3.6, file: qwen3.6-27b.gguf}
    alias: qwen3.6-27b-mtp-q6_k
    context: 131072
`,
		"gemma-4-31b-mm": `
name: gemma-4-31b-mm
cell: gpu2
backend:
  external: true
  llama_server:
    path: ~/models/gemma-4-31b.gguf
    huggingface: {repo: unsloth/gemma, file: gemma-4-31b.gguf}
    alias: gemma-4-31b-it
    context: 57344
`,
		"anthropic": `
name: anthropic
backend:
  cloud_peer:
    base_url: https://api.anthropic.com
    api_key_env: ANTHROPIC_API_KEY
    models: [claude-opus-4-8, claude-sonnet-5]
`,
	}
	for name, text := range defsYAML {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defs, err := LoadDefs(dir)
	if err != nil {
		t.Fatalf("LoadDefs: %v", err)
	}

	out, err := Render(defs, Options{
		LlamaServerBinary: testBinary,
		Cell:              fleetcfg.FrontCell,
		Hosts:             testHosts(t, cellTestHosts),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The hand-maintained front config an operator would keep for this
	// fleet: one peer per cell carrying that cell's model ids (aliases
	// included, alias-winner resolved), plus the cloud peer.
	const handMaintained = `
healthCheckTimeout: 300
sendLoadingState: true
startPort: 5800
includeAliasesInList: true
peers:
  gpu1:
    proxy: http://gpu1.lan:9000
    models: [qwen3.6-27b, qwen3.6-27b-mtp-q6_k, qwen3.6-27b-tools]
  gpu2:
    proxy: http://gpu2.lan:9000
    models: [gemma-4-31b-mm, gemma-4-31b-it]
  anthropic:
    proxy: https://api.anthropic.com
    apiKey: ${env.ANTHROPIC_API_KEY}
    models: [claude-opus-4-8, claude-sonnet-5]
`
	got := parseRendered(t, out)
	want := parseRendered(t, handMaintained)

	if g, w := slices.Sorted(maps.Keys(got.Peers)), slices.Sorted(maps.Keys(want.Peers)); !equalSlices(g, w) {
		t.Fatalf("peer set = %v, want %v", g, w)
	}
	if got.Models != nil {
		t.Errorf("front render must have no models: stanza, got %v", keys(got.Models))
	}
	for name, wp := range want.Peers {
		gp := got.Peers[name]
		if gp.Proxy != wp.Proxy {
			t.Errorf("peer %s proxy = %q, want %q", name, gp.Proxy, wp.Proxy)
		}
		if gp.APIKey != wp.APIKey {
			t.Errorf("peer %s apiKey = %q, want %q", name, gp.APIKey, wp.APIKey)
		}
		if !equalSlices(gp.Models, wp.Models) {
			t.Errorf("peer %s models = %v, want %v", name, gp.Models, wp.Models)
		}
	}
	if got.HealthCheckTimeout != want.HealthCheckTimeout ||
		got.SendLoadingState != want.SendLoadingState ||
		got.StartPort != want.StartPort ||
		got.IncludeAliasesInList != want.IncludeAliasesInList {
		t.Errorf("top-level fields drifted from the hand-maintained config: got %+v want %+v", got, want)
	}
}

// TestRenderCell_FrontCarriesNoCapabilities pins the reason the front render
// stays capability-free. llama-swap's peer stanza is a bare list of model ids,
// and its /v1/models builds every peer record with an empty capability config,
// so a block emitted here would be silently dropped rather than reach a
// front-addressed client. Clients that talk to the front need their own
// per-id corrections until llama-swap propagates a peer's catalog.
func TestRenderCell_FrontCarriesNoCapabilities(t *testing.T) {
	a := llamaDef("qwen-a", "qwen")
	a.Cell = "gpu1"
	vision := llamaDef("gemma-mm", "gemma-it")
	vision.Cell = "gpu2"
	vision.Backend.LlamaServer.MMProj = "/models/mmproj.gguf"

	out, err := Render([]*profile.BackendDef{a, vision}, Options{
		LlamaServerBinary: testBinary,
		Cell:              fleetcfg.FrontCell,
		Hosts:             testHosts(t, cellTestHosts),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "capabilities:") {
		t.Errorf("front render emitted capabilities, which peers cannot carry:\n%s", out)
	}

	// The same defs on their own cell must still declare them, so the
	// exclusion is the front's shape and not a lost derivation.
	out, err = Render([]*profile.BackendDef{vision}, Options{
		LlamaServerBinary: testBinary,
		Cell:              "gpu2",
		Hosts:             testHosts(t, cellTestHosts),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "capabilities:") || !strings.Contains(out, "image") {
		t.Errorf("cell render dropped the vision capability:\n%s", out)
	}
}
