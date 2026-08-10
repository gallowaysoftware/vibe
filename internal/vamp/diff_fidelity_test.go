package vamp

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const foreachYAML = `name: fan
inputs:
  topic:
    type: string
stages:
  - id: plan
    type: text
    capability: writing
    prompt: list the chunks
    output_format: json
    output: stages/plan.json
  - id: chunk
    type: text
    capability: writing
    inputs: [plan]
    foreach:
      from: plan
      var: item
    prompt: write about {{.item}}
    output: stages/chunk-{{.i}}.md
`

// TestCompare_ForeachStageOutputsAreCompared is finding 4: a foreach
// stage's outputs were never compared AT ALL.
//
// stageOutputMetadataOnly rendered `output: stages/chunk-{{.i}}.md` with
// no per-item binding; under missingkey=error that render fails, and the
// failure returned `StageOutputSide{Missing: true}` — the same value that
// means "this stage does not exist". writeOutputBlock returns early when
// both sides are missing, so two runs whose fan-out files differed
// completely produced two lines of status and duration.
//
// Foreach is the package's primary fan-out construct: it is where the
// CONTENT of a run lives (per-chapter prose, per-shot audio, per-scene
// video). `vamp diff` compared everything except that.
func TestCompare_ForeachStageOutputsAreCompared(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("fan", start,
		StageRecord{ID: "plan", Status: "ok", DurationMS: 10},
		StageRecord{ID: "chunk", Status: "ok", DurationMS: 20},
	)
	a := writeDiffRun(t, root, "run-a", rec, foreachYAML, `{"topic":"otters"}`, map[string]string{
		"stages/plan.json":   `["alpha","beta"]`,
		"stages/chunk-0.md":  "AAAA the first run said alpha\n",
		"stages/chunk-1.md":  "AAAA the first run said beta\n",
		"stages/ignored.txt": "not a stage output\n",
	})
	b := writeDiffRun(t, root, "run-b", rec, foreachYAML, `{"topic":"otters"}`, map[string]string{
		"stages/plan.json":   `["alpha","beta"]`,
		"stages/chunk-0.md":  "ZZZZ the second run said something completely different\n",
		"stages/chunk-1.md":  "AAAA the first run said beta\n",
		"stages/ignored.txt": "not a stage output\n",
	})

	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var chunk *StageDiff
	for i := range rep.Stages {
		if rep.Stages[i].ID == "chunk" {
			chunk = &rep.Stages[i]
		}
	}
	if chunk == nil {
		t.Fatal("no chunk stage in the report")
	}
	if chunk.OutputA.Missing || chunk.OutputB.Missing {
		t.Errorf("a foreach stage whose files exist must not report Missing (that is the value for 'stage absent'): A=%+v B=%+v", chunk.OutputA, chunk.OutputB)
	}
	if chunk.OutputA.Items != 2 || chunk.OutputB.Items != 2 {
		t.Errorf("expected 2 enumerated items per side, got A=%d B=%d", chunk.OutputA.Items, chunk.OutputB.Items)
	}
	if chunk.OutputDiff == "" {
		var buf bytes.Buffer
		_ = rep.Markdown(&buf, false)
		t.Fatalf("two runs whose fan-out files differ produced no output diff.\nreport:\n%s", buf.String())
	}
	// The item that changed is named; the item that did not is not part
	// of the change.
	if !strings.Contains(chunk.OutputDiff, "chunk-0.md") {
		t.Errorf("the diff does not name the item that changed:\n%s", chunk.OutputDiff)
	}
	var buf bytes.Buffer
	if err := rep.Markdown(&buf, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "output (foreach") {
		t.Errorf("the human report does not label the fan-out block:\n%s", buf.String())
	}
	// The per-item manifest must carry sha256s, which is what makes a
	// content change visible for BINARY per-item outputs too.
	if !strings.Contains(chunk.OutputDiff, "sha256:") {
		t.Errorf("the per-item manifest carries no digests:\n%s", chunk.OutputDiff)
	}
}

