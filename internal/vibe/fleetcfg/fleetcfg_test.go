package fleetcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeHosts(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_MissingFileIsNil(t *testing.T) {
	f, err := LoadFrom(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || f != nil {
		t.Fatalf("missing file: got (%v, %v), want (nil, nil)", f, err)
	}
}

func TestLoad_FullFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := writeHosts(t, `
fleetd_url: "http://front.lan:9001"
model_classes:
  bge-embed: embed
cells:
  front:    { url: "http://front.lan:9000", class: always_on }
  gpu-cell: { url: "http://gpu.lan:9000", class: opportunistic,
              host_probe: "gpu.lan:22",
              daemon_url: "http://gpu.lan:9001",
              token_file: "~/.config/vibe/tokens/gpu-cell" }
  laptop:   { url: "http://laptop.lan:9000", class: roaming }
`)
	f, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if !f.HasCells() {
		t.Fatal("HasCells false, want true")
	}
	if f.FleetdURL != "http://front.lan:9001" {
		t.Errorf("FleetdURL = %q", f.FleetdURL)
	}
	gpu := f.Cells["gpu-cell"]
	if gpu.Class != ClassOpportunistic || gpu.HostProbe != "gpu.lan:22" || gpu.DaemonURL != "http://gpu.lan:9001" {
		t.Errorf("gpu-cell parsed wrong: %+v", gpu)
	}
	wantTok := filepath.Join(home, ".config", "vibe", "tokens", "gpu-cell")
	if gpu.TokenFile != wantTok {
		t.Errorf("TokenFile = %q, want tilde-expanded %q", gpu.TokenFile, wantTok)
	}
	if f.ModelClasses["bge-embed"] != "embed" {
		t.Errorf("ModelClasses = %v", f.ModelClasses)
	}
}

func TestLoad_FrontRequired(t *testing.T) {
	p := writeHosts(t, `
cells:
  gpu-cell: { url: "http://gpu.lan:9000", class: opportunistic }
`)
	_, err := LoadFrom(p)
	if err == nil || !strings.Contains(err.Error(), `"front"`) {
		t.Fatalf("want front-required error, got %v", err)
	}
}

func TestLoad_CellValidation(t *testing.T) {
	cases := map[string]string{
		"missing url": `
cells:
  front: { class: always_on }
`,
		"bad class": `
cells:
  front: { url: "http://front.lan:9000", class: sometimes }
`,
		"empty model class": `
cells:
  front: { url: "http://front.lan:9000", class: always_on }
model_classes:
  bge-embed: ""
`,
		"wake without mac or cmd": `
cells:
  front: { url: "http://front.lan:9000", class: always_on, wake: {broadcast: "192.0.2.255:9"} }
`,
		"eui-64 mac rejected": `
cells:
  front: { url: "http://front.lan:9000", class: always_on, wake: {mac: "aa:bb:cc:dd:ee:ff:00:11"} }
`,
	}
	for name, doc := range cases {
		if _, err := LoadFrom(writeHosts(t, doc)); err == nil {
			t.Errorf("%s: want validation error, got nil", name)
		}
	}
}

func TestLoad_WakeValidation(t *testing.T) {
	// cmd-only wake is valid (the fallback path needs no MAC).
	p := writeHosts(t, `
cells:
  front: { url: "http://front.lan:9000", class: always_on, wake: {cmd: "ssh otherbox wol aa:bb"} }
`)
	if _, err := LoadFrom(p); err != nil {
		t.Fatalf("cmd-only wake must load: %v", err)
	}
	// 48-bit MAC alone is valid.
	p = writeHosts(t, `
cells:
  front: { url: "http://front.lan:9000", class: always_on, wake: {mac: "aa:bb:cc:dd:ee:ff"} }
`)
	if _, err := LoadFrom(p); err != nil {
		t.Fatalf("48-bit wake must load: %v", err)
	}
}

func TestLoad_NoCellsSectionIsValid(t *testing.T) {
	p := writeHosts(t, `fleetd_url: "http://front.lan:9001"`)
	f, err := LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.HasCells() {
		t.Fatal("HasCells true for fleetd_url-only file")
	}
}

func TestLoad_EmptyOrCommentOnlyIsNotAnError(t *testing.T) {
	// An empty or comment-only hosts.yaml means "no fleet config", not
	// a parse failure — the strict decoder's io.EOF must not leak out.
	for name, doc := range map[string]string{
		"empty":        "",
		"comment only": "# nothing here yet\n",
		"whitespace":   "\n\n",
	} {
		f, err := LoadFrom(writeHosts(t, doc))
		if err != nil {
			t.Fatalf("%s: got %v, want nil error", name, err)
		}
		if f.HasCells() {
			t.Fatalf("%s: HasCells true", name)
		}
	}
}

// TestLoad_ToleratesHostInventory pins C6/MIN-R: fleet.md §4.1's
// top-level `hosts:` inventory shares this filename, and KnownFields
// (which must STAY on) would otherwise abort fleetd's startup on a file
// that is perfectly valid for the other reader.
func TestLoad_ToleratesHostInventory(t *testing.T) {
	p := writeHosts(t, `
hosts:
  localmodel:
    local: true
    gpu: {kind: cuda, vram_gb: 32}
  spark-1:
    addr: 10.0.40.11
    ssh: {user: kyle, key: ~/.ssh/id_ed25519_fleet}
cells:
  front: { url: "http://front.lan:9000", class: always_on }
`)
	f, err := LoadFrom(p)
	if err != nil {
		t.Fatalf("a hosts.yaml carrying both schemas must load: %v", err)
	}
	if !f.HasCells() {
		t.Error("cells section lost")
	}
	// Still strict about the keys it DOES own (a mistyped cell field).
	if _, err := LoadFrom(writeHosts(t, `
cells:
  front: { url: "http://front.lan:9000", host_prob: "front.lan:22", class: always_on }
`)); err == nil {
		t.Error("a typo'd cell key must still fail: KnownFields(true) is what keeps display semantics honest")
	}
}

// TestLoad_ModelClassVocabulary pins C6/MIN-Q's config half: warm_model's
// guard keys on the class string, so a typo'd class would silently stop
// gating an embed id.
func TestLoad_ModelClassVocabulary(t *testing.T) {
	for _, class := range ModelClasses {
		p := writeHosts(t, `
cells:
  front: { url: "http://front.lan:9000", class: always_on }
model_classes:
  some-model: `+class+`
`)
		if _, err := LoadFrom(p); err != nil {
			t.Errorf("class %q must be accepted: %v", class, err)
		}
	}
	p := writeHosts(t, `
cells:
  front: { url: "http://front.lan:9000", class: always_on }
model_classes:
  bge-embed: embeddings
`)
	_, err := LoadFrom(p)
	if err == nil || !strings.Contains(err.Error(), "embeddings") {
		t.Errorf("typo'd class: got %v, want a validation error naming it", err)
	}
	if KnownModelClass("embeddings") {
		t.Error("KnownModelClass accepted a value outside the vocabulary")
	}
}

// ─── C7b: pricing, power and capital ────────────────────────────────────────

const c7bCells = `
cells:
  front: {url: "http://front:9000", class: always_on}
  gpu:
    url: "http://gpu:9000"
    class: opportunistic
`

func TestLoad_PricingBlock(t *testing.T) {
	f, err := LoadFrom(writeHosts(t, c7bCells+`
pricing:
  electricity_price_per_kwh: 0.15
  models:
    qwen-coder:
      twin: "Acme/Acme-Coder-30B"
      counterfactual: batch
    cloud-chat:
      priced_as: "vendor-chat-1"
  frontier:
    model: "vendor-frontier-1"
    rationale: "example: the agentic refactors I would actually have paid for"
`))
	if err != nil {
		t.Fatal(err)
	}
	mp, ok := f.ModelPricingFor("qwen-coder")
	if !ok || mp.Twin != "Acme/Acme-Coder-30B" {
		t.Fatalf("model pricing = %+v (ok=%v)", mp, ok)
	}
	if mp.Multiplier() != 0.5 {
		t.Errorf("batch multiplier = %g, want 0.5 (Anthropic's Batch API is a flat 50%%)", mp.Multiplier())
	}
	if def, _ := f.ModelPricingFor("cloud-chat"); def.Multiplier() != 1 {
		t.Errorf("unset counterfactual multiplier = %g, want 1 (interactive)", def.Multiplier())
	}
	if f.Pricing.Frontier == nil || f.Pricing.Frontier.Rationale == "" {
		t.Error("frontier rationale lost on load")
	}
}

// TestLoad_FrontierRequiresARationale: the frontier row is a claim about
// work you would actually have paid a frontier model to do. Without a
// written rationale it is an unarguable number, which is the one thing
// this screen must never render.
func TestLoad_FrontierRequiresARationale(t *testing.T) {
	_, err := LoadFrom(writeHosts(t, c7bCells+`
pricing:
  frontier:
    model: "vendor-frontier-1"
`))
	if err == nil || !strings.Contains(err.Error(), "rationale") {
		t.Fatalf("err = %v, want a rationale requirement", err)
	}
	if _, err := LoadFrom(writeHosts(t, c7bCells+"pricing:\n  frontier:\n    rationale: \"because\"\n")); err == nil {
		t.Error("accepted a frontier with a rationale and no model")
	}
}

func TestLoad_CapitalCostRequiresANote(t *testing.T) {
	_, err := LoadFrom(writeHosts(t, `
cells:
  front: {url: "http://front:9000", class: always_on}
  gpu: {url: "http://gpu:9000", class: opportunistic, capital_cost: 3100}
`))
	if err == nil || !strings.Contains(err.Error(), "capital_note") {
		t.Fatalf("err = %v, want a capital_note requirement", err)
	}
	// And a note with no number is equally wrong: no capital number means
	// no payback bar at all.
	_, err = LoadFrom(writeHosts(t, `
cells:
  front: {url: "http://front:9000", class: always_on}
  gpu: {url: "http://gpu:9000", class: opportunistic, capital_note: "a GPU"}
`))
	if err == nil {
		t.Error("accepted a capital_note with no capital_cost")
	}
}

// TestLoad_PowerSourceOnlyDeclared: nvidia_smi and ha_entity are named
// future values, not built ones. A config that asks for a sampler must
// fail loudly rather than silently reporting declared numbers.
func TestLoad_PowerSourceOnlyDeclared(t *testing.T) {
	ok := writeHosts(t, `
cells:
  front: {url: "http://front:9000", class: always_on}
  gpu:
    url: "http://gpu:9000"
    class: opportunistic
    power: {source: declared, watts_idle: 100, watts_busy: 400}
`)
	f, err := LoadFrom(ok)
	if err != nil {
		t.Fatal(err)
	}
	if p := f.Cells["gpu"].Power; p == nil || p.WattsIdle != 100 || p.WattsBusy != 400 {
		t.Fatalf("power = %+v", p)
	}
	for _, bad := range []string{
		`power: {source: nvidia_smi, watts_idle: 100}`,
		`power: {source: ha_entity, watts_idle: 100}`,
		`power: {source: guess, watts_idle: 100}`,
		`power: {watts_idle: 100}`,
		`power: {source: declared}`,
		`power: {source: declared, watts_idle: 400, watts_busy: 100}`,
	} {
		doc := "cells:\n  front: {url: \"http://front:9000\", class: always_on}\n  gpu: {url: \"http://gpu:9000\", class: opportunistic, " + bad + "}\n"
		if _, err := LoadFrom(writeHosts(t, doc)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestLoad_PricingRejectsNonsense(t *testing.T) {
	for _, bad := range []string{
		"pricing:\n  electricity_price_per_kwh: -1\n",
		"pricing:\n  models:\n    m: {twin: \"t\", counterfactual: sometimes}\n",
		"pricing:\n  models:\n    m: {counterfactual: batch}\n",
	} {
		if _, err := LoadFrom(writeHosts(t, c7bCells+bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
