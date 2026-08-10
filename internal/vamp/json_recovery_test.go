package vamp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// answerOf decodes a {"answer": "..."} payload and returns the field, so a
// test can assert WHICH of two candidate objects the recovery path picked
// rather than the much weaker "it parsed".
func answerOf(t *testing.T, blob string) string {
	t.Helper()
	var v struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal([]byte(blob), &v); err != nil {
		t.Fatalf("unmarshal %q: %v", blob, err)
	}
	return v.Answer
}

// TestJSONRecovery_ParseJSONAndExtractCleanJSONAgree pins the property the
// two entry points had silently lost: on the same model output they must
// return the same payload, and it must be the model's ANSWER rather than a
// draft it discarded inside a reasoning block.
//
// Before this fix `parseJSON` stripped fences only — never <think> — so the
// template chain the templateFuncs doc comment advertises
// (`readFile … | parseJSON | toJSON`) substituted the reasoning draft for
// the answer, with a nil error, while its sibling on the same bytes returned
// the answer.
func TestJSONRecovery_ParseJSONAndExtractCleanJSONAgree(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "think block at byte 0",
			in:   "<think>\nFirst draft: {\"answer\": \"WRONG\"}\nOn reflection, no.\n</think>\n{\"answer\": \"RIGHT\"}",
			want: "RIGHT",
		},
		{
			// One conversational token in front of the tag is all it took.
			name: "conversational token before the think block",
			in:   "Sure, here goes.\n<think>\nDraft: {\"answer\": \"WRONG\"}\n</think>\n{\"answer\": \"RIGHT\"}",
			want: "RIGHT",
		},
		{
			name: "quoted preamble line before the think block",
			in:   "> restating the task\n<think>{\"answer\":\"WRONG\"}</think>\n{\"answer\":\"RIGHT\"}",
			want: "RIGHT",
		},
		{
			name: "two think blocks",
			in:   "<think>a {\"answer\":\"W1\"}</think>\nhm, again.\n<think>{\"answer\":\"W2\"}</think>\n{\"answer\":\"RIGHT\"}",
			want: "RIGHT",
		},
		{
			name: "think block then fenced answer",
			in:   "<think>{\"answer\":\"WRONG\"}</think>\n```json\n{\"answer\":\"RIGHT\"}\n```",
			want: "RIGHT",
		},
		{
			// Models whose chat template pre-fills the opening tag emit
			// only the closer.
			name: "orphan closing tag",
			in:   "considering {\"answer\":\"WRONG\"}\n</think>\n{\"answer\":\"RIGHT\"}",
			want: "RIGHT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseJSONTemplate(tc.in)
			if err != nil {
				t.Fatalf("parseJSON: %v", err)
			}
			blob, err := toJSONTemplate(v)
			if err != nil {
				t.Fatal(err)
			}
			if got := answerOf(t, blob); got != tc.want {
				t.Errorf("parseJSON picked answer %q, want %q (in=%q)", got, tc.want, tc.in)
			}
			clean, err := extractCleanJSON(tc.in)
			if err != nil {
				t.Fatalf("extractCleanJSON: %v", err)
			}
			if got := answerOf(t, clean); got != tc.want {
				t.Errorf("extractCleanJSON picked answer %q, want %q (in=%q)", got, tc.want, tc.in)
			}
		})
	}
}

// TestJSONRecovery_UnclosedThinkIsNotAnAnswer covers the truncated-generation
// shape: the model opened a reasoning block and the generation stopped before
// it closed one. Everything after the tag is reasoning by construction, so
// there is no answer to hand back — the caller must see an error, not the
// first object it can find inside the model's scratch work.
//
// The direction matters at the two live consumers: the output_format: json
// resume gate re-runs the stage (GPU minutes) and the vision executor fails
// the stage, rather than either of them adopting a draft as the answer.
func TestJSONRecovery_UnclosedThinkIsNotAnAnswer(t *testing.T) {
	const in = "<think>plan: {\"draft\":1}\n{\"final\":2}"
	if got, err := extractCleanJSON(in); err == nil {
		t.Errorf("extractCleanJSON = %q, nil error; an unclosed <think> has no answer in it", got)
	}
	if got, err := parseJSONTemplate(in); err == nil {
		t.Errorf("parseJSON = %#v, nil error; an unclosed <think> has no answer in it", got)
	}
	if got := stripModelArtifacts(in); strings.Contains(got, "draft") {
		t.Errorf("stripModelArtifacts kept reasoning-stream content: %q", got)
	}
	// The resume gate is the consumer where a wrong-but-valid extraction
	// is worst: it would report the stage healthy and skip re-running it.
	if err := validateJSON(in); err == nil {
		t.Error("validateJSON accepted a truncated reasoning stream as a stage's JSON output")
	}
}

