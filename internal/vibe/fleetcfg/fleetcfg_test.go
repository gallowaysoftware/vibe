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