// TestCompare_ForeachWithUnresolvableItemsSaysSo: when the upstream
// output is not on disk the differ cannot enumerate the fan-out. That is
// a legitimate state — but it must be REPORTED, not rendered as the
// silence that means "these agree".
func TestCompare_ForeachWithUnresolvableItemsSaysSo(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("fan", start,
		StageRecord{ID: "plan", Status: "ok", DurationMS: 10},
		StageRecord{ID: "chunk", Status: "ok", DurationMS: 20},
	)
	// No plan.json on either side: the item list is unknowable.
	files := map[string]string{"stages/chunk-0.md": "x\n"}
	a := writeDiffRun(t, root, "run-a", rec, foreachYAML, `{"topic":"otters"}`, files)
	b := writeDiffRun(t, root, "run-b", rec, foreachYAML, `{"topic":"otters"}`, files)

	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var chunk *StageDiff
	for i := range rep.Stages {
		if rep.Stages[i].ID == "chunk" {
			chunk = &rep.Stages[i]
		}
	}
	if chunk == nil {
		t.Fatal("no chunk stage in the report")
	}
	if chunk.OutputA.NotCompared == "" {
		t.Errorf("expected a stated reason, got %+v", chunk.OutputA)
	}
	var buf bytes.Buffer
	_ = rep.Markdown(&buf, false)
	if !strings.Contains(buf.String(), "not compared") {
		t.Errorf("the report must say the comparison was skipped:\n%s", buf.String())
	}
}

// TestUnifiedDiff_BoundsTheLCSTable is finding 6: lineHunks allocates an
// (n+1)x(m+1) int table with nothing bounding n or m. unifiedDiff is
// called on stage OUTPUT content, which is LLM-generated documents, and
// looksTextual only samples 8 KiB — so an arbitrarily large .md reaches
// the LCS.
//
// Measured before the bound: 622 KB of text (20,000 lines) allocated
// 3.3 GB, a clean 4x per doubling. Two 2 MB outputs project to ~34 GB on
// a box that also hosts live model servers.
//
// The fixture is 4,000 lines rather than 20,000 on purpose: the mutation
// registry entry for this bound RAISES the ceiling, so the mutated run
// really does build the table, and 4,000x4,000 costs ~128 MB where
// 20,000x20,000 costs 3.2 GB. A guard whose disarmed form OOMs the CI
// runner is not a guard, it is a second outage.
func TestUnifiedDiff_BoundsTheLCSTable(t *testing.T) {
	var a, b strings.Builder
	const lines = 4000
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&a, "line %d of the first document, long enough to matter\n", i)
		fmt.Fprintf(&b, "line %d of the second document, long enough to matter\n", i)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out := unifiedDiff(a.String(), b.String(), "a/big", "b/big")
	runtime.ReadMemStats(&after)

	if out == "" {
		t.Fatal("two different inputs produced no diff at all")
	}
	if !strings.Contains(out, "too large to diff") {
		t.Errorf("expected an honest 'too large' report, got:\n%s", firstLines(out, 6))
	}
	// The report still has to say the two sides DIFFER and by how much:
	// a bound that degrades to silence is the same defect as the one
	// above it.
	for _, want := range []string{"sha256:", "lines"} {
		if !strings.Contains(out, want) {
			t.Errorf("the 'too large' report is missing %q:\n%s", want, out)
		}
	}
	const budget = 64 << 20
	if grew := after.TotalAlloc - before.TotalAlloc; grew > budget {
		t.Errorf("unifiedDiff allocated %d MB for %d KB of input; the bound is not holding", grew>>20, (a.Len()+b.Len())>>10)
	}
}

// TestUnifiedDiff_StillDiffsOrdinarySizes guards the other direction: a
// bound set too low turns every real diff into "too large", which is a
// worse outcome than the unbounded version it replaced.
func TestUnifiedDiff_StillDiffsOrdinarySizes(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&a, "line %d\n", i)
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString("one more line\n")
	out := unifiedDiff(a.String(), b.String(), "a/x", "b/x")
	if strings.Contains(out, "too large") {
		t.Fatalf("a 500-line diff must still be diffed inline:\n%s", firstLines(out, 6))
	}
	if !strings.Contains(out, "+one more line") {
		t.Errorf("the added line is missing:\n%s", out)
	}
}

