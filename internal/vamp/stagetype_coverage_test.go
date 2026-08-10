package vamp

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// This file is the mechanical half of the fix for "a per-type rule
// restated in N places is a rule that is right in N-1 of them"
// (exec.go's allStageTypes comment). It adds the two consumers that
// comment did not count — diff.go and dryrun.go — to the set of
// consumers a test walks, and adds a syntactic backstop so a switch in
// either file that stops naming every stage type fails before anybody
// has to notice a stage rendering as silence.
//
// The three tests below fail in three different ways on purpose:
//
//   - TestEveryStageTypeIsPreviewedByDryRun runs the real DryRun over one
//     stage of every type. It caught `unknown stage type "pandoc"`.
//   - TestEveryStageTypeIsComparedByDiff runs the real Compare over two
//     runs whose yaml is byte-identical and whose inputs differ. It
//     caught four stage types whose prompt-equivalent was never compared
//     and therefore rendered as agreement.
//   - TestStageTypeSwitchesAreExhaustive is syntactic: it reads
//     allStageTypes out of exec.go's AST and requires every stage-type
//     switch in diff.go / dryrun.go to name every one of them. It is the
//     rung that goes red at the commit that ADDS a stage type, rather
//     than at the commit that first dry-runs one.

// stageTypeFixtures returns one minimal stage of every type, each of
// which embeds `{{.inputs.topic}}` in its type-specific payload — the
// prompt for a text stage, the script_file for a mix stage, the body for
// a webhook stage. That is what lets one table drive both the dry-run
// walk (the payload must appear in the plan) and the diff walk (changing
// topic must produce a prompt diff).
//
// The map's key set is asserted against allStageTypes by every caller,
// so a new stage type fails here first with a message that says what to
// add rather than in whichever consumer happens to be walked first.
func stageTypeFixtures() map[StageType]Stage {
	return map[StageType]Stage{
		StageTypeText: {
			ID: "s", Type: StageTypeText, Capability: "reasoning",
			Prompt: "summarise {{.inputs.topic}}", Output: "s.md",
		},
		StageTypeComfyUI: {
			ID: "s", Type: StageTypeComfyUI, Capability: "image_gen",
			Workflow:   "wf.json",
			Parameters: map[string]string{"4.text": "cover for {{.inputs.topic}}"},
			Output:     "s.png",
		},
		StageTypeAudio: {
			ID: "s", Type: StageTypeAudio,
			Text: "read this: {{.inputs.topic}}", Voice: "en_US-test",
			Output: "s.wav",
		},
		StageTypeFFmpeg: {
			ID: "s", Type: StageTypeFFmpeg,
			FFmpegArgs: []string{"-i", "clip-{{.inputs.topic}}.wav"},
			Output:     "s.mp4",
		},
		StageTypeYouTube: {
			ID: "s", Type: StageTypeYouTube,
			Title: "episode: {{.inputs.topic}}", Description: "about it", Video: "s.mp4",
			Output: "s.json",
		},
		StageTypeWebhook: {
			ID: "s", Type: StageTypeWebhook,
			URL:    "https://hooks.example.com/services/T1/B1/AAAASECRETAAAA",
			Method: "POST",
			Body:   map[string]any{"text": "done: {{.inputs.topic}}"},
			Output: "s.json",
		},
		StageTypeConfirm: {
			ID: "s", Type: StageTypeConfirm,
			Message: "ship {{.inputs.topic}}?", Output: "s.txt",
		},
		StageTypeRender: {
			ID: "s", Type: StageTypeRender,
			Prompt: "rendered {{.inputs.topic}}", Output: "s.md",
		},
		StageTypeCompact: {
			ID: "s", Type: StageTypeCompact, Capability: "reasoning",
			Source: "a long document about {{.inputs.topic}}", TargetChars: 10,
			Output: "s.md",
		},
		StageTypePandoc: {
			ID: "s", Type: StageTypePandoc,
			SourceFile: "book-{{.inputs.topic}}.md", PandocFrom: "markdown", PandocTo: "epub3",
			Output: "s.epub",
		},
		StageTypeMix: {
			ID: "s", Type: StageTypeMix,
			ScriptFile: "script-{{.inputs.topic}}.json", Output: "s.m4b",
		},
		StageTypeShort: {
			ID: "s", Type: StageTypeShort,
			ScriptFile: "shots-{{.inputs.topic}}.json", Output: "s.mp4",
		},
	}
}