// TestJSONRecovery_HostileShapes are the messes a model actually produces,
// driven through both entry points at once. Each asserts WHICH payload came
// back, because "it parsed" is the assertion that let the original defect
// live.
func TestJSONRecovery_HostileShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // toJSON of the recovered value
	}{
		{
			// A JSON document quoting JSON. Nothing may be stripped from
			// it — it parses as-is.
			name: "json block inside a string literal",
			in:   `{"tpl":"{\"a\":1}","fence":"` + "```json" + `"}`,
			want: `{"fence":"` + "```json" + `","tpl":"{\"a\":1}"}`,
		},
		{
			name: "fence inside the think block",
			in:   "<think>```json\n{\"answer\":\"WRONG\"}\n```</think>\n{\"answer\":\"RIGHT\"}",
			want: `{"answer":"RIGHT"}`,
		},
		{
			name: "think block after the payload",
			in:   "{\"answer\":\"RIGHT\"}\n<think>should I revise?</think>",
			want: `{"answer":"RIGHT"}`,
		},
		{
			name: "unbalanced braces after the payload",
			in:   "{\"answer\":\"RIGHT\"}\n\nand then some {{{ chatter",
			want: `{"answer":"RIGHT"}`,
		},
		{
			// Documented, and a sharp edge rather than a bug: "first"
			// means first. Pinned so a future change to the scan has to
			// decide about it on purpose.
			name: "example array before the real one wins",
			in:   "Example: [1,2,3]. Real answer follows:\n[\"real\"]",
			want: `[1,2,3]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseJSONTemplate(tc.in)
			if err != nil {
				t.Fatalf("parseJSON: %v", err)
			}
			got, err := toJSONTemplate(v)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("parseJSON = %s, want %s", got, tc.want)
			}
			clean, err := extractCleanJSON(tc.in)
			if err != nil {
				t.Fatalf("extractCleanJSON: %v", err)
			}
			var reparsed any
			if err := json.Unmarshal([]byte(clean), &reparsed); err != nil {
				t.Fatalf("extractCleanJSON returned unparseable text %q: %v", clean, err)
			}
			blob, err := toJSONTemplate(reparsed)
			if err != nil {
				t.Fatal(err)
			}
			if blob != tc.want {
				t.Errorf("extractCleanJSON = %s, want %s (the two must agree)", blob, tc.want)
			}
		})
	}
}

// TestJSONRecovery_ValidPayloadIsNeverScrubbed is the conservatism half of
// the scan-anywhere change. Stripping <think> wherever it occurs would
// otherwise mangle a well-formed payload that happens to talk ABOUT the
// tags, so a document that already parses is returned untouched.
func TestJSONRecovery_ValidPayloadIsNeverScrubbed(t *testing.T) {
	const in = "{\"note\":\"the model emits <think> and </think> markers\",\"n\":1}"
	got, err := extractCleanJSON(in)
	if err != nil {
		t.Fatalf("extractCleanJSON: %v", err)
	}
	if got != in {
		t.Errorf("extractCleanJSON rewrote a valid payload:\n got %q\nwant %q", got, in)
	}
	v, err := parseJSONTemplate(in)
	if err != nil {
		t.Fatalf("parseJSON: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("parseJSON returned %T, want map", v)
	}
	// Asserted on the decoded value, not on toJSON's output: encoding/json
	// HTML-escapes '<' on the way back out, so the round trip would hide
	// whether the data survived.
	if note, _ := m["note"].(string); !strings.Contains(note, "<think>") {
		t.Errorf("parseJSON scrubbed data out of a valid payload: %q", note)
	}
}

// TestExtractFirstJSONBlock_OpenerSkipsQuotedDelimiters drives the scan that
// FINDS the opening delimiter. The body scan was already string-aware; the
// opener scan was not, while the doc comment claimed both were.
func TestExtractFirstJSONBlock_OpenerSkipsQuotedDelimiters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "quoted brace in the preamble",
			in:   `The key is "{" ok. {"real": true}`,
			want: `{"real": true}`,
		},
		{
			name: "quoted bracket in the preamble",
			in:   "Wrap it in \"[\" and \"]\":\n[1,2]",
			want: "[1,2]",
		},
		{
			name: "escaped quote in the preamble",
			in:   `he said "a \" then {" — anyway: {"real":1}`,
			want: `{"real":1}`,
		},
		{
			// A balanced span that is not JSON must not shadow the payload:
			// markdown link syntax before a JSON answer is an extremely
			// ordinary LLM output shape.
			name: "markdown link before the array",
			in:   "I'll return a **[list](x)** now.\n\n[1,2,3]",
			want: "[1,2,3]",
		},
		{
			// No regression for the string-blind case: an odd number of
			// quotes in the preamble must still recover the payload.
			name: "unbalanced quote in the preamble",
			in:   "Here's the JSON for \"widgets:\n{\"a\":1}",
			want: `{"a":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractFirstJSONBlock(tc.in)
			if !ok {
				t.Fatalf("extractFirstJSONBlock(%q) = ok:false; the payload is present and unambiguous", tc.in)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("returned block is not valid JSON: %q", got)
			}
		})
	}
}

