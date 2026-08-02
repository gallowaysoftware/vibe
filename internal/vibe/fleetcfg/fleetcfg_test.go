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
	}
	for name, doc := range cases {
		if _, err := LoadFrom(writeHosts(t, doc)); err == nil {
			t.Errorf("%s: want validation error, got nil", name)
		}
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