// TestCompare_MissingArtefactsAreNotReportedAsIdentical: Compare
// discarded os.ReadFile's error, so two runs with absent (or empty)
// pipeline.yaml.snapshot and inputs.json compared empty-to-empty, found
// no difference, and the human renderer printed `(identical)` for two
// comparisons that never ran. exec.go writes an EMPTY snapshot by design
// when PipelineSource is unpopulated, so this is a reachable state.
func TestCompare_MissingArtefactsAreNotReportedAsIdentical(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	recA := basicRec("demo", start, StageRecord{ID: "a", Status: "ok", DurationMS: 10})
	recB := basicRec("demo", start, StageRecord{ID: "a", Status: "ok", DurationMS: 11})
	a := writeDiffRun(t, root, "run-a", recA, "", "", nil)
	b := writeDiffRun(t, root, "run-b", recB, "", "", nil)

	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var buf bytes.Buffer
	if err := rep.Markdown(&buf, false); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "(identical)") {
		t.Errorf("a comparison that never ran must not render as agreement:\n%s", got)
	}
	for _, want := range []string{"no pipeline.yaml.snapshot", "no inputs.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not state that %q:\n%s", want, got)
		}
	}
	if !rep.RunA.SnapshotMissing || !rep.RunB.SnapshotMissing {
		t.Errorf("the JSON report does not carry the missing-artefact state: %+v / %+v", rep.RunA, rep.RunB)
	}
}

// TestCompare_UnparseableSnapshotIsAnnounced: a snapshot that exists but
// does not decode leaves r.pipeline nil, which silently degrades EVERY
// per-stage prompt and output comparison to nothing. One loud banner
// beats N silent stage blocks.
func TestCompare_UnparseableSnapshotIsAnnounced(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("demo", start, StageRecord{ID: "a", Status: "ok", DurationMS: 10})
	a := writeDiffRun(t, root, "run-a", rec, "stages: [this is not a stage list\n", `{"topic":"x"}`, nil)
	b := writeDiffRun(t, root, "run-b", rec, twoStageYAML, `{"topic":"y"}`, nil)

	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !rep.RunA.SnapshotUnparsed {
		t.Errorf("run A's snapshot did not parse and the report does not say so: %+v", rep.RunA)
	}
	var buf bytes.Buffer
	_ = rep.Markdown(&buf, false)
	if !strings.Contains(buf.String(), "could not be parsed") {
		t.Errorf("the report does not warn that per-stage comparison is degraded:\n%s", buf.String())
	}
}

