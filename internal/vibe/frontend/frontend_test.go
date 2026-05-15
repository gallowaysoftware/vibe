package frontend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

func TestActivate_External_WritesExpandedJSON(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "opencode.json")
	p := &profile.Profile{
		Name: "code",
		Backend: profile.Backend{LlamaServer: &profile.LlamaServerBackend{
			Path:     "/m.gguf",
			Alias:    "qwen3",
			Context:  8192,
			Parallel: 1,
		}},
		Frontend: profile.Frontend{
			Kind:            profile.FrontendExternal,
			App:             "opencode",
			RestartRequired: true,
			WriteFile:       target,
			Template: map[string]any{
				"provider": map[string]any{
					"vibe-local": map[string]any{
						"options": map[string]any{
							"baseURL": "${VIBE_API}",
						},
						"models": map[string]any{
							"${MODEL_ALIAS}": map[string]any{
								"limit": map[string]any{"context": "${MODEL_CONTEXT}"},
							},
						},
					},
				},
				"model": "vibe-local/${MODEL_ALIAS}",
			},
		},
	}
	r, err := Activate(p, profile.ExpandContext{
		VibeAPI:      "http://127.0.0.1:9000/v1",
		ModelAlias:   "qwen3",
		ModelContext: 8192,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.WroteFile != target {
		t.Errorf("WroteFile = %q", r.WroteFile)
	}
	if !r.RestartRequired {
		t.Errorf("RestartRequired = false")
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "vibe-local/qwen3" {
		t.Errorf("model = %v", got["model"])
	}
	prov := got["provider"].(map[string]any)["vibe-local"].(map[string]any)
	if prov["options"].(map[string]any)["baseURL"] != "http://127.0.0.1:9000/v1" {
		t.Errorf("baseURL = %v", prov["options"].(map[string]any)["baseURL"])
	}
	limit := prov["models"].(map[string]any)["qwen3"].(map[string]any)["limit"].(map[string]any)
	if v, ok := limit["context"].(float64); !ok || v != 8192 {
		t.Errorf("limit.context = %v (%T)", limit["context"], limit["context"])
	}
}

func TestActivate_UnknownKind(t *testing.T) {
	// Pass a literal kind the dispatch doesn't know about. (Validation
	// would reject this at Load() time; we exercise the Activate dispatch
	// here directly.)
	p := &profile.Profile{Frontend: profile.Frontend{Kind: "weird"}}
	if _, err := Activate(p, profile.ExpandContext{}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// docker-compose + write_file is rejected at profile.Validate() time. Confirm
// the validation path here so a future refactor doesn't silently lose it.
func TestValidate_DockerCompose_RejectsWriteFile(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(modelPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &profile.Profile{
		Name:    "p",
		Backend: profile.Backend{LlamaServer: &profile.LlamaServerBackend{Path: modelPath, Alias: "x", Context: 1, Parallel: 1}},
		Frontend: profile.Frontend{
			Kind:        profile.FrontendDockerCompose,
			App:         "x",
			ComposeFile: "/tmp/dc.yaml",
			WriteFile:   "/tmp/x.json",
		},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "write_file") {
		t.Errorf("err = %v, want mention of write_file", err)
	}
}

// seedMCPDir points XDG_CONFIG_HOME at a fresh temp dir, creates the
// vibe/mcp subdir, and writes the supplied name->yaml-body pairs.
func seedMCPDir(t *testing.T, defs map[string]string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	mcpDir := filepath.Join(xdg, "vibe", "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range defs {
		if err := os.WriteFile(filepath.Join(mcpDir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActivate_External_InjectsMCPBlock(t *testing.T) {
	seedMCPDir(t, map[string]string{
		"datadog": `type: local
command: [npx, -y, "@us-all/datadog-mcp"]
environment:
  DD_API_KEY: ${env:DD_API_KEY}
`,
		"jira": `type: local
command: [npx, -y, "@some/jira-mcp"]
`,
	})

	target := filepath.Join(t.TempDir(), "opencode.json")
	p := &profile.Profile{
		Name: "code",
		Backend: profile.Backend{LlamaServer: &profile.LlamaServerBackend{
			Path:     "/m.gguf",
			Alias:    "qwen3",
			Context:  8192,
			Parallel: 1,
		}},
		Frontend: profile.Frontend{
			Kind:      profile.FrontendExternal,
			App:       "opencode",
			WriteFile: target,
			MCPs:      []string{"datadog", "jira"},
			Template: map[string]any{
				"model": "vibe-local/${MODEL_ALIAS}",
			},
		},
	}
	r, err := Activate(p, profile.ExpandContext{
		VibeAPI:    "http://127.0.0.1:9000/v1",
		ModelAlias: "qwen3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.WroteFile != target {
		t.Errorf("WroteFile = %q", r.WroteFile)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	if got["model"] != "vibe-local/qwen3" {
		t.Errorf("model = %v (template should still expand)", got["model"])
	}

	mcpBlock, ok := got["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level mcp map, got %T: %v", got["mcp"], got["mcp"])
	}
	if len(mcpBlock) != 2 {
		t.Errorf("len(mcp) = %d, want 2", len(mcpBlock))
	}

	dd, ok := mcpBlock["datadog"].(map[string]any)
	if !ok {
		t.Fatalf("mcp.datadog = %v (%T)", mcpBlock["datadog"], mcpBlock["datadog"])
	}
	if dd["type"] != "local" {
		t.Errorf("mcp.datadog.type = %v", dd["type"])
	}
	env, ok := dd["environment"].(map[string]any)
	if !ok {
		t.Fatalf("mcp.datadog.environment = %v (%T)", dd["environment"], dd["environment"])
	}
	if env["DD_API_KEY"] != "${env:DD_API_KEY}" {
		t.Errorf("mcp.datadog.environment.DD_API_KEY = %v, want literal pass-through", env["DD_API_KEY"])
	}

	ji, ok := mcpBlock["jira"].(map[string]any)
	if !ok {
		t.Fatalf("mcp.jira = %v (%T)", mcpBlock["jira"], mcpBlock["jira"])
	}
	if ji["type"] != "local" {
		t.Errorf("mcp.jira.type = %v", ji["type"])
	}
}

func TestActivate_External_NoMCPs_OmitsBlock(t *testing.T) {
	// Even with an MCPDir present, an empty MCPs slice must not introduce
	// a top-level "mcp" key.
	seedMCPDir(t, map[string]string{
		"datadog": "type: local\n",
	})

	target := filepath.Join(t.TempDir(), "opencode.json")
	p := &profile.Profile{
		Name:    "code",
		Backend: profile.Backend{LlamaServer: &profile.LlamaServerBackend{Path: "/m.gguf", Alias: "x", Context: 1, Parallel: 1}},
		Frontend: profile.Frontend{
			Kind:      profile.FrontendExternal,
			App:       "opencode",
			WriteFile: target,
			Template:  map[string]any{"a": 1},
		},
	}
	if _, err := Activate(p, profile.ExpandContext{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["mcp"]; present {
		t.Errorf("got top-level mcp key with no MCPs requested: %v", got)
	}
}

func TestActivate_External_MCPConflictsWithTemplateKey(t *testing.T) {
	seedMCPDir(t, map[string]string{
		"datadog": "type: local\n",
	})

	target := filepath.Join(t.TempDir(), "opencode.json")
	p := &profile.Profile{
		Name:    "code",
		Backend: profile.Backend{LlamaServer: &profile.LlamaServerBackend{Path: "/m.gguf", Alias: "x", Context: 1, Parallel: 1}},
		Frontend: profile.Frontend{
			Kind:      profile.FrontendExternal,
			App:       "opencode",
			WriteFile: target,
			MCPs:      []string{"datadog"},
			Template: map[string]any{
				"mcp": map[string]any{"already": "defined"},
			},
		},
	}
	_, err := Activate(p, profile.ExpandContext{})
	if err == nil {
		t.Fatal("expected error when template already defines top-level mcp key")
	}
	if !strings.Contains(err.Error(), "mcp") {
		t.Errorf("err = %v, want mention of mcp", err)
	}
	// File should not have been written.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("write_file %s exists; should have errored before writing", target)
	}
}

func TestActivate_External_MissingMCPDefinition(t *testing.T) {
	seedMCPDir(t, map[string]string{
		"jira": "type: local\n",
	})

	target := filepath.Join(t.TempDir(), "opencode.json")
	p := &profile.Profile{
		Name:    "code",
		Backend: profile.Backend{LlamaServer: &profile.LlamaServerBackend{Path: "/m.gguf", Alias: "x", Context: 1, Parallel: 1}},
		Frontend: profile.Frontend{
			Kind:      profile.FrontendExternal,
			App:       "opencode",
			WriteFile: target,
			MCPs:      []string{"datadog"},
			Template:  map[string]any{"a": 1},
		},
	}
	_, err := Activate(p, profile.ExpandContext{})
	if err == nil {
		t.Fatal("expected error for missing mcp")
	}
	if !strings.Contains(err.Error(), "datadog") {
		t.Errorf("err = %v, want mention of missing mcp name", err)
	}
	if !strings.Contains(err.Error(), "jira") {
		t.Errorf("err = %v, want available list to include 'jira'", err)
	}
}
