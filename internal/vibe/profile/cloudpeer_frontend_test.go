package profile

import (
	"strings"
	"testing"
)

// A cloud peer's model ids ARE the router's ids, so a single-model peer has an
// unambiguous ${MODEL_ALIAS}. This is the case that makes a harness profile
// against a cloud model a render rather than per-machine hand-wiring.
func TestExpandModelAliasFromSingleModelPeer(t *testing.T) {
	out, err := ExpandTemplate(map[string]any{
		"id":            "${MODEL_ALIAS}",
		"contextWindow": "${MODEL_CONTEXT}",
	}, ExpandContext{ModelAlias: "kimi-k3", ModelContext: 1048576})
	if err != nil {
		t.Fatalf("ExpandTemplate: %v", err)
	}
	if out["id"] != "kimi-k3" {
		t.Errorf("id = %v, want kimi-k3", out["id"])
	}
	// Whole-string references keep their native type: a harness reading
	// contextWindow wants a number, not "1048576".
	if out["contextWindow"] != 1048576 {
		t.Errorf("contextWindow = %#v, want int 1048576", out["contextWindow"])
	}
}

// With several models there is no right answer, and guessing one would point
// the harness at a model the user did not choose. Failing by name is the same
// contract ${VIBE_SEARCH} already uses for "real but not configured here".
func TestExpandModelAliasUnsetFailsWithHint(t *testing.T) {
	_, err := ExpandTemplate(map[string]any{"id": "${MODEL_ALIAS}"}, ExpandContext{})
	if err == nil {
		t.Fatal("expected an error when no single model id is known")
	}
	if !strings.Contains(err.Error(), "not configured") || !strings.Contains(err.Error(), "cloud_peer") {
		t.Errorf("err = %v, want it to explain the multi-model peer case", err)
	}
}

// 0 is a plausible-looking context window that a harness will either honour
// (and refuse to send anything) or ignore. Neither failure points back here.
func TestExpandModelContextUnsetFailsWithHint(t *testing.T) {
	_, err := ExpandTemplate(map[string]any{"ctx": "${MODEL_CONTEXT}"}, ExpandContext{ModelAlias: "kimi-k3"})
	if err == nil {
		t.Fatal("expected an error when no context window is declared")
	}
	if !strings.Contains(err.Error(), "not configured") || !strings.Contains(err.Error(), "cloud_peer.context") {
		t.Errorf("err = %v, want it to name cloud_peer.context as the fix", err)
	}
}

// The router owns an external backend's process, so its artifacts live
// wherever the router runs. Requiring them locally is what stopped a laptop
// from carrying a profile for a model the front serves.
func TestExternalLlamaServerSkipsOnDiskChecks(t *testing.T) {
	m := &LlamaServerBackend{
		Path:    "/nonexistent/weights.gguf",
		Alias:   "qwen3.6-27b",
		Context: 131072,
	}
	if err := validateLlamaServer(m, true); err != nil {
		t.Fatalf("external backend rejected for a path this box does not have: %v", err)
	}
	if err := validateLlamaServer(m, false); err == nil {
		t.Fatal("a vibe-supervised backend must still prove its weights exist")
	}
}

// Loosening the path check must not loosen the rules that hold wherever the
// process runs — llama-server parses --chat-template-file only after --jinja.
func TestExternalLlamaServerStillEnforcesTemplateOrdering(t *testing.T) {
	err := validateLlamaServer(&LlamaServerBackend{
		Path:             "/nonexistent/weights.gguf",
		Alias:            "qwen3.6-27b",
		Context:          131072,
		ChatTemplateFile: "/nonexistent/tpl.jinja",
	}, true)
	if err == nil || !strings.Contains(err.Error(), "jinja") {
		t.Fatalf("err = %v, want the jinja ordering constraint to still apply", err)
	}
}
