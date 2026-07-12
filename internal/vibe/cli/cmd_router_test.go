package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