// assertCoversAllStageTypes fails when keys is not exactly the
// allStageTypes set. Every table in this file goes through it so a new
// stage type cannot be half-covered by a table that silently skipped it.
func assertCoversAllStageTypes[V any](t *testing.T, what string, m map[StageType]V) {
	t.Helper()
	for _, typ := range allStageTypes {
		if _, ok := m[typ]; !ok {
			t.Fatalf("%s has no entry for stage type %q: every consumer of allStageTypes must handle every one of them", what, typ)
		}
	}
	for typ := range m {
		found := false
		for _, known := range allStageTypes {
			if typ == known {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s has an entry for %q, which is not in allStageTypes", what, typ)
		}
	}
}

// dryRunPlanMarkers is the per-type line the plan must contain. It is the
// difference between "the dry run did not crash" and "the dry run said
// something about this stage type" — an empty per-stage block was one of
// the two failure shapes this file exists to make impossible.
func dryRunPlanMarkers() map[StageType]string {
	return map[StageType]string{
		StageTypeText:    "prompt (",
		StageTypeComfyUI: "workflow: wf.json",
		StageTypeAudio:   "piper:",
		StageTypeFFmpeg:  "argv:",
		StageTypeYouTube: "youtube:",
		StageTypeWebhook: "webhook:",
		StageTypeConfirm: "message (",
		StageTypeRender:  "template output (",
		StageTypeCompact: "compact:",
		StageTypePandoc:  "pandoc:",
		StageTypeMix:     "mix:",
		StageTypeShort:   "short:",
	}
}

// dryRunPayloadEcho is the rendered per-type payload that must appear in
// the plan for the fixture above. For most types the rendered input is
// visible verbatim; the webhook is the exception — its URL is redacted
// on purpose (see TestDryRunWebhookPreviewRedactsTheURL), so the echo
// comes from its body.
func dryRunPayloadEcho() map[StageType]string {
	return map[StageType]string{
		StageTypeText:    "summarise otters",
		StageTypeComfyUI: "cover for otters",
		StageTypeAudio:   "read this: otters",
		StageTypeFFmpeg:  "clip-otters.wav",
		StageTypeYouTube: "episode: otters",
		StageTypeWebhook: "done: otters",
		StageTypeConfirm: "ship otters?",
		StageTypeRender:  "rendered otters",
		StageTypeCompact: "a long document about otters",
		StageTypePandoc:  "book-otters.md",
		StageTypeMix:     "script-otters.json",
		StageTypeShort:   "shots-otters.json",
	}
}

// TestEveryStageTypeIsPreviewedByDryRun walks allStageTypes and requires
// `vamp run --dry-run` to (a) not fail and (b) print a type-specific
// block naming the rendered payload.
//
// Before the fix, compact / pandoc / mix / short reached formatStageHeader's
// default arm and returned `stage s: unknown stage type "pandoc"` — which
// aborts the WHOLE plan at that stage, on a pipeline `vamp validate`
// accepts and `vamp run` executes happily. Those four are the longest
// running stage types in the package, i.e. exactly the ones a dry run
// exists to protect.
func TestEveryStageTypeIsPreviewedByDryRun(t *testing.T) {
	fixtures := stageTypeFixtures()
	markers := dryRunPlanMarkers()
	echoes := dryRunPayloadEcho()
	assertCoversAllStageTypes(t, "stageTypeFixtures", fixtures)
	assertCoversAllStageTypes(t, "dryRunPlanMarkers", markers)
	assertCoversAllStageTypes(t, "dryRunPayloadEcho", echoes)

	for _, typ := range allStageTypes {
		t.Run(string(typ), func(t *testing.T) {
			pipelineDir := t.TempDir()
			writeTestWorkflow(t, pipelineDir)
			st := fixtures[typ]
			var logBuf bytes.Buffer
			e := &Executor{
				Pipeline:     &Pipeline{Name: "walk", Stages: []Stage{st}},
				PipelineDir:  pipelineDir,
				Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}, "image_gen": {Profile: "comfy"}}},
				Inputs:       map[string]string{"topic": "otters"},
				RunDir:       t.TempDir(),
				Log:          &logBuf,
				Inference: func(_ context.Context, _, _, _ string, _ map[string]any, _ StreamFunc) (string, error) {
					t.Errorf("inference must not be invoked during DryRun")
					return "", nil
				},
			}
			if err := e.DryRun(context.Background()); err != nil {
				t.Fatalf("DryRun on a single %q stage failed: %v\nplan:\n%s", typ, err, logBuf.String())
			}
			got := logBuf.String()
			for _, want := range []string{`stage "s"`, markers[typ], echoes[typ]} {
				if !strings.Contains(got, want) {
					t.Errorf("stage type %q: plan is missing %q.\nplan:\n%s", typ, want, got)
				}
			}
			entries, err := os.ReadDir(e.RunDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("stage type %q: DryRun wrote into RunDir: %v", typ, entries)
			}
		})
	}
}

