package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// objectForeachPipeline is the shape ensureUnderRunDir's own comment cites
// as the motivating example for the containment rule: a foreach whose
// items are OBJECTS, with the per-item output path built from a field.
func objectForeachPipeline() *Pipeline {
	return &Pipeline{
		Name: "object_foreach",
		Stages: []Stage{
			{ID: "plan", Capability: "reasoning", Prompt: "list the chapters", OutputFormat: "json", Output: "plan.json"},
			{
				ID: "write", Capability: "reasoning", Inputs: []string{"plan"},
				Foreach: &ForeachSpec{From: "plan", Var: "item"},
				Prompt:  "write chapter {{.item.title}}",
				Output:  "{{.item.slug}}.md",
			},
		},
	}
}

// TestDryRunAcceptsForeachItemsThatAreObjects is finding 2: the dry run
// reported a template error on a CORRECT pipeline.
//
// dryRunForeachStubItems is a list of plain strings, so `{{.item.slug}}`
// — which is a field access, not a map lookup, and therefore not
// something missingkey can rescue — died with `can't evaluate field slug
// in type interface {}`, and dryRunForeachStage turned that into a fatal.
// The stage runs fine under `vamp run`; only --dry-run called it broken,
// and it stopped there, so nothing downstream was checked either.
//
// Before the fix:
//
//	DryRun err = stage write: render output path for item 0: template:
//	write:output:1:13: executing "write:output" at <.item.slug>:
//	can't evaluate field slug in type interface {}
func TestDryRunAcceptsForeachItemsThatAreObjects(t *testing.T) {
	var logBuf bytes.Buffer
	e := &Executor{
		Pipeline:     objectForeachPipeline(),
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	if err := e.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun on a valid object-item foreach failed: %v\nplan:\n%s", err, logBuf.String())
	}
	got := logBuf.String()
	for _, want := range []string{
		// The two per-item output paths must be distinct, or the
		// collision check would have fired on a correct pipeline.
		"slug-1.md",
		"slug-2.md",
		// The prompt renders the other field the template reads.
		"write chapter title-1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan is missing %q.\nplan:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "stub") {
		t.Errorf("the plan must say the item fields are stand-ins.\nplan:\n%s", got)
	}
}

