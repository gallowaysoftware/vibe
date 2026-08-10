package vamp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// resumeForeachExecutor builds the minimum Executor tryResumeForeachStage
// needs: a run dir to read from and a pre-seeded upstream output for
// resolveForeachItems to parse. No server, no registry — the pre-pass is a
// pure function of the run dir plus stageOutputs, which is exactly why it
// is worth testing directly.
func resumeForeachExecutor(t *testing.T, upstream string) (*Executor, string) {
	t.Helper()
	runDir := t.TempDir()
	return &Executor{
		RunDir:          runDir,
		stageOutputs:    map[string]*stageResult{"titles": {Output: upstream}},
		foreachResumes:  map[string]*foreachResumeInfo{},
		completedStages: map[string]bool{"titles": true},
	}, runDir
}

// writeItem writes one per-item output file under the run dir.
func writeItem(t *testing.T, runDir, rel, body string) {
	t.Helper()
	full := filepath.Join(runDir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTryResumeForeach_ClassifiesByOutputKindNotAnAllowlist is finding 1
// of the resume review, and it is the same defect the single-stage path
// was already fixed for — with a comment that ends "Classify by output
// kind, not an allowlist that forgot compact."
//
// Of the six content-bearing stage types the foreach pre-pass's allowlist
// named two. render / compact / webhook / confirm fell to the binary
// branch and resumed with an ABSOLUTE PATH where the next stage's prompt
// expects the file's bytes: the model is handed "/home/you/.local/state/
// vamp/runs/…/items/a.txt" and generates from memory, the run exits 0.
// (It also leaks the host's directory layout into a model request.)
//
// The assertion is per stage type, both directions, because a fix that
// flipped the classification the other way would be just as wrong.
func TestTryResumeForeach_ClassifiesByOutputKindNotAnAllowlist(t *testing.T) {
	for _, tc := range []struct {
		typ    StageType
		binary bool
	}{
		{StageTypeText, false},
		{StageTypeYouTube, false},
		{StageTypeRender, false},
		{StageTypeCompact, false},
		{StageTypeWebhook, false},
		{StageTypeConfirm, false},
		{StageTypeComfyUI, true},
		{StageTypeAudio, true},
		{StageTypeFFmpeg, true},
		{StageTypePandoc, true},
		{StageTypeMix, true},
		{StageTypeShort, true},
	} {
		t.Run(string(tc.typ), func(t *testing.T) {
			e, runDir := resumeForeachExecutor(t, `["a","b"]`)
			writeItem(t, runDir, "items/a.txt", "BODY-A")
			writeItem(t, runDir, "items/b.txt", "BODY-B")
			st := &Stage{
				ID: "consumer", Type: tc.typ,
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Output:  "items/{{.title}}.txt",
			}
			info, err := e.tryResumeForeachStage(st)
			if err != nil {
				t.Fatalf("tryResumeForeachStage: %v", err)
			}
			if info == nil || len(info.MissingIndices) != 0 {
				t.Fatalf("both item files exist; want a complete resume, got %+v", info)
			}
			got := e.stageOutputs["consumer"].Outputs
			want := []string{"BODY-A", "BODY-B"}
			if tc.binary {
				want = []string{filepath.Join(runDir, "items/a.txt"), filepath.Join(runDir, "items/b.txt")}
			}
			if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				kind := "the file's CONTENT"
				if tc.binary {
					kind = "an absolute PATH"
				}
				t.Errorf("resumed outputs = %q, want %q (%s stages resume with %s)", got, want, tc.typ, kind)
			}
		})
	}
}

// TestTryResumeForeach_JSONRevalidationCoversEveryContentType is guard G1
// of finding 8: the output_format: json re-check existed but sat INSIDE
// the two-type allowlist, so it protected text and youtube and nothing
// else. A truncated JSON body from the crashed prior run satisfies
// "size > 0" and poisons every downstream consumer that parses it.
func TestTryResumeForeach_JSONRevalidationCoversEveryContentType(t *testing.T) {
	for _, typ := range []StageType{StageTypeText, StageTypeRender, StageTypeCompact} {
		t.Run(string(typ), func(t *testing.T) {
			e, runDir := resumeForeachExecutor(t, `["a","b"]`)
			writeItem(t, runDir, "items/a.txt", `{"ok":true}`)
			writeItem(t, runDir, "items/b.txt", `{"truncated":`)
			st := &Stage{
				ID: "consumer", Type: typ,
				Inputs:       []string{"titles"},
				Foreach:      &ForeachSpec{From: "titles", Var: "title"},
				Output:       "items/{{.title}}.txt",
				OutputFormat: "json",
			}
			info, err := e.tryResumeForeachStage(st)
			if err != nil {
				t.Fatalf("tryResumeForeachStage: %v", err)
			}
			if info == nil {
				t.Fatal("pre-pass declined entirely; want item 1 reported missing")
			}
			if len(info.MissingIndices) != 1 || info.MissingIndices[0] != 1 {
				t.Errorf("MissingIndices = %v, want [1]: a truncated JSON item is not a completed item", info.MissingIndices)
			}
			if _, ok := info.ResumedOutputs[1]; ok {
				t.Error("the truncated item was resumed as a completed result")
			}
		})
	}
}

// TestTryResumeStage_JSONRevalidation is guard G2: the same re-check on
// the single-stage path. It has always been correct and has never had a
// test — the delete-the-guard run left the suite green.
func TestTryResumeStage_JSONRevalidation(t *testing.T) {
	runDir := t.TempDir()
	e := &Executor{RunDir: runDir, stageOutputs: map[string]*stageResult{}}
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a",`), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &Stage{ID: "titles", Output: "titles.json", OutputFormat: "json"}
	ok, err := e.tryResumeStage(st)
	if err != nil {
		t.Fatalf("tryResumeStage: %v", err)
	}
	if ok {
		t.Fatal("a truncated JSON output resumed as a completed stage")
	}
	if _, seeded := e.stageOutputs["titles"]; seeded {
		t.Error("the corrupted body was seeded into stageOutputs anyway")
	}
}

// TestReadNonEmpty_ZeroByteIsNotAResult is guard G3 of finding 8: the
// rule readNonEmpty is NAMED for. Deleting the Size() == 0 branch left
// every test in the package green, which means the difference between
// "the prior run wrote this" and "the prior run created the file and
// died" was resting on nothing.
//
// Both call paths assert it, because the guard is one function and two
// consumers and the untested one is how it stops being true.
func TestReadNonEmpty_ZeroByteIsNotAResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if body, ok, err := readNonEmpty(filepath.Join(dir, "empty.txt")); ok || err != nil || body != nil {
		t.Errorf("readNonEmpty(zero-byte) = %q, %v, %v; a zero-byte file is a crashed write, not a result", body, ok, err)
	}
	if body, ok, err := readNonEmpty(filepath.Join(dir, "absent.txt")); ok || err != nil || body != nil {
		t.Errorf("readNonEmpty(missing) = %q, %v, %v; want nil, false, nil", body, ok, err)
	}
	if body, ok, err := readNonEmpty(filepath.Join(dir, "empty.txt")); ok || err != nil || body != nil {
		t.Errorf("readNonEmpty(zero-byte) second read = %q, %v, %v", body, ok, err)
	}

	t.Run("single-stage resume reruns it", func(t *testing.T) {
		runDir := t.TempDir()
		e := &Executor{RunDir: runDir, stageOutputs: map[string]*stageResult{}}
		if err := os.WriteFile(filepath.Join(runDir, "a.txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		ok, err := e.tryResumeStage(&Stage{ID: "a", Output: "a.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("a zero-byte output resumed as a completed stage")
		}
	})

	t.Run("foreach resume reruns that item", func(t *testing.T) {
		e, runDir := resumeForeachExecutor(t, `["a","b"]`)
		writeItem(t, runDir, "items/a.txt", "BODY-A")
		writeItem(t, runDir, "items/b.txt", "")
		info, err := e.tryResumeForeachStage(&Stage{
			ID: "consumer", Inputs: []string{"titles"},
			Foreach: &ForeachSpec{From: "titles", Var: "title"},
			Output:  "items/{{.title}}.txt",
		})
		if err != nil {
			t.Fatal(err)
		}
		if info == nil || len(info.MissingIndices) != 1 || info.MissingIndices[0] != 1 {
			t.Errorf("MissingIndices = %+v, want [1]; the zero-byte item must rerun", info)
		}
	})
}

// TestTryResumeForeachStage_SurfacesRunDirEscape is guard G5 of finding
// 8, and the pointed one: errOutputPathEscape is a SECURITY refusal — a
// model-authored item field interpolated into the output template,
// rendering "/etc/passwd", read back into the next prompt. The
// non-foreach half of that refusal is covered by
// TestTryResumeStage_SurfacesRunDirEscape and is in the mutation
// registry; the foreach half, added in the same change with a five-line
// comment explaining why it matters, was covered by nothing.
func TestTryResumeForeachStage_SurfacesRunDirEscape(t *testing.T) {
	e, _ := resumeForeachExecutor(t, `["../escaped"]`)
	info, err := e.tryResumeForeachStage(&Stage{
		ID: "consumer", Inputs: []string{"titles"},
		Foreach: &ForeachSpec{From: "titles", Var: "title"},
		Output:  "{{.title}}.txt",
	})
	if err == nil {
		t.Fatalf("tryResumeForeachStage = %+v, nil; an item whose path leaves the run dir is a refusal, not 'nothing to resume'", info)
	}
	if !errors.Is(err, errOutputPathEscape) {
		t.Errorf("error = %v; want it to wrap errOutputPathEscape", err)
	}
	if info != nil {
		t.Error("resume info returned alongside the refusal")
	}
}

// collidingForeachPipeline fans out over two titles that slugify to the
// SAME file name. The fresh run refuses it at exec.go's seenPaths check;
// the question these tests ask is whether resume can suppress that.
func collidingForeachPipeline() *Pipeline {
	return &Pipeline{
		Name: "fan",
		Stages: []Stage{
			{ID: "titles", Capability: "reasoning", Prompt: "TITLES", Output: "titles.json", OutputFormat: "json"},
			{
				ID: "consumer", Capability: "reasoning",
				Inputs:  []string{"titles"},
				Foreach: &ForeachSpec{From: "titles", Var: "title"},
				Prompt:  "UP:{{.title}}",
				Output:  "items/{{.title | slugify}}.txt",
			},
		},
	}
}

// twoTitleInfFn is the inference stub for the collision / non-templated
// tests: the upstream emits the given JSON array verbatim and each item
// uppercases.
func twoTitleInfFn(items string, calls *sync.Map) InferenceFunc {
	return func(_ context.Context, _, _, prompt string, _ map[string]any, _ StreamFunc) (string, error) {
		if prompt == "TITLES" {
			return items, nil
		}
		if strings.HasPrefix(prompt, "UP:") {
			tok := strings.TrimPrefix(prompt, "UP:")
			calls.Store(tok, true)
			return strings.ToUpper(tok), nil
		}
		return "", fmt.Errorf("unexpected prompt %q", prompt)
	}
}

// seedResumeDir writes the pipeline snapshot + inputs record a resume run
// expects to find, so the drift checks pass and the test is about the
// thing it names.
func seedResumeDir(t *testing.T, e *Executor, runDir, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runDir, "pipeline.yaml.snapshot"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "inputs.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.PipelineSource = []byte(source)
	e.ResumeDir = runDir
}

// TestResumeForeach_CollisionStaysRefused is finding 2. executeForeachStage
// refuses two items that render to the same output path — one body would
// be written twice and N-1 items would silently be copies of one. Resume
// rendered the same paths in the same loop with no such check, so when
// the colliding file was present BOTH items "resumed" from it, the stage
// was marked complete, and the error could never fire: err = nil,
// Outputs = ["ONE-BODY","ONE-BODY"].
//
// The guard is on the path resume prevents you from reaching, which is
// the repo's "sender guards, receiver doesn't" class with the ends
// swapped.
func TestResumeForeach_CollisionStaysRefused(t *testing.T) {
	const source = "name: fan\n"
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	var calls sync.Map
	exec, runDir := stubExecutor(t, collidingForeachPipeline(), caps, twoTitleInfFn(`["Foo Bar","foo-bar"]`, &calls))
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["Foo Bar","foo-bar"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeItem(t, runDir, "items/foo-bar.txt", "ONE-BODY")
	seedResumeDir(t, exec, runDir, source)

	err := exec.Run(context.Background())
	if err == nil {
		exec.mu.Lock()
		got := exec.stageOutputs["consumer"]
		exec.mu.Unlock()
		t.Fatalf("resume accepted a colliding foreach the fresh run refuses; Outputs = %q", got.Outputs)
	}
	if !strings.Contains(err.Error(), "foreach output path collision") {
		t.Errorf("error = %v; want the same collision message a fresh run produces", err)
	}
}

// TestResumeForeach_NonTemplatedOutputStaysRefused is finding 3: the
// other runtime refusal resume could suppress. pipeline.go moved this
// check OUT of Validate deliberately (a single-item foreach with a static
// output is legal), so the pipeline validates and only the runtime says
// no — and resume skipped the runtime.
//
// The reachable shape: a 1-item run succeeded, the upstream's list later
// grew, the operator resumed. The error the check exists to raise is then
// permanently suppressed for that run dir.
func TestResumeForeach_NonTemplatedOutputStaysRefused(t *testing.T) {
	const source = "name: fan\n"
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := collidingForeachPipeline()
	pipeline.Stages[1].Output = "items/only.txt"
	var calls sync.Map
	exec, runDir := stubExecutor(t, pipeline, caps, twoTitleInfFn(`["a","b"]`, &calls))
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeItem(t, runDir, "items/only.txt", "ONE-BODY")
	seedResumeDir(t, exec, runDir, source)

	err := exec.Run(context.Background())
	if err == nil {
		exec.mu.Lock()
		got := exec.stageOutputs["consumer"]
		exec.mu.Unlock()
		t.Fatalf("resume accepted a non-templated multi-item foreach the fresh run refuses; Outputs = %q", got.Outputs)
	}
	if !strings.Contains(err.Error(), "templated output path") {
		t.Errorf("error = %v; want the same non-templated message a fresh run produces", err)
	}
}

// TestResumeForeach_IndexPathDoesNotPairByPositionAcrossADifferentList is
// finding 4. When the output template is index-derived
// (assets/img_{{.i}}.png — a pattern the code's own comment advertises)
// the pre-pass's only pairing key is the position, and nothing on disk
// records which item produced items/0.txt.
//
// The compound precondition, stated honestly: an index-only template AND
// an upstream that re-resolves to a different list on the resume. The
// second half is not the common case — when the upstream's own file
// survives, the list is byte-identical and positional pairing is exactly
// right. It becomes reachable when the upstream's output is the thing
// that got corrupted, which is also the most likely reason someone is
// resuming.
//
// Before the fix: prior run over [a b c d], upstream re-runs and returns
// [w x y z], resume yields ["A-prior","B-prior","Y","Z"] and exits 0 —
// item w carrying a body generated for item a.
func TestResumeForeach_IndexPathDoesNotPairByPositionAcrossADifferentList(t *testing.T) {
	const source = "name: fan\n"
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := collidingForeachPipeline()
	pipeline.Stages[1].Output = "items/{{.i}}.txt"
	var calls sync.Map
	// titles.json is deliberately ABSENT, so the upstream cannot resume
	// and re-runs against a model that has moved on.
	exec, runDir := stubExecutor(t, pipeline, caps, twoTitleInfFn(`["w","x","y","z"]`, &calls))
	writeItem(t, runDir, "items/0.txt", "A-prior")
	writeItem(t, runDir, "items/1.txt", "B-prior")
	seedResumeDir(t, exec, runDir, source)

	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	exec.mu.Lock()
	got := exec.stageOutputs["consumer"].Outputs
	exec.mu.Unlock()
	for i, body := range got {
		if strings.HasSuffix(body, "-prior") {
			t.Errorf("Outputs[%d] = %q for item %q — a body generated for a DIFFERENT item was reused; the on-disk file records only a position", i, body, []string{"w", "x", "y", "z"}[i])
		}
	}
	var ran []string
	calls.Range(func(k, _ any) bool { ran = append(ran, fmt.Sprint(k)); return true })
	sort.Strings(ran)
	if strings.Join(ran, ",") != "w,x,y,z" {
		t.Errorf("items executed = %v, want every item of the NEW list: the prior run's files describe the old one", ran)
	}
}

// TestResumeForeach_IndexPathStillResumesWhenTheUpstreamResumed is the
// other half of the same rule, and the reason the guard is compound
// rather than a blanket refusal of index-templated paths. When the
// upstream itself resumed from disk, its output is byte-for-byte the
// prior run's, so the item list IS the list that produced the files and
// positional pairing is sound. Refusing here would re-render every image
// of a 50-item comfyui fan-out that crashed on image 47 — the exact work
// --resume exists to avoid.
func TestResumeForeach_IndexPathStillResumesWhenTheUpstreamResumed(t *testing.T) {
	const source = "name: fan\n"
	caps := &Capabilities{Mapping: map[string]CapabilityBinding{"reasoning": {Profile: "code"}}}
	pipeline := collidingForeachPipeline()
	pipeline.Stages[1].Output = "items/{{.i}}.txt"
	var calls sync.Map
	exec, runDir := stubExecutor(t, pipeline, caps, twoTitleInfFn(`["w","x","y","z"]`, &calls))
	// The upstream's own output SURVIVED, so it resumes and the item
	// list is the prior run's list.
	if err := os.WriteFile(filepath.Join(runDir, "titles.json"), []byte(`["a","b","c","d"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeItem(t, runDir, "items/0.txt", "A-prior")
	writeItem(t, runDir, "items/1.txt", "B-prior")
	seedResumeDir(t, exec, runDir, source)

	if err := exec.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	exec.mu.Lock()
	got := exec.stageOutputs["consumer"].Outputs
	exec.mu.Unlock()
	if len(got) != 4 || got[0] != "A-prior" || got[1] != "B-prior" {
		t.Errorf("Outputs = %q; the first two items must still resume from disk when the upstream resumed", got)
	}
	for _, tok := range []string{"a", "b"} {
		if _, ran := calls.Load(tok); ran {
			t.Errorf("item %q was re-run despite its output existing and the item list being unchanged", tok)
		}
	}
}

// TestCheckResumeSnapshot_InputsAreDrift is finding 4 of the JSON/run-dir
// review. The drift hash covered the pipeline file and nothing else, so
// `vamp run --input topic=cats` then `--resume --input topic=dogs` was
// accepted: half the run is about cats, and snapshot() then REWROTE
// inputs.json to "dogs" — destroying the only on-disk record of what the
// already-complete stages were generated from. `runs ls` and `vamp diff`
// both read that file.
func TestCheckResumeSnapshot_InputsAreDrift(t *testing.T) {
	const source = "name: p\n"
	newExec := func(t *testing.T, inputs map[string]string, recorded string) *Executor {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "pipeline.yaml.snapshot"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if recorded != "" {
			if err := os.WriteFile(filepath.Join(dir, "inputs.json"), []byte(recorded), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &Executor{RunDir: dir, ResumeDir: dir, PipelineSource: []byte(source), Inputs: inputs}
	}

	t.Run("changed value is refused", func(t *testing.T) {
		e := newExec(t, map[string]string{"topic": "dogs"}, `{"topic":"cats"}`)
		err := e.checkResumeSnapshot()
		if err == nil {
			t.Fatal("resume accepted a changed --input; the completed stages were generated from the old one")
		}
		for _, want := range []string{"topic", "resume-force"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name %q so the operator can see WHAT changed: %v", want, err)
			}
		}
	})
	t.Run("unchanged inputs resume", func(t *testing.T) {
		e := newExec(t, map[string]string{"topic": "cats"}, `{"topic":"cats"}`)
		if err := e.checkResumeSnapshot(); err != nil {
			t.Errorf("identical inputs must resume: %v", err)
		}
	})
	t.Run("added input is refused", func(t *testing.T) {
		e := newExec(t, map[string]string{"topic": "cats", "voice": "amy"}, `{"topic":"cats"}`)
		if err := e.checkResumeSnapshot(); err == nil {
			t.Error("resume accepted an input the original run never had")
		}
	})
	t.Run("resume-force overrides", func(t *testing.T) {
		e := newExec(t, map[string]string{"topic": "dogs"}, `{"topic":"cats"}`)
		e.ResumeForce = true
		if err := e.checkResumeSnapshot(); err != nil {
			t.Errorf("--resume-force is the documented override for drift: %v", err)
		}
	})
	t.Run("legacy run dir without inputs.json still resumes", func(t *testing.T) {
		e := newExec(t, nil, "")
		if err := e.checkResumeSnapshot(); err != nil {
			t.Errorf("a run dir written before inputs were recorded must stay resumable: %v", err)
		}
	})
}

// TestSnapshot_ResumeKeepsThePriorRunsInputsRecord is the other half of
// the same finding: even once drift is refused, --resume-force still
// reaches snapshot(), and an unconditional rewrite of inputs.json would
// replace the record of what produced the already-complete stages. The
// pipeline snapshot already has exactly this carve-out; inputs did not.
func TestSnapshot_ResumeKeepsThePriorRunsInputsRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inputs.json"), []byte(`{"topic":"cats"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{RunDir: dir, ResumeDir: dir, Inputs: map[string]string{"topic": "dogs"}}
	if err := e.snapshot(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "cats") {
		t.Errorf("inputs.json = %s; the resumed run overwrote the record of what the completed stages were generated from", got)
	}
}
