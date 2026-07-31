package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaJSON_RoundTrip verifies that SchemaJSON() emits a draft-07
// JSON Schema document with the expected root shape, and that the
// required-field declarations match what Profile.Validate actually
// enforces. The check is structural rather than a full gojsonschema
// validation: we stay on Go stdlib (no new modules), so this test
// guarantees the schema's required-field set tracks the YAML loader's
// required-field set, and that a representative bundled profile template
// satisfies those required fields after a synthetic fill-in pass.
func TestSchemaJSON_RoundTrip(t *testing.T) {
	data, err := SchemaJSON()
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("schema output missing trailing newline")
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if got := doc["$schema"]; got != JSONSchemaDraft {
		t.Errorf("$schema = %v, want %s", got, JSONSchemaDraft)
	}
	if got := doc["type"]; got != "object" {
		t.Errorf("root type = %v, want object", got)
	}
	required, _ := doc["required"].([]any)
	wantRequired := map[string]bool{"name": true}
	gotRequired := make(map[string]bool, len(required))
	for _, r := range required {
		gotRequired[r.(string)] = true
	}
	for k := range wantRequired {
		if !gotRequired[k] {
			t.Errorf("root required missing %q (got %v)", k, required)
		}
	}
	if gotRequired["backend"] {
		t.Errorf("root required must not include \"backend\" (backend_ref profiles have no inline backend); got %v", required)
	}

	// The inline-backend-or-backend_ref requirement lives in a shallow
	// root anyOf, mirroring Load's runtime check.
	rootAnyOf, _ := doc["anyOf"].([]any)
	anyOfReq := make(map[string]bool, len(rootAnyOf))
	for _, branch := range rootAnyOf {
		b, _ := branch.(map[string]any)
		req, _ := b["required"].([]any)
		for _, r := range req {
			anyOfReq[r.(string)] = true
		}
	}
	for _, k := range []string{"backend", "backend_ref"} {
		if !anyOfReq[k] {
			t.Errorf("root anyOf missing a branch requiring %q (got %v)", k, rootAnyOf)
		}
	}

	// Verify the discriminated-union shape on backend: schema must declare
	// oneOf with one branch per backend sub-block, matching
	// validateBackend's "exactly one of" check.
	props, _ := doc["properties"].(map[string]any)
	backend, _ := props["backend"].(map[string]any)
	oneOf, _ := backend["oneOf"].([]any)
	if len(oneOf) != 6 {
		t.Errorf("backend.oneOf length = %d, want 6 (llama_server XOR comfyui XOR http_server XOR tabby_api XOR cloud_peer XOR mlx_server)", len(oneOf))
	}

	// llama_server required keys: path, alias, context. Catches drift
	// where a new required field is added to LlamaServerBackend without
	// being added here.
	backendProps, _ := backend["properties"].(map[string]any)
	llama, _ := backendProps["llama_server"].(map[string]any)
	llamaReq, _ := llama["required"].([]any)
	llamaReqSet := toStringSet(llamaReq)
	for _, k := range []string{"path", "alias", "context"} {
		if !llamaReqSet[k] {
			t.Errorf("llama_server.required missing %q (got %v)", k, llamaReq)
		}
	}

	// mlx_server required keys: alias, context, venv. model_dir is absent
	// on purpose — a huggingface block supplies it at pull time.
	mlx, _ := backendProps["mlx_server"].(map[string]any)
	mlxReq, _ := mlx["required"].([]any)
	mlxReqSet := toStringSet(mlxReq)
	for _, k := range []string{"alias", "context", "venv"} {
		if !mlxReqSet[k] {
			t.Errorf("mlx_server.required missing %q (got %v)", k, mlxReq)
		}
	}

	// comfyui required keys: dir.
	comfy, _ := backendProps["comfyui"].(map[string]any)
	comfyReq, _ := comfy["required"].([]any)
	if set := toStringSet(comfyReq); !set["dir"] {
		t.Errorf("comfyui.required missing \"dir\" (got %v)", comfyReq)
	}

	// frontend.kind enum must list all three FrontendKind constants.
	frontend, _ := props["frontend"].(map[string]any)
	frontendProps, _ := frontend["properties"].(map[string]any)
	kindProp, _ := frontendProps["kind"].(map[string]any)
	kindEnum, _ := kindProp["enum"].([]any)
	kindSet := toStringSet(kindEnum)
	for _, want := range []string{FrontendExternal, FrontendDockerCompose, FrontendManaged} {
		if !kindSet[want] {
			t.Errorf("frontend.kind enum missing %q (got %v)", want, kindEnum)
		}
	}

	// mmproj field must surface on the llama_server property so editor
	// autocomplete shows it.
	llamaProps, _ := llama["properties"].(map[string]any)
	if _, ok := llamaProps["mmproj"]; !ok {
		t.Errorf("llama_server.properties missing mmproj")
	}
	if _, ok := llamaProps["huggingface"]; !ok {
		t.Errorf("llama_server.properties missing huggingface")
	}

	// external must surface on the backend union so editors autocomplete
	// the router-owned-lifecycle flag next to the sub-blocks it modifies.
	if _, ok := backendProps["external"]; !ok {
		t.Errorf("backend.properties missing external")
	}
}