// TestDryRunObjectStubsCoverEveryFieldTheStageReads walks the whole stage
// rather than just the output template: a field read by the prompt, by an
// ffmpeg arg or by a webhook body has to bind too, or the dry run fails
// at a different template instead of the first one.
func TestDryRunObjectStubsCoverEveryFieldTheStageReads(t *testing.T) {
	var logBuf bytes.Buffer
	e := &Executor{
		Pipeline: &Pipeline{
			Name: "deep",
			Stages: []Stage{
				{ID: "plan", Capability: "reasoning", Prompt: "list", OutputFormat: "json", Output: "plan.json"},
				{
					ID: "cut", Type: StageTypeFFmpeg, Inputs: []string{"plan"},
					Foreach:    &ForeachSpec{From: "plan", Var: "shot"},
					FFmpegArgs: []string{"-i", "{{.shot.source}}", "-t", "{{.shot.timing.seconds}}"},
					Output:     "{{.shot.name}}-{{.i}}.mp4",
				},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	if err := e.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun: %v\nplan:\n%s", err, logBuf.String())
	}
	got := logBuf.String()
	for _, want := range []string{"source-1", "seconds-1", "name-1-0.mp4"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan is missing %q (a field the stage reads off the item did not bind).\nplan:\n%s", want, got)
		}
	}
}

// TestDryRunReportsEveryBrokenStageNotJustTheFirst is finding 7: the
// `errors` counter was structurally always zero, because every `s.errors++`
// was immediately followed by a `return err` that unwound past the summary
// line. The design that counter is a fossil of is the one that matters: a
// dry run's job is to find everything wrong in ONE pass, and this one
// stopped at the first problem, so the operator fixed defects one
// invocation at a time.
//
// Three stages, two broken in different ways. The dry run must report
// both, must still fail (exit status is how `--dry-run` says "do not run
// this"), and must still print the summary — which the doc has always
// claimed is "always emitted" and which was not emitted at all on the
// fatal path.
func TestDryRunReportsEveryBrokenStageNotJustTheFirst(t *testing.T) {
	var logBuf bytes.Buffer
	e := &Executor{
		Pipeline: &Pipeline{
			Name: "two_defects",
			Stages: []Stage{
				{ID: "a", Capability: "reasoning", Prompt: "hello {{.inputs.absent}}", Output: "a.md"},
				{ID: "b", Capability: "reasoning", Prompt: "fine", Output: "../escape.md"},
				{ID: "c", Capability: "reasoning", Prompt: "also fine", Output: "c.md"},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		Inputs:       map[string]string{},
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	err := e.DryRun(context.Background())
	if err == nil {
		t.Fatal("DryRun must still fail when a stage cannot be planned")
	}
	got := logBuf.String()
	// Both defects, not just the first.
	if !strings.Contains(got, "absent") {
		t.Errorf("stage a's template error is missing from the plan:\n%s", got)
	}
	if !strings.Contains(got, "escape") {
		t.Errorf("stage b's containment refusal is missing from the plan:\n%s", got)
	}
	// The third stage is fine and must still have been planned.
	if !strings.Contains(got, `stage "c"`) {
		t.Errorf("the dry run stopped before the last stage:\n%s", got)
	}
	// The summary is emitted, and its error count is no longer a
	// structurally-dead zero.
	if !strings.Contains(got, "dry-run: 2 errors") {
		t.Errorf("expected the summary to count both errors:\n%s", got)
	}
	// The returned error still names both, so a caller that only reads
	// the error learns as much as one reading the plan.
	for _, want := range []string{"absent", "escape"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("returned error does not mention %q: %v", want, err)
		}
	}
}

// TestDryRunHeaderAnnotatesRunWhen: the real scheduler skips a
// `run_when: failure` stage on a successful run, and the plan used to
// print it exactly like a stage that always runs.
func TestDryRunHeaderAnnotatesRunWhen(t *testing.T) {
	var logBuf bytes.Buffer
	e := &Executor{
		Pipeline: &Pipeline{
			Name: "conditional",
			Stages: []Stage{
				{ID: "always", Capability: "reasoning", Prompt: "x", Output: "a.md"},
				{ID: "cleanup", Capability: "reasoning", Prompt: "y", Output: "b.md", RunWhen: RunWhenFailure},
				{ID: "tmpl", Capability: "reasoning", Prompt: "z", Output: "c.md", RunWhen: `{{ contains .stages.always.output "x" }}`, Inputs: []string{"always"}},
			},
		},
		Capabilities: &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}},
		RunDir:       t.TempDir(),
		Log:          &logBuf,
	}
	if err := e.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	got := logBuf.String()
	if !strings.Contains(got, "[run_when: failure") {
		t.Errorf("the plan does not say the cleanup stage is conditional:\n%s", got)
	}
	if !strings.Contains(got, "evaluated at run time") {
		t.Errorf("the plan does not flag the template-form gate:\n%s", got)
	}
	// The unconditional stage must NOT be annotated — noise on every
	// stage is the same as no annotation.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, `stage "always"`) && strings.Contains(line, "run_when") {
			t.Errorf("an unconditional stage should carry no run_when annotation: %s", line)
		}
	}
}

// TestDryRunWebhookPreviewRedactsTheURL: a webhook stage had no per-type
// preview at all, so `--dry-run` said nothing about where a notification
// goes. It now says — with the destination redacted the same way the
// executor's own log line redacts it, because an incoming-webhook URL
// carries its bearer in the path and a dry-run plan is a thing people
// paste into bug reports.
func TestDryRunWebhookPreviewRedactsTheURL(t *testing.T) {
	const secretURL = "https://hooks.slack.com/services/T1/B1/AAAASECRETAAAA"
	var logBuf bytes.Buffer
	e := &Executor{
		Pipeline: &Pipeline{
			Name: "notify",
			Stages: []Stage{{
				ID: "notify", Type: StageTypeWebhook,
				URL:     "{{.inputs.hook}}",
				Method:  "post",
				Headers: map[string]string{"Authorization": "Bearer {{.inputs.tok}}"},
				Body:    map[string]any{"text": "{{.pipeline_name}} finished"},
				Output:  "notify.json",
			}},
		},
		Inputs: map[string]string{"hook": secretURL, "tok": "xoxb-AAAA"},
		RunDir: t.TempDir(),
		Log:    &logBuf,
	}
	if err := e.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	got := logBuf.String()
	for _, forbidden := range []string{"AAAASECRETAAAA", "xoxb-AAAA"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the plan leaked %q:\n%s", forbidden, got)
		}
	}
	for _, want := range []string{
		"POST https://hooks.slack.com/...",
		"headers: Authorization",
		"notify finished", // the body rendered, with .pipeline_name bound
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the plan is missing %q:\n%s", want, got)
		}
	}
}