// TestEveryStageTypeIsComparedByDiff walks allStageTypes and requires
// `vamp diff` to report SOMETHING for a stage whose rendered payload
// changed. The two runs carry a byte-identical pipeline.yaml.snapshot and
// identical outputs, so the pipeline.yaml section cannot cover for the
// per-stage comparison: the only place the change can surface is
// PromptDiff.
//
// Before the fix, compact / pandoc / mix / short fell through
// renderStagePromptForDiff's switch and returned "" on both sides, which
// compareStages reads as equality — the stage block then printed status,
// duration and `output: (identical)`. A skipped comparison rendered as
// agreement is the failure this walk makes impossible.
func TestEveryStageTypeIsComparedByDiff(t *testing.T) {
	fixtures := stageTypeFixtures()
	assertCoversAllStageTypes(t, "stageTypeFixtures", fixtures)
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)

	for _, typ := range allStageTypes {
		t.Run(string(typ), func(t *testing.T) {
			root := t.TempDir()
			st := fixtures[typ]
			snap, err := yaml.Marshal(&Pipeline{Name: "walk", Stages: []Stage{st}})
			if err != nil {
				t.Fatalf("marshal fixture pipeline: %v", err)
			}
			rec := basicRec("walk", start, StageRecord{ID: "s", Status: "ok", DurationMS: 1})
			// Identical outputs on both sides: the payload change must
			// surface through the prompt comparison or not at all.
			files := map[string]string{"s.md": "same\n"}
			a := writeDiffRun(t, root, "run-a", rec, string(snap), `{"topic":"otters"}`, files)
			b := writeDiffRun(t, root, "run-b", rec, string(snap), `{"topic":"badgers"}`, files)

			rep, err := Compare(a, b, CompareOpts{})
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if rep.PipelineYAMLDiff != "" {
				t.Fatalf("fixture bug: the two snapshots differ, so this test cannot prove anything about the per-stage comparison:\n%s", rep.PipelineYAMLDiff)
			}
			if len(rep.Stages) != 1 {
				t.Fatalf("len(Stages) = %d, want 1", len(rep.Stages))
			}
			sd := rep.Stages[0]
			if sd.PromptDiff == "" {
				var buf bytes.Buffer
				_ = rep.Markdown(&buf, false)
				t.Fatalf("stage type %q: PromptDiff is empty for two runs whose rendered payload differs — the report renders a skipped comparison as agreement.\nreport:\n%s", typ, buf.String())
			}
			for _, want := range []string{"otters", "badgers"} {
				if !strings.Contains(sd.PromptDiff, want) {
					t.Errorf("stage type %q: PromptDiff does not mention %q:\n%s", typ, want, sd.PromptDiff)
				}
			}
		})
	}
}