// TestExtractFirstJSONBlock_HostileInputStaysBounded keeps the scan linear.
// The candidate loop only advances past a span it already matched, so an
// input that never closes ends the scan instead of restarting it at every
// opener.
func TestExtractFirstJSONBlock_HostileInputStaysBounded(t *testing.T) {
	if got, ok := extractFirstJSONBlock(strings.Repeat("{", 200_000)); ok {
		t.Errorf("unmatched openers returned %q", got[:min(len(got), 40)])
	}
	if got, ok := extractFirstJSONBlock(strings.Repeat("[", 200_000)); ok {
		t.Errorf("unmatched openers returned %q", got[:min(len(got), 40)])
	}
	big := strings.Repeat("prose prose prose. ", 400_000) + `{"a":1}`
	got, ok := extractFirstJSONBlock(big)
	if !ok || got != `{"a":1}` {
		t.Errorf("got %q ok=%v, want the trailing object", got, ok)
	}
}

// TestMergeJSONTemplate_StripsThinkBlocks: the foreach-merge path saw the
// same reasoning blocks as parseJSON and failed outright on them (decode at
// offset 0), which was the better half of the same defect but still a lost
// run.
func TestMergeJSONTemplate_StripsThinkBlocks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "think preamble",
			in:   "<think>weighing it up</think>\n{\"a\":1}\n{\"b\":2}",
			want: `[{"a":1},{"b":2}]`,
		},
		{
			name: "think block per item",
			in:   "<think>one</think>\n{\"a\":1}\n\n<think>two</think>\n{\"b\":2}",
			want: `[{"a":1},{"b":2}]`,
		},
		{
			name: "conversational token before the think block",
			in:   "Okay.\n<think>one</think>\n{\"a\":1}",
			// The block goes; the prose in front of it is not an
			// artefact this function claims to recognise. Unchanged
			// contract, stated here so the next reader knows it is a
			// choice: mergeJSON surfaces parse errors rather than
			// skipping content, because a dropped item in a foreach
			// merge is invisible downstream.
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeJSONTemplate(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mergeJSON = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergeJSON: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunStageCleanup_KeepsRunMetadata: the run's own provenance is not a
// stage artefact, and a user glob aimed at stage artefacts must not be able
// to delete it. `cleanup: ["*.json"]` is an ordinary thing to write for a
// pipeline whose intermediates are JSON; before this fix it took inputs.json
// with them, permanently, and reported success.
func TestRunStageCleanup_KeepsRunMetadata(t *testing.T) {
	for _, pattern := range []string{"*", "*.json", "vamp.*", "pipeline*"} {
		t.Run(pattern, func(t *testing.T) {
			runDir := t.TempDir()
			meta := []string{
				"pipeline.yaml.snapshot",
				"inputs.json",
				"pipeline.json",
				"pipeline_timing.json",
				LogFileName,
				PidFileName,
			}
			artefacts := []string{"draft.json", "scratch.txt"}
			for _, n := range append(append([]string{}, meta...), artefacts...) {
				if err := os.WriteFile(filepath.Join(runDir, n), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			e := &Executor{RunDir: runDir}
			e.runStageCleanup(&Stage{ID: "s", Cleanup: []string{pattern}})
			for _, n := range meta {
				if _, err := os.Stat(filepath.Join(runDir, n)); err != nil {
					t.Errorf("cleanup %q deleted run metadata %q (%v)", pattern, n, err)
				}
			}
		})
	}
	// The exclusion must not turn cleanup into a no-op: a stage artefact
	// that matches is still removed, and a same-named file in a
	// subdirectory is an artefact, not the run's metadata.
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"draft.json", "sub/inputs.json"} {
		if err := os.WriteFile(filepath.Join(runDir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "inputs.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{RunDir: runDir}
	e.runStageCleanup(&Stage{ID: "s", Cleanup: []string{"*.json", "sub/*.json"}})
	for _, n := range []string{"draft.json", "sub/inputs.json"} {
		if _, err := os.Stat(filepath.Join(runDir, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived cleanup (err=%v); only the run's own metadata is reserved", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "inputs.json")); err != nil {
		t.Errorf("inputs.json deleted (%v)", err)
	}
}
