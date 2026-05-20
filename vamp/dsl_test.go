package vamp

import (
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

// TestBuild_MinimalPipeline confirms the smallest reasonable DSL invocation
// produces a Validate-passing pipeline. Anything less than this fails to
// exercise the chain at all.
func TestBuild_MinimalPipeline(t *testing.T) {
	p := New("minimal").
		Describe("smoke test").
		Input("name", Required())

	p.Text("hello").
		Capability("fast_drafting").
		Prompt("Say hi to {{.inputs.name}}.").
		Output("hello.txt")

	built, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := built.inner.Name; got != "minimal" {
		t.Errorf("Name = %q, want %q", got, "minimal")
	}
	if got := len(built.inner.Stages); got != 1 {
		t.Errorf("len(Stages) = %d, want 1", got)
	}
	if got := built.inner.Stages[0].Capability; got != "fast_drafting" {
		t.Errorf("Stage capability = %q, want %q", got, "fast_drafting")
	}
}

// TestBuild_CrossStageRefs verifies that After / Foreach take typed Ref
// arguments and the resulting Stage.Inputs slice contains the upstream
// stage ids. This is the compile-time-safety win of the DSL — calling
// After with the variable holding a *TextStage produces a string id at
// Build time, no string typos to worry about.
func TestBuild_CrossStageRefs(t *testing.T) {
	p := New("refs")
	queries := p.Text("queries").
		Capability("fast_drafting").
		Prompt(`["one","two"]`).
		Output("q.json").
		OutputFormatJSON()

	p.Text("judge").
		Capability("careful_reasoning").
		After(queries).
		Foreach(queries, "q").
		Prompt("Judge {{.q}}").
		Output("judge_{{.i}}.txt")

	built, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	judge := built.inner.Stages[1]
	if got, want := judge.Inputs, []string{"queries"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("judge.Inputs = %v, want %v", got, want)
	}
	if judge.Foreach == nil || judge.Foreach.From != "queries" {
		t.Errorf("judge.Foreach = %+v, want From=queries", judge.Foreach)
	}
	if judge.Foreach.Var != "q" {
		t.Errorf("judge.Foreach.Var = %q, want %q", judge.Foreach.Var, "q")
	}
}

// TestBuild_ForeachAutoAddsDep verifies the convenience that calling
// Foreach(upstream, ...) implicitly adds upstream to Inputs even when the
// caller forgot to call After. The YAML loader requires explicit Inputs
// declaration; the DSL papers over that small footgun.
func TestBuild_ForeachAutoAddsDep(t *testing.T) {
	p := New("foreach-auto")
	queries := p.Text("queries").
		Capability("fast").
		Prompt(`["a"]`).
		Output("q.json").
		OutputFormatJSON()

	// Note: no .After(queries) — Foreach should add the dep itself.
	p.Text("judge").
		Capability("fast").
		Foreach(queries, "q").
		Prompt("{{.q}}").
		Output("j_{{.i}}.txt")

	built, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := built.inner.Stages[1].Inputs; len(got) != 1 || got[0] != "queries" {
		t.Errorf("auto-added Inputs = %v, want [queries]", got)
	}
}

// TestBuild_PromptFSPropagatesAssetFS confirms PromptFS sets both the path
// and the AssetFS field on the underlying Stage so the executor's asset
// reader picks the FS branch.
func TestBuild_PromptFSPropagatesAssetFS(t *testing.T) {
	mfs := fstest.MapFS{
		"hello.tmpl": &fstest.MapFile{Data: []byte("Hi {{.inputs.name}}")},
	}
	p := New("prompt-fs").Input("name", Required())
	p.Text("hello").
		Capability("fast").
		PromptFS(mfs, "hello.tmpl").
		Output("out.txt")

	built, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := built.inner.Stages[0]
	if s.PromptFile != "hello.tmpl" {
		t.Errorf("PromptFile = %q, want hello.tmpl", s.PromptFile)
	}
	if s.AssetFS == nil {
		t.Errorf("AssetFS = nil, want non-nil")
	}
}

// TestBuild_WebhookBodyTemplateInline wraps an inline template body into an
// in-memory fs.FS automatically so the executor's BodyTemplateFile code
// path is reused.
func TestBuild_WebhookBodyTemplateInline(t *testing.T) {
	p := New("inline-body")
	p.Webhook("send").
		URL("https://example.com/h").
		Method("POST").
		BodyTemplate(`{"text":"hi"}`).
		Output("resp.json")

	built, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := built.inner.Stages[0]
	if s.BodyTemplateFile == "" {
		t.Errorf("BodyTemplateFile is empty; inline body should set it")
	}
	if s.AssetFS == nil {
		t.Errorf("AssetFS = nil, want in-memory FS for inline body")
	}
}

// TestBuild_RejectsInvalidPipeline confirms Build runs Validate and surfaces
// errors. The minimal failure shape is a stage without an Output.
func TestBuild_RejectsInvalidPipeline(t *testing.T) {
	p := New("bad")
	p.Text("missing_output").
		Capability("fast").
		Prompt("hi")
	// No Output set; Pipeline.Validate must reject this.

	if _, err := p.Build(); err == nil {
		t.Fatalf("Build succeeded; want validation error")
	} else if !strings.Contains(err.Error(), "output is required") {
		t.Errorf("Build error = %v, want substring \"output is required\"", err)
	}
}

// TestBuild_YamlMarshalRoundtrip verifies the in-memory pipeline marshals
// to YAML that the existing yaml.v3 loader could (in principle) consume.
// The point isn't a literal round-trip — yaml.Marshal won't reconstruct
// AssetFS — but the marshaled form should contain the stage IDs, types,
// and capabilities a YAML reader would expect.
func TestBuild_YamlMarshalRoundtrip(t *testing.T) {
	p := New("yaml-rt").Input("foo", Required())
	alpha := p.Text("alpha").
		Capability("fast").
		Prompt("hi").
		Output("a.txt")
	p.Text("beta").
		Capability("careful").
		After(alpha).
		Prompt("{{.stages.alpha.output}}").
		Output("b.txt")

	built, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	bytes, err := yaml.Marshal(built.inner)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	got := string(bytes)
	for _, want := range []string{"yaml-rt", "alpha", "beta", "careful"} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled YAML missing %q; output:\n%s", want, got)
		}
	}
}