// TestCompare_WebhookDestinationIsComparedAndRedacted is finding 9: the
// url / method / headers of a webhook stage were not part of the
// comparison at all, so `vamp diff` could not answer "did these two runs
// notify the same endpoint?" — and the report printed the credentials
// carried in the inputs verbatim.
func TestCompare_WebhookDestinationIsComparedAndRedacted(t *testing.T) {
	const webhookYAML = `name: notify
stages:
  - id: notify
    type: webhook
    url: "{{.inputs.hook}}"
    headers:
      Authorization: "Bearer {{.inputs.tok}}"
    body:
      text: "done"
    output: notify.json
`
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	rec := basicRec("notify", start, StageRecord{ID: "notify", Status: "ok", DurationMS: 1})
	files := map[string]string{"notify.json": "{}\n"}
	a := writeDiffRun(t, root, "run-a", rec, webhookYAML,
		`{"hook":"https://hooks.slack.com/services/T1/B1/AAAASECRETAAAA","tok":"xoxb-AAAA"}`, files)
	b := writeDiffRun(t, root, "run-b", rec, webhookYAML,
		`{"hook":"https://hooks.slack.com/services/T2/B2/BBBBSECRETBBBB","tok":"xoxb-BBBB"}`, files)

	rep, err := Compare(a, b, CompareOpts{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(rep.Stages) != 1 {
		t.Fatalf("len(Stages) = %d", len(rep.Stages))
	}
	if rep.Stages[0].PromptDiff == "" {
		t.Fatal("two runs that posted to different endpoints produced no diff")
	}
	var human bytes.Buffer
	if err := rep.Markdown(&human, false); err != nil {
		t.Fatal(err)
	}
	var jsonOut bytes.Buffer
	if err := rep.JSON(&jsonOut); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AAAASECRETAAAA", "BBBBSECRETBBBB", "xoxb-AAAA", "xoxb-BBBB"} {
		if strings.Contains(human.String(), secret) {
			t.Errorf("the human report leaked %q:\n%s", secret, human.String())
		}
		if strings.Contains(jsonOut.String(), secret) {
			t.Errorf("the JSON report leaked %q", secret)
		}
	}
	// Redact keeps scheme+host and a stable per-URL id, so the CHANGE is
	// still visible without the credential.
	if !strings.Contains(rep.Stages[0].PromptDiff, "hooks.slack.com") {
		t.Errorf("the diff does not show the destination at all:\n%s", rep.Stages[0].PromptDiff)
	}
	if !strings.Contains(rep.Stages[0].PromptDiff, "id ") {
		t.Errorf("the diff carries no endpoint id, so two different endpoints look the same:\n%s", rep.Stages[0].PromptDiff)
	}
}

// TestRenderBodyMap_RecursesThroughNestedContainers covers the webhook
// body renderer's recursion, which the whole suite had never executed
// (renderBodyMap and renderBodyValue were both at 0.0% statement
// coverage) — while being the ONLY part of a webhook stage `vamp diff`
// looked at. A Slack blocks payload or a Discord embed is nested maps
// inside slices; a renderer that only handled the flat case would leave
// the interesting half as raw templates and every run would "differ".
func TestRenderBodyMap_RecursesThroughNestedContainers(t *testing.T) {
	body := map[string]any{
		"text": "hello {{.name}}",
		"attachments": []any{
			map[string]any{"title": "for {{.name}}", "n": 3},
			"plain {{.name}}",
		},
		"meta":  map[string]any{"inner": map[string]any{"deep": "{{.name}}"}},
		"count": 7,
		"ok":    true,
	}
	got := renderBodyMap(body, func(_, raw string) string {
		return strings.ReplaceAll(raw, "{{.name}}", "otters")
	})
	if got["text"] != "hello otters" {
		t.Errorf("text = %v", got["text"])
	}
	att, ok := got["attachments"].([]any)
	if !ok || len(att) != 2 {
		t.Fatalf("attachments = %#v", got["attachments"])
	}
	first, ok := att[0].(map[string]any)
	if !ok || first["title"] != "for otters" {
		t.Errorf("attachments[0] = %#v", att[0])
	}
	if first["n"] != 3 {
		t.Errorf("a non-string leaf must pass through untouched, got %#v", first["n"])
	}
	if att[1] != "plain otters" {
		t.Errorf("attachments[1] = %#v", att[1])
	}
	inner := got["meta"].(map[string]any)["inner"].(map[string]any)
	if inner["deep"] != "otters" {
		t.Errorf("meta.inner.deep = %#v", inner["deep"])
	}
	if got["count"] != 7 || got["ok"] != true {
		t.Errorf("scalar leaves were altered: %#v / %#v", got["count"], got["ok"])
	}
	if renderBodyMap(nil, func(_, raw string) string { return raw }) != nil {
		t.Error("a nil body must stay nil so the JSON shape does not change")
	}
}

// TestStagePriorOutputsIsCalledFromOnePlace keeps finding 5 fixed. The
// expensive walk (every stage in the pipeline, every stage's output file
// read) is now hoisted into compareStages and PASSED to its consumers.
// Nothing else may call it: the moment a consumer computes its own copy
// again, the amplification comes back as 2N+1 and no unit test notices,
// because the report is byte-identical either way.
//
// This is internal/astscan's shape of check — a syntactic scan with an
// inertness floor and named exemptions — written locally because
// astscan.Rule models "a function that calls X must also call Y", and
// this is "only this function may call X".
func TestStagePriorOutputsIsCalledFromOnePlace(t *testing.T) {
	// name -> why it is allowed to call stagePriorOutputs.
	allowed := map[string]string{
		"compareStages": "the single hoisted call site: it computes one map per run and passes them down",
	}
	callers := callersOf(t, "diff.go", "stagePriorOutputs")
	if len(callers) == 0 {
		t.Fatal("no caller of stagePriorOutputs found in diff.go: this scan is INERT — the function was renamed, moved or deleted")
	}
	for _, fn := range callers {
		if _, ok := allowed[fn]; !ok {
			t.Errorf("%s calls stagePriorOutputs. It walks the whole pipeline and reads every stage's output file; computing it per consumer is what made Compare read each run dir 2N+1 times. Take the map as a parameter, or add an exemption with a reason here.", fn)
		}
	}
	for fn := range allowed {
		found := false
		for _, got := range callers {
			if got == fn {
				found = true
			}
		}
		if !found {
			t.Errorf("exemption %q calls stagePriorOutputs nowhere: it is STALE. Re-point it or delete it — a stale exemption is an unwatched hole", fn)
		}
	}
}

// callersOf returns the names of the functions in file that call fn,
// excluding fn itself (a recursive call is not a second call site).
func callersOf(t *testing.T, file, fn string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Name.Name == fn {
			continue
		}
		calls := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch t := call.Fun.(type) {
			case *ast.Ident:
				if t.Name == fn {
					calls = true
				}
			case *ast.SelectorExpr:
				if t.Sel.Name == fn {
					calls = true
				}
			}
			return true
		})
		if calls {
			out = append(out, fd.Name.Name)
		}
	}
	sort.Strings(out)
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
