package vamp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The examples ship prompt and body templates, and until this file
// NOTHING RENDERED THEM. schema_test.go and viz_test.go load example
// *YAML* only; the `prompts/` and `body_templates/` directories beside
// it were never opened by any test or CI step.
//
// What that cost: examples/rag-eval-pipeline/prompts/report.tmpl — the
// last stage of the pipeline AGENTS.md calls the canonical chain, in
// both its YAML and Go-DSL forms — could not render at all. It did
//
//	{{ range .stages.judge.outputs -}}
//	{{ readFile . }}
//
// over a TEXT foreach stage, whose .outputs hold the model's own text
// rather than per-item paths (exec.go, the `outputs[i] = out.Text` arm),
// so the stage died with
//
//	open {"query":"What does BGE-M3 do?",...}: no such file or directory
//
// after four stages of real LLM and HTTP work had already run. The same
// line is the injection sink: a model that emits a bare path instead of
// JSON gets that file inlined into the report prompt.
//
// One fixed template is worth much less than the check, which is why
// this file exists. Every asset every example stage references is
// rendered here, through the real renderTemplate and the real FuncMap,
// against synthetic prior outputs built with the SAME content-vs-path
// distinction the executor uses. A template that cannot render, names a
// binding no stage provides, calls a helper that no longer exists, or
// hands a text stage's output to readFile now fails a 20ms unit test
// instead of a live run.
//
// It also refuses a template that no stage references, because a
// shipped file nothing points at is a file this check silently skips.

// exampleSyntheticFileBodies overrides the generic synthetic output for
// stages whose on-disk file has a SHAPE a downstream template destructures
// rather than merely interpolates. Keyed "<pipeline name>/<stage id>".
//
// One entry today: rag-eval's `embed` stage writes a TEI embeddings
// response, and body_templates/qdrant_search.json.tmpl reaches into it
// with `index ... "data" 0 "embedding"`. No generic fixture can satisfy
// that, and inventing one per stage type would be guessing.
//
// A new example whose template destructures an upstream body will fail
// here until it adds a row. That is the intended cost: the row is the
// only written-down record of the schema that template assumes.
var exampleSyntheticFileBodies = map[string]string{
	"rag-eval/embed": `{"data":[{"embedding":[0.1,0.2,0.3]}]}`,
}

// exampleForeachItems overrides the synthetic per-item foreach binding
// for stages whose item is not a plain string. Keyed the same way.
// Empty today — every example's foreach iterates strings — and present
// so the next pipeline that fans out over objects has somewhere to say
// so instead of weakening the harness.
var exampleForeachItems = map[string]any{}

// governingPipeline maps an example directory to the pipeline.yaml that
// describes it.
//
// A "<name>-go" directory is the Go-DSL mirror of "<name>": pipeline.go
// builds the identical DAG (its own doc comment says so) and ships
// byte-identical templates, but it is `package main` under examples/ and
// cannot be imported from here. Driving its templates through the
// sibling's YAML is what puts them under the same check — and if the two
// ever diverge, the mirror's templates fail here, which is the correct
// alarm for a mirror that stopped mirroring.
func governingPipeline(root, dir string) string {
	if p := filepath.Join(root, dir, "pipeline.yaml"); fileExists(p) {
		return p
	}
	if base, ok := strings.CutSuffix(dir, "-go"); ok {
		if p := filepath.Join(root, base, "pipeline.yaml"); fileExists(p) {
			return p
		}
	}
	return ""
}

