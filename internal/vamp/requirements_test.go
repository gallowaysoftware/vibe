package vamp

import (
	"testing"
)

// TestRequirements_AutoDerivesCapabilities pins the contract that
// Pipeline.Requirements() walks Stages, collects distinct Capability
// values, and records which stages depend on each. This is the load-
// bearing piece that lets pipeline binaries advertise their runtime
// contract without the author having to maintain a hand-written list
// that drifts from the stage graph.
func TestRequirements_AutoDerivesCapabilities(t *testing.T) {
	p := &Pipeline{
		Name: "test",
		Stages: []Stage{
			{ID: "first", Capability: "long_form", Output: "first.txt"},
			{ID: "second", Capability: "vision", Output: "second.txt"},
			{ID: "third", Capability: "long_form", Output: "third.txt"},
			{ID: "no_cap", Output: "no_cap.txt"}, // no capability — should not appear
		},
	}
	req := p.Requirements()
	if got := len(req.Capabilities); got != 2 {
		t.Fatalf("Capabilities: want 2 distinct, got %d (%v)", got, req.Capabilities)
	}
	// Sorted alphabetically: long_form before vision.
	if req.Capabilities[0].Name != "long_form" {
		t.Errorf("Capabilities[0].Name = %q, want long_form", req.Capabilities[0].Name)
	}
	if got, want := len(req.Capabilities[0].UsedBy), 2; got != want {
		t.Errorf("long_form UsedBy: got %d entries, want %d (%v)", got, want, req.Capabilities[0].UsedBy)
	}
	if req.Capabilities[1].Name != "vision" {
		t.Errorf("Capabilities[1].Name = %q, want vision", req.Capabilities[1].Name)
	}
}

// TestRequirements_ManualFieldsRoundTrip pins that the declarative
// requirements state (services, GPU/disk hints, notes, model hints)
// reaches the Requirements() report verbatim. Equivalent to the
// builder-method round-trip in vamp/dsl.go — kept here in the internal
// package so the type-level contract is independently tested.
func TestRequirements_ManualFieldsRoundTrip(t *testing.T) {
	p := &Pipeline{
		Name:              "test",
		Description:       "a pipeline",
		Stages:            []Stage{{ID: "only", Capability: "long_form", Output: "out.txt"}},
		RequiredGPUMemory: "32GB",
		RequiredDiskSpace: "20GB",
		RequirementNotes:  []string{"note one", "note two"},
		RequiredServices: []ServiceRequirement{
			{Name: "searxng", URL: "http://127.0.0.1:14002", Description: "Search", SetupHint: "docker compose up -d"},
		},
		CapabilityModelHints: map[string]ModelHint{
			"long_form": {MinParams: "27B", MinContext: 131072, SuggestedModel: "qwen3.6-27b"},
		},
		Inputs: map[string]InputSpec{
			"topic": {Required: true, Description: "what to write about"},
		},
	}
	req := p.Requirements()
	if req.Pipeline != "test" || req.Description != "a pipeline" {
		t.Errorf("Pipeline/Description not propagated: %+v", req)
	}
	if req.GPUMemory != "32GB" || req.DiskSpace != "20GB" {
		t.Errorf("GPU/disk hints not propagated: gpu=%q disk=%q", req.GPUMemory, req.DiskSpace)
	}
	if len(req.Notes) != 2 || req.Notes[0] != "note one" {
		t.Errorf("Notes not propagated: %v", req.Notes)
	}
	if len(req.Services) != 1 || req.Services[0].Name != "searxng" {
		t.Errorf("Services not propagated: %v", req.Services)
	}
	if req.Capabilities[0].ModelHint == nil || req.Capabilities[0].ModelHint.MinParams != "27B" {
		t.Errorf("ModelHint not attached: %+v", req.Capabilities[0])
	}
	if len(req.Inputs) != 1 || req.Inputs[0].Name != "topic" || !req.Inputs[0].Required {
		t.Errorf("Inputs not propagated: %+v", req.Inputs)
	}
}
