package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestPipelineCandidates_XDG_AndLocal drops two pipeline-ish yaml files
// in an "XDG" dir and one in a "local" dir and confirms the completer
// returns the union with the expected prefixes — full path for XDG hits,
// "./<name>" for local hits — and ignores non-yaml siblings.
func TestPipelineCandidates_XDG_AndLocal(t *testing.T) {
	xdg := t.TempDir()
	local := t.TempDir()

	must := func(path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("name: stub\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(xdg, "one.yaml"))
	must(filepath.Join(xdg, "two.yaml"))
	must(filepath.Join(xdg, "README.md")) // ignored
	must(filepath.Join(local, "pipeline.yaml"))
	must(filepath.Join(local, "notes.txt")) // ignored

	got := pipelineCandidates(xdg, local)
	want := []string{
		"./pipeline.yaml",
		filepath.Join(xdg, "one.yaml"),
		filepath.Join(xdg, "two.yaml"),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pipelineCandidates() = %v, want %v", got, want)
	}
}

// TestPipelineCandidates_MissingDirs verifies both sources are optional:
// nothing exists, nothing is returned, no error.
func TestPipelineCandidates_MissingDirs(t *testing.T) {
	got := pipelineCandidates(
		filepath.Join(t.TempDir(), "no-xdg"),
		filepath.Join(t.TempDir(), "no-local"),
	)
	if len(got) != 0 {
		t.Errorf("expected empty slice for missing dirs, got %v", got)
	}
}

// completionTestPipeline is a minimal valid pipeline with one text stage
// and one confirm stage, so the stage-id completers have both a full list
// and a type-filtered list to exercise.
const completionTestPipeline = `name: demo
stages:
  - id: draft
    type: text
    capability: code
    prompt: "hi"
    output: draft.txt
  - id: approve
    type: confirm
    message: "ok?"
    output: approve.txt
`

// setupRunsDir points $XDG_STATE_HOME at a temp dir and creates one run
// dir per id (newest ids should sort first via their timestamps).
func setupRunsDir(t *testing.T, ids ...string) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	runsDir := filepath.Join(state, "vamp", "runs")
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(runsDir, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return runsDir
}

// TestCompleteRunIDsUpTo verifies the diff-shaped completer fills both
// slots, drops an id already consumed by slot 0, and that the single-slot
// completeRunIDs still refuses the second slot.
func TestCompleteRunIDsUpTo(t *testing.T) {
	idA := "2026-07-12T10-00-00_alpha"
	idB := "2026-07-12T09-00-00_beta"
	setupRunsDir(t, idA, idB)

	got, _ := completeRunIDsUpTo(1)(nil, nil, "")
	if want := []string{idA, idB}; !reflect.DeepEqual(got, want) {
		t.Errorf("slot 0 = %v, want %v", got, want)
	}
	got, _ = completeRunIDsUpTo(1)(nil, []string{idA}, "")
	if want := []string{idB}; !reflect.DeepEqual(got, want) {
		t.Errorf("slot 1 = %v, want %v (args[0] must be filtered out)", got, want)
	}
	if got, _ := completeRunIDs(nil, []string{idA}, ""); got != nil {
		t.Errorf("completeRunIDs past slot 0 = %v, want nil", got)
	}
}

// TestCompleteRenderArgs_StageIDs verifies the second render slot offers
// every stage id from the pipeline named in the first slot, and degrades
// to no suggestions when the pipeline doesn't load.
func TestCompleteRenderArgs_StageIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(path, []byte(completionTestPipeline), 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := completeRenderArgs(nil, []string{path}, "")
	if want := []string{"draft", "approve"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stage ids = %v, want %v", got, want)
	}
	if got, _ := completeRenderArgs(nil, []string{filepath.Join(dir, "missing.yaml")}, ""); got != nil {
		t.Errorf("unloadable pipeline = %v, want nil", got)
	}
	if got, _ := completeRenderArgs(nil, []string{path, "draft"}, ""); got != nil {
		t.Errorf("past slot 1 = %v, want nil", got)
	}
}

// TestCompleteConfirmArgs_ConfirmStagesOnly verifies the second confirm
// slot resolves the run, reads pipeline.yaml.snapshot, and offers only the
// `type: confirm` stage ids; a run without a snapshot yields nothing.
func TestCompleteConfirmArgs_ConfirmStagesOnly(t *testing.T) {
	id := "2026-07-12T10-00-00_demo"
	bare := "2026-07-12T09-00-00_bare"
	runsDir := setupRunsDir(t, id, bare)
	if err := os.WriteFile(filepath.Join(runsDir, id, "pipeline.yaml.snapshot"), []byte(completionTestPipeline), 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := completeConfirmArgs(nil, []string{id}, "")
	if want := []string{"approve"}; !reflect.DeepEqual(got, want) {
		t.Errorf("confirm stage ids = %v, want %v", got, want)
	}
	if got, _ := completeConfirmArgs(nil, []string{bare}, ""); got != nil {
		t.Errorf("run without snapshot = %v, want nil", got)
	}
}