// TestDryRunMatchesTheRealRunOnEveryStagePath is the differential: the
// same pipeline through DryRun and through a real Run with stubbed
// inference/subprocess boundaries, comparing what each CLAIMS about the
// output paths. A dry run's whole job is to tell the operator what the
// real run will do; the two agreeing on the set of artefacts is the
// cheapest honest statement of that.
func TestDryRunMatchesTheRealRunOnEveryStagePath(t *testing.T) {
	pipeline := objectForeachPipeline()
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}

	// --- the real run, against a stubbed vibe + chat-completions server
	// that answers the plan stage with the item objects.
	stub := &stubControl{}
	mux := http.NewServeMux()
	cpath, handler := vibev1connect.NewControlServiceHandler(stub)
	mux.Handle(cpath, handler)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "stub-model"}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		prompt := ""
		if len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]any); ok {
				prompt, _ = m["content"].(string)
			}
		}
		answer := "chapter body"
		if strings.Contains(prompt, "list the chapters") {
			// Deliberately NOT the shape of the dry run's stub values:
			// the two must agree about the artefacts a run produces, not
			// about what the model happens to say.
			answer = `[{"slug":"alpha","title":"Alpha"},{"slug":"beta","title":"Beta"}]`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": answer}}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stub.proxyURL = srv.URL

	realDir := t.TempDir()
	real := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		Vibe:         vibeclient.NewWithHTTPClient(srv.URL, srv.Client(), ""),
		RunDir:       realDir,
	}
	if err := real.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	realFiles := relFiles(t, realDir)
	if len(realFiles) == 0 {
		t.Fatal("the real run produced no artefacts, so this differential proves nothing")
	}

	// --- the dry run, whose stub items are synthesised from the very
	// templates the real items fed.
	var plan bytes.Buffer
	dry := &Executor{
		Pipeline:     pipeline,
		Capabilities: caps,
		RunDir:       t.TempDir(),
		Log:          &plan,
		Inference: func(_ context.Context, _, _, _ string, _ map[string]any, _ StreamFunc) (string, error) {
			t.Error("inference must not fire during a dry run")
			return "", nil
		},
	}
	if err := dry.DryRun(context.Background()); err != nil {
		t.Fatalf("DryRun: %v\nplan:\n%s", err, plan.String())
	}
	// The two agree about the SHAPE of what the run produces: how many
	// artefacts, with which extensions, and the exactly-named ones
	// verbatim. The per-item names cannot match — the real ones come from
	// the model — and pretending otherwise would be a test that proves
	// only that the fixture agrees with itself.
	planned := plannedOutputs(plan.String())
	if len(planned) != len(realFiles) {
		t.Fatalf("the plan lists %d output(s) and the real run produced %d.\nplanned: %v\nreal:    %v\nplan:\n%s",
			len(planned), len(realFiles), planned, realFiles, plan.String())
	}
	if got, want := extCount(planned), extCount(realFiles); !reflect.DeepEqual(got, want) {
		t.Errorf("the plan and the real run disagree about output shapes: planned %v, real %v", got, want)
	}
	// plan.json has a fixed name in the yaml, so this one IS comparable
	// verbatim — and it is the one a wrong dry run would drop.
	if !strings.Contains(plan.String(), "plan.json") {
		t.Errorf("the plan never named plan.json:\n%s", plan.String())
	}
}

// plannedOutputs pulls the `output: <path>` lines out of a dry-run plan.
func plannedOutputs(plan string) []string {
	var out []string
	for _, line := range strings.Split(plan, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "output: "); ok {
			out = append(out, strings.TrimSpace(rest))
		}
	}
	sort.Strings(out)
	return out
}

// extCount counts paths by extension, which is the part of "what will this
// run produce" that a dry run can honestly predict.
func extCount(paths []string) map[string]int {
	out := map[string]int{}
	for _, p := range paths {
		out[filepath.Ext(p)]++
	}
	return out
}

// relFiles lists the paths a run actually wrote, relative to its run dir,
// excluding vamp's own bookkeeping artefacts.
func relFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	bookkeeping := map[string]bool{
		"pipeline.json": true, "inputs.json": true, "pipeline.yaml.snapshot": true,
		"run.log": true, "timing.json": true, "pipeline_timing.json": true,
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if bookkeeping[rel] || strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestStageMarshalsForStubSynthesis pins the one assumption
// dryRunItemFieldPaths makes about Stage: that it can be JSON-marshaled
// (with the fs.FS field cleared) so the scan sees every template on it,
// including ones added later. A template-bearing field that a hand-kept
// list forgot is the same defect class as a switch arm that a hand-kept
// list forgot.
func TestStageMarshalsForStubSynthesis(t *testing.T) {
	st := &Stage{ID: "s", Prompt: "{{.item.a}}", Output: "{{.item.b}}.md", AssetFS: os.DirFS(t.TempDir())}
	cp := *st
	cp.AssetFS = nil
	if _, err := json.Marshal(&cp); err != nil {
		t.Fatalf("Stage must stay JSON-marshalable for the stub-field scan: %v", err)
	}
	st.Foreach = &ForeachSpec{From: "up", Var: "item"}
	got := dryRunItemFieldPaths(st)
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("dryRunItemFieldPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dryRunItemFieldPaths = %v, want %v", got, want)
		}
	}
}
