package vamp

import "sort"

// Requirements is the runtime contract a pipeline declares: which vibe
// capabilities it needs activated, which external services must be
// reachable, what hardware footprint to expect. Distributable pipeline
// binaries surface this over their `requirements` subcommand so tooling
// (vibe doctor, CI smoke tests) can verify the host before kicking off a
// multi-hour run.
//
// Capabilities are auto-derived from each stage's Capability field; the
// rest (services, GPU/disk hints, notes, model hints) are declarative
// state on the Pipeline value and set via the DSL builder methods (or
// the equivalent yaml: top-level requirements: block).
type Requirements struct {
	Pipeline     string                  `json:"pipeline"`
	Description  string                  `json:"description,omitempty"`
	Capabilities []CapabilityRequirement `json:"capabilities"`
	Services     []ServiceRequirement    `json:"services,omitempty"`
	Inputs       []InputRequirement      `json:"inputs,omitempty"`
	GPUMemory    string                  `json:"gpu_memory,omitempty"`
	DiskSpace    string                  `json:"disk_space,omitempty"`
	Notes        []string                `json:"notes,omitempty"`
	// Profile names the vibe profile the pipeline expects to hold
	// the active slot during a run (its text stages talk to that
	// profile's LLM via the proxy). Empty when the pipeline is
	// profile-agnostic. Consumed by the `activate` CLI subcommand.
	Profile string `json:"profile,omitempty"`
}

// CapabilityRequirement names a vibe capability slot the pipeline needs
// at runtime, plus the stage IDs that depend on it (so a missing profile
// produces an actionable error rather than a generic "stage X failed").
// ModelHint, when set, lets a pipeline author advertise the model class
// the prompts were tuned against — vibe doctor can refuse to set the
// profile up with a model that obviously won't satisfy the prompts.
type CapabilityRequirement struct {
	Name      string     `json:"name"`
	UsedBy    []string   `json:"used_by"`
	ModelHint *ModelHint `json:"model_hint,omitempty"`
}

// ModelHint advertises what the prompts behind a capability assume of the
// underlying model. None of these are enforced at run time — they exist
// so tooling can warn the operator before a multi-hour run kicks off
// against a model that obviously can't satisfy the prompts (e.g. a 7B
// dense model with 8K context standing in for a 27B-class long-form
// capability).
type ModelHint struct {
	MinParams      string   `json:"min_params,omitempty"`
	MinContext     int      `json:"min_context,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	SuggestedModel string   `json:"suggested_model,omitempty"`
}

// ServiceRequirement names a non-vibe HTTP service the pipeline needs
// reachable on the host (SearXNG, Kokoro-FastAPI, an internal API, …).
// SetupHint is a one-line command the operator can run to bring the
// service up, surfaced verbatim by tooling when the URL doesn't answer.
type ServiceRequirement struct {
	Name        string `json:"name"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description"`
	SetupHint   string `json:"setup_hint,omitempty"`
}

// InputRequirement mirrors a pipeline-level Input declaration in a JSON-
// friendly shape for the requirements report. Pipeline binaries that
// expose first-class CLI flags translate these into Cobra flag
// definitions so `--help` documents the inputs alongside the pipeline.
type InputRequirement struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// Requirements returns the pipeline's runtime contract. Capabilities are
// auto-derived from each stage's Capability field; everything else is
// pulled from the Pipeline's declarative requirements state.
func (p *Pipeline) Requirements() Requirements {
	req := Requirements{
		Pipeline:    p.Name,
		Description: p.Description,
		Services:    append([]ServiceRequirement(nil), p.RequiredServices...),
		GPUMemory:   p.RequiredGPUMemory,
		DiskSpace:   p.RequiredDiskSpace,
		Notes:       append([]string(nil), p.RequirementNotes...),
		Profile:     p.RequiredProfile,
	}

	caps := make(map[string][]string)
	for _, s := range p.Stages {
		if s.Capability == "" {
			continue
		}
		caps[s.Capability] = append(caps[s.Capability], s.ID)
	}
	for name, users := range caps {
		cr := CapabilityRequirement{Name: name, UsedBy: users}
		if hint, ok := p.CapabilityModelHints[name]; ok {
			h := hint
			cr.ModelHint = &h
		}
		req.Capabilities = append(req.Capabilities, cr)
	}
	sort.Slice(req.Capabilities, func(i, j int) bool {
		return req.Capabilities[i].Name < req.Capabilities[j].Name
	})

	for name, spec := range p.Inputs {
		req.Inputs = append(req.Inputs, InputRequirement{
			Name:        name,
			Required:    spec.Required,
			Default:     spec.Default,
			Description: spec.Description,
		})
	}
	sort.Slice(req.Inputs, func(i, j int) bool {
		return req.Inputs[i].Name < req.Inputs[j].Name
	})

	return req
}
