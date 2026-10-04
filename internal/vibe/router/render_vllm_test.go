// The vllm backend kind: an externally managed engine (recipe launcher,
// systemd, docker) declared so the render can derive its llama-swap
// entries. The cell render emits a PEER stanza (a models: entry needs a
// cmd, and no cmd exists to build — llama-swap must never JIT-start a
// TP=2 boot); the front render joins the def into its cell's peer model
// list like any other cell-assigned def.
package router

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

func vllmDef(name, cell string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Cell: cell,
		Backend: profile.Backend{
			External: true, // normalize() forces this on a loaded def; inline here
			VLLM:     &profile.VLLMBackend{URL: "http://127.0.0.1:8888"},
		},
	}
}

func vllmLlamaDef(name, cell string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Cell: cell,
		Backend: profile.Backend{
			External:    true,
			LlamaServer: &profile.LlamaServerBackend{Path: "/models/" + name + ".gguf"},
		},
	}
}

// vllmHosts is the minimal membership the renders below validate against.
func vllmHosts(t *testing.T) *fleetcfg.File {
	return testHosts(t, `
cells:
  front: {url: "http://front.lan:9000", class: always_on}
  heavy: {url: "http://heavy.lan:9000", class: always_on, timeouts: {responseHeader: 600}}
  gpu:   {url: "http://gpu.lan:9000", class: always_on}
`)
}

type vllmCfg struct {
	Peers map[string]struct {
		Proxy    string   `yaml:"proxy"`
		Models   []string `yaml:"models"`
		APIKey   string   `yaml:"apiKey"`
		Timeouts struct {
			ResponseHeader int `yaml:"responseHeader"`
		} `yaml:"timeouts"`
	} `yaml:"peers"`
	Models map[string]struct {
		Cmd   string `yaml:"cmd"`
		Proxy string `yaml:"proxy"`
	} `yaml:"models"`
}

func parseVllmCfg(t *testing.T, out string) vllmCfg {
	t.Helper()
	var c vllmCfg
	if err := yaml.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("rendered config is not YAML: %v\n%s", err, out)
	}
	return c
}

// TestRender_VllmCellRenderIsAPeerStanza: on the engine's own cell the def
// becomes a peers: entry pointing at the engine URL — never a models:
// entry, which would hand llama-swap a cmd it could JIT-start.
func TestRender_VllmCellRenderIsAPeerStanza(t *testing.T) {
	out, err := Render([]*profile.BackendDef{vllmDef("glm53flash", "heavy")}, Options{Cell: "heavy", Hosts: vllmHosts(t)})
	if err != nil {
		t.Fatal(err)
	}
	c := parseVllmCfg(t, out)
	p, ok := c.Peers["glm53flash"]
	if !ok {
		t.Fatalf("no glm53flash peer stanza in the cell render:\n%s", out)
	}
	if p.Proxy != "http://127.0.0.1:8888" {
		t.Errorf("peer proxy = %q, want the engine URL", p.Proxy)
	}
	if len(p.Models) != 1 || p.Models[0] != "glm53flash" {
		t.Errorf("peer models = %v, want [glm53flash] — the served-model name IS the def name", p.Models)
	}
	if len(c.Models) != 0 {
		t.Errorf("a vllm def must never render a models: entry, got %v", c.Models)
	}
}

// TestRender_VllmCellRenderCarriesKeyAndTimeouts: api_key_env renders as a
// ${env.*} reference (never a literal) and the timeouts block rides the
// stanza, because llama-swap's peer responseHeader default (60s) 502s a
// large-context prefill one hop from the engine.
func TestRender_VllmCellRenderCarriesKeyAndTimeouts(t *testing.T) {
	d := vllmDef("glm53flash", "heavy")
	d.Backend.VLLM.APIKeyEnv = "HEAVY_KEY"
	d.Backend.VLLM.Timeouts = &fleetcfg.Timeouts{ResponseHeader: 600}
	out, err := Render([]*profile.BackendDef{d}, Options{Cell: "heavy", Hosts: vllmHosts(t)})
	if err != nil {
		t.Fatal(err)
	}
	c := parseVllmCfg(t, out)
	p := c.Peers["glm53flash"]
	if p.APIKey != "${env.HEAVY_KEY}" {
		t.Errorf("apiKey = %q, want the ${env.*} reference", p.APIKey)
	}
	if p.Timeouts.ResponseHeader != 600 {
		t.Errorf("timeouts.responseHeader = %d, want 600", p.Timeouts.ResponseHeader)
	}
	if strings.Count(out, "HEAVY_KEY") != 1 {
		t.Errorf("the key must appear exactly once, as the env reference:\n%s", out)
	}
}

