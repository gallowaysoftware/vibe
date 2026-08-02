package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

func writeRouterFixture(t *testing.T, dir, name, yaml string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

const routerFixtureDef = `name: m1
backend:
  external: true
  llama_server:
    path: ~/models/m1.gguf
    huggingface: {repo: r/x, file: m1.gguf}
    alias: m1-alias
    context: 4096
`

func TestRunRouterRender(t *testing.T) {
	tmp := t.TempDir()
	backends := filepath.Join(tmp, "backends")
	cfgPath := filepath.Join(tmp, "llama-swap", "config.yaml")
	writeRouterFixture(t, backends, "m1", routerFixtureDef)
	opts := router.Options{LlamaServerBinary: "/opt/llama-server"}

	t.Run("stdout prints without writing", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runRouterRender(&buf, backends, cfgPath, opts, false, true); err != nil {
			t.Fatalf("stdout render: %v", err)
		}
		if !strings.Contains(buf.String(), "models:") || !strings.Contains(buf.String(), "m1:") {
			t.Errorf("stdout output missing rendered config:\n%s", buf.String())
		}
		if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
			t.Error("--stdout must not write the config file")
		}
	})

	t.Run("first render writes and hints at restart", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runRouterRender(&buf, backends, cfgPath, opts, false, false); err != nil {
			t.Fatalf("render: %v", err)
		}
		if _, err := os.Stat(cfgPath); err != nil {
			t.Fatalf("config not written: %v", err)
		}
		if !strings.Contains(buf.String(), "systemctl --user restart llama-swap") {
			t.Errorf("changed render must print the restart hint:\n%s", buf.String())
		}
	})

	t.Run("no drift", func(t *testing.T) {
		var buf bytes.Buffer
		if err := runRouterRender(&buf, backends, cfgPath, opts, true, false); err != nil {
			t.Fatalf("check with no drift must pass: %v", err)
		}
		if !strings.Contains(buf.String(), "up to date") {
			t.Errorf("output = %q", buf.String())
		}
	})

	t.Run("check reports drift without writing", func(t *testing.T) {
		before, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		writeRouterFixture(t, backends, "m1", strings.Replace(routerFixtureDef, "4096", "8192", 1))
		var buf bytes.Buffer
		err = runRouterRender(&buf, backends, cfgPath, opts, true, false)
		if err == nil {
			t.Fatal("check must exit non-zero on drift")
		}
		if !strings.Contains(buf.String(), "-") || !strings.Contains(buf.String(), "8192") {
			t.Errorf("check output should carry the diff:\n%s", buf.String())
		}
		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("--check must not modify the config file")
		}

		// A plain render then converges the file.
		buf.Reset()
		if err := runRouterRender(&buf, backends, cfgPath, opts, false, false); err != nil {
			t.Fatalf("converging render: %v", err)
		}
		if err := runRouterRender(&buf, backends, cfgPath, opts, true, false); err != nil {
			t.Fatalf("post-converge check: %v", err)
		}
	})
}

// setupRenderXDG points the whole config home at a tmp dir and optionally
// writes a daemon config with fleet.cell set, so planRender's fleet
// resolution runs against fixtures instead of the developer's box.
func setupRenderXDG(t *testing.T, daemonConfig string) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if daemonConfig != "" {
		dir := filepath.Join(xdg, "vibe")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(daemonConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return xdg
}

func TestPlanRender_NonLocalCellRequiresOut(t *testing.T) {
	setupRenderXDG(t, "fleet: {cell: gpu1}\n")

	// Another cell with no --out/--stdout would overwrite this box's
	// llama-swap config with a foreign render.
	if _, _, _, err := planRender("gpu2", "", false); err == nil ||
		!strings.Contains(err.Error(), "gpu2") || !strings.Contains(err.Error(), "--out") {
		t.Errorf("non-local --cell must be refused without --out/--stdout, got %v", err)
	}
	// --out or --stdout discharges the safety rule.
	if _, _, _, err := planRender("gpu2", filepath.Join(t.TempDir(), "front.yaml"), false); err != nil {
		t.Errorf("--out must allow a non-local render: %v", err)
	}
	if _, _, _, err := planRender("gpu2", "", true); err != nil {
		t.Errorf("--stdout must allow a non-local render: %v", err)
	}
	// The box's own cell may write the default path.
	target, _, cfgPath, err := planRender("gpu1", "", false)
	if err != nil {
		t.Fatalf("local --cell must be allowed: %v", err)
	}
	if target != "gpu1" || cfgPath != llamaSwapConfigPath() {
		t.Errorf("local render = (%q, %q), want (gpu1, %q)", target, cfgPath, llamaSwapConfigPath())
	}

	// Same refusal when the box has no fleet identity at all.
	setupRenderXDG(t, "")
	if _, _, _, err := planRender("gpu2", "", false); err == nil {
		t.Error("--cell on a no-fleet box must still require --out/--stdout")
	}
}

func TestPlanRender_FleetCellIsDefaultTarget(t *testing.T) {
	setupRenderXDG(t, "fleet: {cell: gpu1}\n")
	target, _, _, err := planRender("", "", false)
	if err != nil {
		t.Fatalf("planRender: %v", err)
	}
	if target != "gpu1" {
		t.Errorf("no --cell must default to the daemon config's fleet.cell, got %q", target)
	}

	// No fleet.cell: the render has no fleet identity (cell-carrying defs
	// are excluded with a warning at render time, not here).
	setupRenderXDG(t, "")
	target, _, _, err = planRender("", "", false)
	if err != nil {
		t.Fatalf("planRender: %v", err)
	}
	if target != "" {
		t.Errorf("no fleet config must yield an empty target cell, got %q", target)
	}
}

func TestRunRouterRender_FrontCell(t *testing.T) {
	tmp := t.TempDir()
	backends := filepath.Join(tmp, "backends")
	writeRouterFixture(t, backends, "m1", routerFixtureDef)
	hosts, err := fleetcfg.LoadFrom(writeHostsFixture(t, tmp))
	if err != nil {
		t.Fatal(err)
	}

	// m1 is unassigned: the front render excludes it (with a warning) and
	// emits no models: stanza — the front owns no models.
	var warns []string
	opts := router.Options{
		LlamaServerBinary: "/opt/llama-server",
		Cell:              "front",
		Hosts:             hosts,
		Warnf:             func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) },
	}
	var buf bytes.Buffer
	if err := runRouterRender(&buf, backends, filepath.Join(tmp, "unused.yaml"), opts, false, true); err != nil {
		t.Fatalf("front render: %v", err)
	}
	if strings.Contains(buf.String(), "models:") {
		t.Errorf("front render must be peers-only:\n%s", buf.String())
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "m1") {
		t.Errorf("unassigned def must produce one warning naming it, got %v", warns)
	}
}

func writeHostsFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(path, []byte("cells:\n  front: {url: \"http://front.lan:9000\", class: always_on}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