// TestStageTypeSwitchesAreExhaustive is the syntactic backstop: every
// switch in diff.go / dryrun.go that names ANY stage-type constant must
// name ALL of them.
//
// The list it checks against is read out of exec.go's `allStageTypes`
// declaration rather than restated here — restating it is the defect
// class — and cross-checked against the runtime slice, so the scan
// cannot drift from the list it is about.
//
// Borrowed wholesale from internal/astscan's philosophy (an inertness
// floor, and exemptions that are a name plus a written reason and that
// are an error once stale). The engine itself is local because
// astscan.Rule models "a function that calls X must also call Y", and
// this question — "a switch over an enum must name every member" — is a
// different shape.
func TestStageTypeSwitchesAreExhaustive(t *testing.T) {
	// Switches allowed to name a subset: a name, and the reason.
	// A stale exemption is an error, same as astscan's.
	exempt := map[string]string{}
	const minSwitches = 3

	names := stageTypeConstNamesFromAST(t)
	if len(names) != len(allStageTypes) {
		t.Fatalf("exec.go's allStageTypes literal has %d entries but the runtime slice has %d: the AST view and the value are out of step", len(names), len(allStageTypes))
	}

	fset := token.NewFileSet()
	found := 0
	used := map[string]bool{}
	for _, file := range []string{"diff.go", "dryrun.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		var fn string
		ast.Inspect(f, func(n ast.Node) bool {
			if d, ok := n.(*ast.FuncDecl); ok {
				fn = d.Name.Name
			}
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			named := map[string]bool{}
			for _, c := range sw.Body.List {
				cc, ok := c.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					if id, ok := expr.(*ast.Ident); ok && names[id.Name] {
						named[id.Name] = true
					}
				}
			}
			if len(named) == 0 {
				return true
			}
			found++
			if _, ok := exempt[fn]; ok {
				used[fn] = true
				return true
			}
			var missing []string
			for name := range names {
				if !named[name] {
					missing = append(missing, name)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s:%d: %s: the switch over stage types does not name %s. A per-type rule restated in N places is a rule that is right in N-1 of them: add an arm, or add an exemption with a reason to this test.",
					file, fset.Position(sw.Pos()).Line, fn, strings.Join(missing, ", "))
			}
			return true
		})
	}
	if found < minSwitches {
		t.Errorf("found %d stage-type switch(es) in diff.go/dryrun.go, expected at least %d: this scan is INERT — the code it guards was renamed, moved or deleted", found, minSwitches)
	}
	for name := range exempt {
		if !used[name] {
			t.Errorf("exemption %q matches no stage-type switch: it is STALE. Re-point it or delete it — a stale exemption is an unwatched hole", name)
		}
	}
}

// stageTypeConstNamesFromAST reads the identifiers out of exec.go's
// `var allStageTypes = []StageType{...}` literal.
func stageTypeConstNamesFromAST(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "exec.go", nil, 0)
	if err != nil {
		t.Fatalf("parse exec.go: %v", err)
	}
	names := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "allStageTypes" || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, el := range lit.Elts {
			if id, ok := el.(*ast.Ident); ok {
				names[id.Name] = true
			}
		}
		return false
	})
	if len(names) == 0 {
		t.Fatal("could not read allStageTypes out of exec.go: this scan verifies nothing")
	}
	return names
}

// writeTestWorkflow drops the two-node comfyui workflow the comfyui
// fixture references into dir.
func writeTestWorkflow(t *testing.T, dir string) {
	t.Helper()
	const workflowJSON = `{
  "4": {"class_type": "CLIPTextEncode", "inputs": {"text": "placeholder", "clip": ["3", 1]}},
  "9": {"class_type": "SaveImage", "inputs": {"filename_prefix": "vamp", "images": ["4", 0]}}
}`
	if err := os.WriteFile(filepath.Join(dir, "wf.json"), []byte(workflowJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ = fmt.Sprintf
