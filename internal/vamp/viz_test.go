package vamp

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot returns the absolute path of the repository root from the
// per-package working directory used by `go test`. Tests run with the
// package directory as CWD, so the project root is two levels up.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return abs
}

// assertContains asserts that every substring is present in the rendered
// output. We deliberately do not byte-compare to a golden file: the
// renderer's whitespace and line ordering details (e.g. when foreach
// edges are emitted vs. regular edges) are implementation choices, but
// the *content* — node ids, type labels, foreach dotted-arrows, class
// directives — is the contract this test pins.
func assertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("rendered Mermaid missing %q\n--- full output ---\n%s", w, got)
		}
	}
}

func loadExample(t *testing.T, rel string) *Pipeline {
	t.Helper()
	path := filepath.Join(repoRoot(t), rel)
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load %s: %v", rel, err)
	}
	return p
}

func TestRenderMermaid_StartsWithFlowchartTD(t *testing.T) {
	p := loadExample(t, "examples/multi-profile-pipeline/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{})
	if !strings.HasPrefix(got, "flowchart TD\n") {
		t.Fatalf("output must start with `flowchart TD\\n`, got: %q", got[:min(40, len(got))])
	}
}

func TestRenderMermaid_MultiProfile(t *testing.T) {
	p := loadExample(t, "examples/multi-profile-pipeline/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{})
	assertContains(t, got,
		"outline",
		"expand",
		"fast_drafting",
		"careful_reasoning",
		"outline --> expand",
		"classDef text fill:#cfe2ff",
		"class outline,expand text",
	)
	if strings.Contains(got, "foreach") {
		t.Errorf("multi-profile pipeline has no foreach stages; got dotted edge:\n%s", got)
	}
}

func TestRenderMermaid_ComfyUIImageBatch_ForeachDottedEdge(t *testing.T) {
	p := loadExample(t, "examples/comfyui-image-batch/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{})
	assertContains(t, got,
		"prompts",
		"render",
		"output_format=json",
		"foreach: prompts",
		"prompts -.foreach.-> render",
		"prompts --> render",
		"classDef text fill:",
		"classDef comfyui fill:",
		"class prompts text",
		"class render comfyui",
	)
}

func TestRenderMermaid_VideoPipeline_MultiTypeColouring(t *testing.T) {
	p := loadExample(t, "examples/video-pipeline/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{})
	// All four stages must be present, and edges from upstreams to
	// downstreams must reflect the inputs DAG declared in the YAML.
	assertContains(t, got,
		"script",
		"voiceover",
		"render",
		"assemble",
		"script --> voiceover",
		"script --> render",
		"voiceover --> assemble",
		"render --> assemble",
		// Per-type class assignments: every type-specific colour we
		// expect from this pipeline must appear as a class directive.
		"classDef text fill:",
		"classDef audio fill:",
		"classDef comfyui fill:",
		"classDef ffmpeg fill:",
		"class script text",
		"class voiceover audio",
		"class render comfyui",
		"class assemble ffmpeg",
	)
}

func TestRenderMermaid_YouTubeUpload_AllTypesPresent(t *testing.T) {
	p := loadExample(t, "examples/youtube-upload/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{})
	assertContains(t, got,
		"publish",
		// publish has three declared inputs; check the most distinctive
		// edge so the test isn't sensitive to slice ordering of the
		// other two.
		"assemble --> publish",
		"classDef youtube fill:",
		"class publish youtube",
	)
}

func TestRenderMermaid_NotifyPipeline_Webhook(t *testing.T) {
	p := loadExample(t, "examples/notify-pipeline/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{})
	assertContains(t, got,
		"haiku",
		"notify",
		"haiku --> notify",
		"classDef webhook fill:",
		"class notify webhook",
	)
}

func TestRenderMermaid_ShowInputsSubgraph(t *testing.T) {
	p := loadExample(t, "examples/multi-profile-pipeline/pipeline.yaml")
	got := RenderMermaid(p, VizOptions{ShowInputs: true})
	assertContains(t, got,
		"subgraph inputs",
		"input_topic",
		"end\n",
	)
	// Without the flag, the subgraph block must NOT appear — make sure
	// we didn't accidentally make it the default.
	plain := RenderMermaid(p, VizOptions{})
	if strings.Contains(plain, "subgraph inputs") {
		t.Errorf("subgraph inputs leaked into plain rendering:\n%s", plain)
	}
}

func TestRenderMermaid_LabelEscapeOnSpecialCharacters(t *testing.T) {
	// Build a synthetic pipeline that forces the escapeLabel branch: a
	// capability with a colon would otherwise terminate the `[...]`
	// label early in Mermaid. The renderer should wrap that label in
	// double quotes.
	p := &Pipeline{
		Name: "esc",
		Stages: []Stage{
			{
				ID:         "weird",
				Type:       StageTypeText,
				Capability: "needs:quoting",
				Prompt:     "hi",
				Output:     "x.txt",
			},
		},
	}
	got := RenderMermaid(p, VizOptions{})
	// The label inside the node must be a quoted string containing the
	// colon.
	if !regexp.MustCompile(`weird\["[^"]*needs:quoting[^"]*"\]`).MatchString(got) {
		t.Fatalf("expected quoted label around capability containing a colon, got:\n%s", got)
	}
}

// vizStage builds a one-stage pipeline whose capability is the string
// under test — capability goes straight into the node label and is the
// one label component Validate never charset-checks.
func vizStage(capability string) *Pipeline {
	return &Pipeline{
		Name: "esc",
		Stages: []Stage{{
			ID:         "weird",
			Type:       StageTypeText,
			Capability: capability,
			Prompt:     "hi",
			Output:     "x.txt",
		}},
	}
}

// TestRenderMermaid_QuotedLabelUsesMermaidEntitiesNotBackslashes covers
// what the sibling test's name implies but its assertion cannot reach:
// the one line of escapeLabel that transforms label CONTENT. Mermaid
// has no backslash escape inside a quoted label — the entity is
// #quot; — so emitting \" produced the very parse error the quoting
// exists to prevent.
func TestRenderMermaid_QuotedLabelUsesMermaidEntitiesNotBackslashes(t *testing.T) {
	got := RenderMermaid(vizStage(`say "hi"`), VizOptions{})
	if strings.Contains(got, `\"`) {
		t.Errorf("backslash escape is not Mermaid syntax; it is a parse error:\n%s", got)
	}
	if !strings.Contains(got, "#quot;hi#quot;") {
		t.Errorf("embedded quotes should become #quot; entities, got:\n%s", got)
	}
	// A literal # is the entity introducer and must itself be escaped,
	// or it swallows whatever follows it.
	hash := RenderMermaid(vizStage("c#1"), VizOptions{})
	if !strings.Contains(hash, "c#35;1") {
		t.Errorf("literal # should become #35;, got:\n%s", hash)
	}
}

// TestRenderMermaid_LabelCannotInjectFlowchartSyntax pins the widened
// trigger. The old denylist (`:|"()[]`) held none of the characters
// that can inject STRUCTURE, and Stage.Capability is never charset-
// validated — Validate only requires it to be non-empty — so a
// model-generated `capability: "a-->b"` emitted an unquoted
// `weird[a · a-->b]`, which Mermaid reads as an extra edge.
func TestRenderMermaid_LabelCannotInjectFlowchartSyntax(t *testing.T) {
	for _, capability := range []string{"a-->b", "a<b", "a{b}", "a;b", "a&b"} {
		got := RenderMermaid(vizStage(capability), VizOptions{})
		line := ""
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "weird[") {
				line = strings.TrimSpace(l)
				break
			}
		}
		if line == "" {
			t.Fatalf("capability %q: no node line in:\n%s", capability, got)
		}
		if !strings.HasPrefix(line, `weird["`) || !strings.HasSuffix(line, `"]`) {
			t.Errorf("capability %q rendered unquoted: %s", capability, line)
		}
	}
	// An inert label is still emitted bare, so the allowlist has not
	// simply become "quote everything unconditionally". Asserted against
	// escapeLabel directly: every RENDERED label carries the " · "
	// separator, which is itself outside the inert set, so the bare form
	// is unreachable through RenderMermaid.
	if got := escapeLabel("write draft 2.0"); got != "write draft 2.0" {
		t.Errorf("inert label was quoted: %q", got)
	}
	if got := escapeLabel("a-b"); got == "a-b" {
		t.Error(`"a-b" must be quoted: a hyphen is one keystroke from an edge`)
	}
}