// exampleAssetFiles lists every template an example ships, as paths
// relative to the example dir (the same form a stage's prompt_file /
// body_template_file field uses).
func exampleAssetFiles(t *testing.T, exDir string) []string {
	t.Helper()
	var out []string
	for _, sub := range []string{"prompts", "body_templates"} {
		entries, err := os.ReadDir(filepath.Join(exDir, sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			out = append(out, filepath.Join(sub, e.Name()))
		}
	}
	return out
}

// stageOutputIsContent reports whether `.stages.<id>.output` / `.outputs`
// carry the stage's TEXT rather than the path of a file it wrote.
//
// This is the distinction the broken example got wrong, so the harness
// has to model it rather than pick whichever is convenient: the executor
// records out.Files (absolute paths) when a stage produced files and
// out.Text otherwise, and the resume path agrees. Binding content where
// the runtime binds content is the entire reason a `readFile` over a text
// stage's output fails here.
func stageOutputIsContent(st *Stage) bool {
	switch st.Type {
	case "", StageTypeText, StageTypeWebhook, StageTypeCompact, StageTypeRender:
		return true
	default:
		return false
	}
}

func exampleForeachBinding(p *Pipeline, st *Stage, i int) map[string]any {
	if st.Foreach == nil {
		return nil
	}
	v := st.Foreach.Var
	if v == "" {
		v = "item"
	}
	var item any = fmt.Sprintf("synthetic %s item %d", st.ID, i)
	if override, ok := exampleForeachItems[p.Name+"/"+st.ID]; ok {
		item = override
	}
	return map[string]any{v: item, "i": i}
}

func exampleSyntheticBody(p *Pipeline, st *Stage) string {
	if b, ok := exampleSyntheticFileBodies[p.Name+"/"+st.ID]; ok {
		return b
	}
	if st.OutputFormat == "json" {
		return fmt.Sprintf(`{"stage":%q,"synthetic":true}`, st.ID)
	}
	return fmt.Sprintf("synthetic output of stage %s", st.ID)
}

// renderExampleAssets walks the pipeline in declaration order, giving
// each stage a synthetic on-disk output and a synthetic prior binding,
// and renders every asset any stage references. Returns the set of
// assets it rendered so the caller can catch the ones it did not.
func renderExampleAssets(t *testing.T, p *Pipeline, exDir string) map[string]bool {
	t.Helper()
	runDir := t.TempDir()

	cliInputs := make(map[string]string, len(p.Inputs))
	for name, spec := range p.Inputs {
		if spec.Default != "" {
			cliInputs[name] = spec.Default
			continue
		}
		cliInputs[name] = "example-" + name
	}

	// Pre-seeded so a stage that names a dependency declared LATER in the
	// file renders against an empty prior rather than failing the harness
	// with a scheduler-bug message that would be about the harness, not
	// the template.
	prior := make(map[string]*stageResult, len(p.Stages))
	for i := range p.Stages {
		prior[p.Stages[i].ID] = &stageResult{}
	}

	// Two items per foreach: one cannot tell a per-item binding from a
	// whole-stage one, which is exactly the confusion report.tmpl shipped.
	const foreachItems = 2

	rendered := make(map[string]bool)
	for i := range p.Stages {
		st := &p.Stages[i]

		// Assets first: every dependency is an earlier stage, so `prior`
		// already holds what the runtime would have bound by now.
		for _, asset := range []string{st.PromptFile, st.BodyTemplateFile} {
			if asset == "" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(exDir, asset))
			if err != nil {
				t.Errorf("stage %q references %s, which is not shipped: %v", st.ID, asset, err)
				continue
			}
			rendered[filepath.Clean(asset)] = true
			n := 1
			if st.Foreach != nil {
				n = foreachItems
			}
			for k := 0; k < n; k++ {
				out, err := renderTemplate(st.ID, string(raw), st.Inputs, cliInputs, prior, runDir, exampleForeachBinding(p, st, k))
				if err != nil {
					t.Errorf("%s (stage %q, item %d) does not render: %v", asset, st.ID, k, err)
					continue
				}
				if strings.TrimSpace(out) == "" {
					t.Errorf("%s (stage %q, item %d) rendered empty", asset, st.ID, k)
				}
			}
		}

		// Then this stage's own synthetic result, so the next stage sees it.
		n := 1
		if st.Foreach != nil {
			n = foreachItems
		}
		outputs := make([]string, 0, n)
		for k := 0; k < n; k++ {
			path, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, cliInputs, prior, runDir, exampleForeachBinding(p, st, k))
			if err != nil {
				t.Fatalf("stage %q output template %q does not render: %v", st.ID, st.Output, err)
			}
			abs := filepath.Join(runDir, path)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			body := exampleSyntheticBody(p, st)
			if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if stageOutputIsContent(st) {
				outputs = append(outputs, body)
			} else {
				outputs = append(outputs, abs)
			}
		}
		res := &stageResult{Output: strings.Join(outputs, "\n")}
		if st.Foreach != nil {
			res.Outputs = outputs
		}
		prior[st.ID] = res
	}
	return rendered
}

// TestExampleTemplatesRender renders every prompt / body template every
// example ships. See the file comment for what this replaces.
func TestExampleTemplatesRender(t *testing.T) {
	root, err := exampleRoot()
	if err != nil {
		t.Fatalf("locate examples/: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		exDir := filepath.Join(root, e.Name())
		assets := exampleAssetFiles(t, exDir)
		if len(assets) == 0 {
			continue
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			yamlPath := governingPipeline(root, e.Name())
			if yamlPath == "" {
				t.Fatalf("%s ships %d template(s) but no pipeline.yaml describes them, "+
					"so nothing can render them: %v", e.Name(), len(assets), assets)
			}
			p, err := LoadPipeline(yamlPath)
			if err != nil {
				t.Fatalf("load %s: %v", yamlPath, err)
			}
			got := renderExampleAssets(t, p, exDir)
			for _, a := range assets {
				if !got[filepath.Clean(a)] {
					t.Errorf("%s ships %s but no stage in %s references it, "+
						"so it is never rendered — by this test or by the pipeline",
						e.Name(), a, filepath.Base(yamlPath))
				}
			}
		})
	}
	// The walk itself is a guard: a rename of examples/ or of the
	// prompts/ convention would otherwise turn this whole file into a
	// green no-op, which is the failure mode it was written against.
	if checked == 0 {
		t.Fatal("no example shipped a prompt or body template — the walk is broken, not the examples")
	}
}

// TestExampleReportTemplateDoesNotReadAJudgeRecordAsAPath is the
// regression pinned at the exact shape of the shipped defect, separately
// from the harness above, because the harness proves "it renders" and
// this proves WHY it used to not: a text foreach stage's `.outputs` are
// records, and handing one to readFile asks the filesystem to open a
// JSON blob.
//
// Driven against the real file on disk rather than an inline copy — an
// inline copy would keep passing after someone reintroduced the bug in
// the example.
func TestExampleReportTemplateDoesNotReadAJudgeRecordAsAPath(t *testing.T) {
	root, err := exampleRoot()
	if err != nil {
		t.Fatalf("locate examples/: %v", err)
	}
	records := []string{
		`{"query":"What does BGE-M3 do?","precision_at_5":0.8,"comment":"ok"}`,
		`{"query":"How does Qdrant index vectors?","precision_at_5":0.6,"comment":"meh"}`,
	}
	for _, ex := range []string{"rag-eval-pipeline", "rag-eval-pipeline-go"} {
		t.Run(ex, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, ex, "prompts", "report.tmpl"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := renderTemplate("report", string(raw), []string{"judge"}, nil,
				map[string]*stageResult{"judge": {Outputs: records}}, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("report.tmpl does not render: %v", err)
			}
			// Every record must reach the prompt. A template that ranges
			// but drops the value renders fine and says nothing.
			for _, rec := range records {
				if !strings.Contains(out, rec) {
					t.Errorf("judge record missing from the rendered report:\n  want: %s\n  got:\n%s", rec, out)
				}
			}
		})
	}
}
