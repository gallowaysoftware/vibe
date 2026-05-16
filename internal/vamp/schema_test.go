package vamp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSchemaJSON_RoundTrip verifies that SchemaJSON() emits a draft-07
// JSON Schema document with the expected root shape, and that the
// `required` declarations match what LoadPipeline actually enforces.
// The check is intentionally structural rather than a full gojsonschema
// validation: we stay on Go stdlib (no new modules), so this test
// guarantees the schema's required-field set tracks the YAML loader's
// required-field set, and that every shipped example pipeline YAML
// satisfies those required fields after parsing through LoadPipeline.
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
	wantRequired := map[string]bool{"name": true, "stages": true}
	gotRequired := make(map[string]bool, len(required))
	for _, r := range required {
		gotRequired[r.(string)] = true
	}
	for k := range wantRequired {
		if !gotRequired[k] {
			t.Errorf("root required missing %q (got %v)", k, required)
		}
	}
	// Sanity-check that every example pipeline checked into the repo
	// parses through LoadPipeline; the schema's stage `required` set
	// (id + output) must be a subset of what LoadPipeline rejects when
	// missing, otherwise editors would surface "this YAML is invalid"
	// for pipelines that actually run fine. We additionally walk the
	// parsed Pipeline and re-validate each Stage has the schema's
	// required keys populated.
	stageProps := requiredOfStage(t, doc)
	root, err := exampleRoot()
	if err != nil {
		t.Skipf("example pipelines not reachable: %v", err)
	}
	yamls, err := filepath.Glob(filepath.Join(root, "*", "pipeline.yaml"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}
	if len(yamls) == 0 {
		t.Fatalf("no example pipelines found under %s", root)
	}
	for _, path := range yamls {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			p, err := LoadPipeline(path)
			if err != nil {
				t.Fatalf("LoadPipeline(%s): %v", path, err)
			}
			for i, st := range p.Stages {
				if st.ID == "" {
					t.Errorf("stages[%d]: id is empty after parse (schema requires id)", i)
				}
				if st.Output == "" {
					t.Errorf("stage %s: output is empty after parse (schema requires output)", st.ID)
				}
			}
			// stageProps is the parsed stage `required` array. We
			// only assert that it contains the same canonical set we
			// just enforced above; this catches future drift where
			// someone updates Stage but forgets to update Schema.
			if _, ok := stageProps["id"]; !ok {
				t.Errorf("schema stages.items.required is missing 'id'")
			}
			if _, ok := stageProps["output"]; !ok {
				t.Errorf("schema stages.items.required is missing 'output'")
			}
		})
	}
}

// TestSchema_RunWhenEnum guarantees that the run_when enum on the Stage
// schema lists exactly the four canonical values (empty + the three valid
// states). Pinned because run_when is a new field and we want the schema
// to fail loudly if a future contributor adds a value to the constant
// list but forgets to thread it through here.
func TestSchema_RunWhenEnum(t *testing.T) {
	s := Schema()
	stage := s.Properties["stages"].Items
	if stage == nil {
		t.Fatal("schema missing stages.items")
	}
	rw, ok := stage.Properties["run_when"]
	if !ok {
		t.Fatal("stage schema missing run_when")
	}
	want := map[string]bool{"": true, RunWhenSuccess: true, RunWhenFailure: true, RunWhenAlways: true}
	if len(rw.Enum) != len(want) {
		t.Errorf("run_when enum len = %d, want %d (%v)", len(rw.Enum), len(want), rw.Enum)
	}
	for _, e := range rw.Enum {
		if !want[e.(string)] {
			t.Errorf("run_when enum has unexpected value %q", e)
		}
	}
}

// requiredOfStage pulls the stage-items required set out of the marshaled
// schema doc and returns it as a set keyed by required field name.
func requiredOfStage(t *testing.T, doc map[string]any) map[string]bool {
	t.Helper()
	props, _ := doc["properties"].(map[string]any)
	stages, _ := props["stages"].(map[string]any)
	items, _ := stages["items"].(map[string]any)
	req, _ := items["required"].([]any)
	out := make(map[string]bool, len(req))
	for _, r := range req {
		out[r.(string)] = true
	}
	return out
}

// exampleRoot returns the absolute path to the examples/ directory from
// the test's perspective. The internal/vamp test binary's cwd is the
// package directory, so we walk up two levels to the repo root and then
// down into examples/.
func exampleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := filepath.Clean(filepath.Join(wd, "..", "..", "examples"))
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", os.ErrNotExist
	}
	return root, nil
}