// TestSchema_BundledTemplateRoundTrip loads the llama-server template,
// fills the REPLACE markers with synthetic but loader-acceptable values,
// and confirms the resulting YAML loads through profile.Load and
// satisfies the schema's required-field set. Pattern matches the vamp
// schema_test.go round-trip: we don't need a real validator, just
// structural agreement between the loader and the schema's `required`.
func TestSchema_BundledTemplateRoundTrip(t *testing.T) {
	// We avoid importing the cli package (cycle); read the template from
	// the source tree relative to this test file instead. Skip gracefully
	// if the layout changes — that's a CLI-side test's job to catch.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tmplPath := filepath.Join(wd, "..", "cli", "profile_templates", "llama-server.yaml")
	body, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Skipf("template not reachable from profile pkg test: %v", err)
	}

	// Synthetic fixture: stub a model file so the loader's path-exists
	// check passes, substitute the REPLACE markers, render the
	// __PROFILE_NAME__ placeholder.
	dir := t.TempDir()
	model := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	filled := string(body)
	filled = strings.ReplaceAll(filled, "__PROFILE_NAME__", "schema-fixture")
	filled = strings.ReplaceAll(filled, "~/models/REPLACE-model.gguf", model)
	filled = strings.ReplaceAll(filled, "REPLACE-model-alias", "fixture-alias")

	fixture := filepath.Join(dir, "schema-fixture.yaml")
	if err := os.WriteFile(fixture, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(fixture)
	if err != nil {
		t.Fatalf("Load: %v\n--- yaml ---\n%s", err, filled)
	}

	// Now walk the schema's root required set and assert each lands a
	// populated value on the loaded profile. This catches drift where a
	// new required field is added to the schema without being checked
	// in the loader (or vice versa).
	data, err := SchemaJSON()
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	required, _ := doc["required"].([]any)
	for _, r := range required {
		key, _ := r.(string)
		switch key {
		case "name":
			if p.Name == "" {
				t.Errorf("schema requires name but loaded profile name is empty")
			}
		case "backend":
			if p.Backend.LlamaServer == nil && p.Backend.ComfyUI == nil {
				t.Errorf("schema requires backend but loaded profile has neither sub-block set")
			}
		default:
			t.Errorf("schema declares unexpected required root key %q; extend this test", key)
		}
	}
}

// TestSchema_LegacyAppNotInProperties guards against re-introducing the
// deprecated `frontend.app:` field as a typed schema property. The field
// is silently absorbed at load time (the inline-map catch-all); surfacing
// it in the schema would tell users it's a supported value and defeat
// the cleanup.
func TestSchema_LegacyAppNotInProperties(t *testing.T) {
	s := Schema()
	frontend, ok := s.Properties["frontend"]
	if !ok {
		t.Fatal("schema missing frontend property")
	}
	if _, present := frontend.Properties["app"]; present {
		t.Errorf("frontend.properties should not declare the deprecated `app` field")
	}
}

// toStringSet collapses a []any of strings into a presence-keyed map for
// readable assertions.
func toStringSet(in []any) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out
}
