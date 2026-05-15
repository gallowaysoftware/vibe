package frontend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/profile"
)

func TestActivate_External_WritesExpandedJSON(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "opencode.json")
	p := &profile.Profile{
		Name: "code",
		Model: profile.Model{
			Path:     "/m.gguf",
			Alias:    "qwen3",
			Context:  8192,
			Parallel: 1,
		},
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

func TestActivate_UnsupportedKind(t *testing.T) {
	p := &profile.Profile{Frontend: profile.Frontend{Kind: profile.FrontendDockerCompose}}
	if _, err := Activate(p, profile.ExpandContext{}); err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}
