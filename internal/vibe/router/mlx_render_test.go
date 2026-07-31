package router

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

func mlxDef(name string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Backend: profile.Backend{
			External: true,
			MLXServer: &profile.MLXServerBackend{
				ModelDir:  "/Users/me/models/qwen-mlx-4bit",
				Venv:      "/Users/me/.venvs/mlx",
				Alias:     "qwen-mlx",
				Context:   65536,
				Host:      "127.0.0.1",
				MaxTokens: 32768,
			},
		},
	}
}

// mlxParsed extends parsedConfig with the field only mlx tenants emit.
type mlxParsed struct {
	Models map[string]struct {
		Cmd          string   `yaml:"cmd"`
		Aliases      []string `yaml:"aliases"`
		UseModelName string   `yaml:"useModelName"`
	} `yaml:"models"`
}

func TestRender_MLXTenant(t *testing.T) {
	out, err := Render([]*profile.BackendDef{mlxDef("qwen36-mlx")}, Options{LlamaServerBinary: testBinary})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg mlxParsed
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered output is not YAML: %v\n%s", err, out)
	}
	m, ok := cfg.Models["qwen36-mlx"]
	if !ok {
		t.Fatalf("def name should be the model key:\n%s", out)
	}
	// useModelName is the whole reason mlx can ride llama-swap: clients say
	// "qwen36-mlx", the process only answers to its snapshot path.
	if m.UseModelName != "/Users/me/models/qwen-mlx-4bit" {
		t.Errorf("useModelName = %q, want the snapshot path", m.UseModelName)
	}
	// The alias field still becomes a client-facing alias, same as
	// llama_server defs.
	if len(m.Aliases) != 1 || m.Aliases[0] != "qwen-mlx" {
		t.Errorf("aliases = %v, want [qwen-mlx]", m.Aliases)
	}
	for _, want := range []string{"mlx_lm.server", "--model /Users/me/models/qwen-mlx-4bit", "--port ${PORT}", "--max-tokens 32768"} {
		if !strings.Contains(m.Cmd, want) {
			t.Errorf("cmd missing %q:\n%s", want, m.Cmd)
		}
	}
	// llama-server flags must not leak into an mlx cmd.
	if strings.Contains(m.Cmd, "llama-server") {
		t.Errorf("mlx cmd should not reference llama-server:\n%s", m.Cmd)
	}
}

// The dual-mode contract: the argv the router tells llama-swap to run and
// the argv the vibe daemon would run for the same def differ only in the
// port, so a model behaves the same connected (hum spawns it) and
// disconnected (vibe launches it locally).
func TestRender_MLXCmdMatchesDaemonSpec(t *testing.T) {
	def := mlxDef("qwen36-mlx")
	out, err := Render([]*profile.BackendDef{def}, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg mlxParsed
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	rendered := cfg.Models["qwen36-mlx"].Cmd

	p := &profile.Profile{Name: def.Name, Backend: def.Backend}
	spec, err := profile.MLXServerSpec(p, 8080)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range spec.Args {
		if a == "8080" {
			spec.Args[i] = "${PORT}"
		}
	}
	// The rendered cmd is one flag per line; flatten before comparing.
	flat := strings.Join(strings.Fields(rendered), " ")
	want := spec.Binary + " " + strings.Join(spec.Args, " ")
	if flat != want {
		t.Errorf("router cmd and daemon spec drifted:\n router: %s\n daemon: %s", flat, want)
	}
}