// TestRender_VllmJoinsTheCellPeerOnTheFront: the front render treats a
// vllm def like any cell-assigned def — its id lands in the cell's peer
// model list, with the peer proxy from hosts.yaml (the front peers to the
// cell's llama-swap, never to the engine).
func TestRender_VllmJoinsTheCellPeerOnTheFront(t *testing.T) {
	defs := []*profile.BackendDef{
		vllmDef("glm53flash", "heavy"),
		vllmLlamaDef("gpu-coder", "gpu"),
	}
	out, err := Render(defs, Options{Cell: "front", Hosts: vllmHosts(t)})
	if err != nil {
		t.Fatal(err)
	}
	c := parseVllmCfg(t, out)
	if models := c.Peers["heavy"].Models; len(models) != 1 || models[0] != "glm53flash" {
		t.Errorf("heavy peer models = %v, want [glm53flash]:\n%s", models, out)
	}
	if models := c.Peers["gpu"].Models; len(models) != 1 || models[0] != "gpu-coder" {
		t.Errorf("gpu peer models = %v, want [gpu-coder]:\n%s", models, out)
	}
	if proxy := c.Peers["heavy"].Proxy; proxy != "http://heavy.lan:9000" {
		t.Errorf("heavy peer proxy = %q, want the cell URL from hosts.yaml", proxy)
	}
}

// TestRender_VllmOtherCellExcludedWithWarning: a vllm def pinned to
// another cell is excluded loudly from this cell's render, exactly like
// every other cell-assigned def.
func TestRender_VllmOtherCellExcludedWithWarning(t *testing.T) {
	var w warnCapture
	out, err := Render([]*profile.BackendDef{vllmDef("glm53flash", "heavy")},
		Options{Cell: "gpu", Hosts: vllmHosts(t), Warnf: w.warnf})
	if err != nil {
		t.Fatal(err)
	}
	if !w.contains("glm53flash", "heavy") {
		t.Errorf("the exclusion must name the def and its cell, got %v", w.msgs)
	}
	if strings.Contains(out, "glm53flash") {
		t.Errorf("another cell's vllm def rendered anyway:\n%s", out)
	}
}

// TestRender_VllmUnknownCellIsAnError: a def whose cell is not in
// hosts.yaml is a typo or stale membership — the model would silently
// strand, so it is a render error in every mode.
func TestRender_VllmUnknownCellIsAnError(t *testing.T) {
	if _, err := Render([]*profile.BackendDef{vllmDef("glm53flash", "nope")}, Options{Cell: "front", Hosts: vllmHosts(t)}); err == nil {
		t.Fatal("rendered a vllm def whose cell is not in hosts.yaml")
	}
}

// TestRender_VllmFrontSelfAssignExcluded: a vllm def assigned to the front
// cell would self-peer (a proxy loop).
func TestRender_VllmFrontSelfAssignExcluded(t *testing.T) {
	var w warnCapture
	out, err := Render([]*profile.BackendDef{vllmDef("glm53flash", "front")},
		Options{Cell: "front", Hosts: vllmHosts(t), Warnf: w.warnf})
	if err != nil {
		t.Fatal(err)
	}
	if !w.contains("glm53flash", "front owns no models") {
		t.Errorf("the self-assign exclusion must be loud, got %v", w.msgs)
	}
	if strings.Contains(out, "glm53flash") {
		t.Errorf("a front-assigned vllm def rendered a self-peer:\n%s", out)
	}
}

// TestRender_VllmPeerNameCollisionIsAnError: a cloud_peer def named after
// a vllm def (or vice versa) would render two peer stanzas under one key —
// llama-swap serves whichever won, which is the lying-catalog shape.
func TestRender_VllmPeerNameCollisionIsAnError(t *testing.T) {
	cp := &profile.BackendDef{
		Name: "glm53flash",
		Backend: profile.Backend{External: true, CloudPeer: &profile.CloudPeerBackend{
			BaseURL: "http://127.0.0.1:8888", Models: []string{"glm53flash"},
		}},
	}
	if _, err := Render([]*profile.BackendDef{vllmDef("glm53flash", "heavy"), cp},
		Options{Cell: "heavy", Hosts: vllmHosts(t)}); err == nil {
		t.Fatal("two peer stanzas under one name rendered without an error")
	}
}

// TestRender_VllmIdIsReservedAgainstAliases: a llama_server def whose
// ALIAS equals a vllm def's name would advertise one client-facing id from
// two entries — and the engine validates the model field, so the alias
// would 404 upstream. The id is canonical, like a peer's catalog ids.
func TestRender_VllmIdIsReservedAgainstAliases(t *testing.T) {
	d := vllmLlamaDef("qwen-coder", "gpu")
	d.Backend.LlamaServer.Alias = "glm53flash"
	if _, err := Render([]*profile.BackendDef{vllmDef("glm53flash", "heavy"), d},
		Options{Cell: "front", Hosts: vllmHosts(t)}); err == nil {
		t.Fatal("an alias equal to a vllm def's id rendered without an error")
	}
}

// TestRender_VllmTrialExcludedFromFront: a trial def is evaluated on ONE
// cell and never becomes fleet catalog, whatever kind it is.
func TestRender_VllmTrialExcludedFromFront(t *testing.T) {
	d := vllmDef("candidate", "heavy")
	d.Trial = true
	var w warnCapture
	out, err := Render([]*profile.BackendDef{d}, Options{Cell: "front", Hosts: vllmHosts(t), Warnf: w.warnf})
	if err != nil {
		t.Fatal(err)
	}
	if !w.contains("candidate", "trial") {
		t.Errorf("the trial exclusion must name the def, got %v", w.msgs)
	}
	if strings.Contains(out, "candidate") {
		t.Errorf("a trial vllm def reached the front render:\n%s", out)
	}
}
