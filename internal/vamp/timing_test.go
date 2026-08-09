package vamp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTracker_SequentialStagesRecordDurations verifies that two stages run
// back-to-back appear in the Report in start order and their durations sum to
// roughly the total wall time (with a generous slack to keep the test stable
// on slow CI). We don't assert exact ms because timer precision is coarse;
// we only need "stage A is shorter than stage B" and "total >= sum-of-stages".
func TestTracker_SequentialStagesRecordDurations(t *testing.T) {
	tr := NewTracker("seq")
	tr.StageStart("a", "text")
	time.Sleep(20 * time.Millisecond)
	tr.StageEnd("a", "ok", map[string]any{"chars_out": 10})
	tr.StageStart("b", "text")
	time.Sleep(40 * time.Millisecond)
	tr.StageEnd("b", "ok", nil)
	tr.Finish()

	rep := tr.Report()
	if rep.Pipeline != "seq" {
		t.Fatalf("pipeline = %q, want %q", rep.Pipeline, "seq")
	}
	if len(rep.Stages) != 2 {
		t.Fatalf("got %d stages, want 2", len(rep.Stages))
	}
	if rep.Stages[0].ID != "a" || rep.Stages[1].ID != "b" {
		t.Fatalf("stage order = [%s, %s], want [a, b]", rep.Stages[0].ID, rep.Stages[1].ID)
	}
	if rep.Stages[0].DurationMS >= rep.Stages[1].DurationMS {
		t.Errorf("stage a (%dms) should be shorter than b (%dms)",
			rep.Stages[0].DurationMS, rep.Stages[1].DurationMS)
	}
	if rep.TotalMS < rep.Stages[0].DurationMS+rep.Stages[1].DurationMS {
		t.Errorf("total %dms < sum-of-stages %dms",
			rep.TotalMS, rep.Stages[0].DurationMS+rep.Stages[1].DurationMS)
	}
	if rep.Stages[0].Notes["chars_out"] != 10 {
		t.Errorf("notes[chars_out] = %v, want 10", rep.Stages[0].Notes["chars_out"])
	}
	if rep.Stages[0].Status != "ok" || rep.Stages[1].Status != "ok" {
		t.Errorf("statuses = [%s, %s], want [ok, ok]",
			rep.Stages[0].Status, rep.Stages[1].Status)
	}
}

// TestTracker_ForeachAggregatesItems verifies that foreach items attach to
// the parent stage and end up in the report's Items slice in input-array
// order, regardless of completion order. We deliberately end item 1 before
// item 0 to verify the sort.
func TestTracker_ForeachAggregatesItems(t *testing.T) {
	tr := NewTracker("fe")
	tr.StageStart("render", "comfyui")
	tr.ItemStart("render", 0)
	tr.ItemStart("render", 1)
	// Finish item 1 first; sort should still place index 0 ahead in the
	// report.
	tr.ItemEnd("render", 1, "ok", map[string]any{"files": 1})
	tr.ItemEnd("render", 0, "ok", map[string]any{"files": 1})
	tr.StageEnd("render", "ok", nil)
	tr.Finish()

	rep := tr.Report()
	if len(rep.Stages) != 1 {
		t.Fatalf("got %d stages, want 1", len(rep.Stages))
	}
	st := rep.Stages[0]
	if len(st.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(st.Items))
	}
	if st.Items[0].Index != 0 || st.Items[1].Index != 1 {
		t.Errorf("item indices = [%d, %d], want [0, 1]",
			st.Items[0].Index, st.Items[1].Index)
	}
	for i, it := range st.Items {
		if it.Notes["files"] != 1 {
			t.Errorf("item %d notes[files] = %v, want 1", i, it.Notes["files"])
		}
		if it.Status != "ok" {
			t.Errorf("item %d status = %q, want ok", i, it.Status)
		}
	}
}

// TestTracker_WriteJSONShape verifies the on-disk pipeline_timing.json
// contains every documented field. We unmarshal into map[string]any so the
// field names (not the Go-side struct names) are what we assert against.
func TestTracker_WriteJSONShape(t *testing.T) {
	tr := NewTracker("shape")
	tr.StageStart("prompts", "text")
	tr.StageEnd("prompts", "ok", map[string]any{"chars_out": 405, "max_tokens": 1024})
	tr.StageStart("render", "comfyui")
	tr.ItemStart("render", 0)
	tr.ItemEnd("render", 0, "ok", nil)
	tr.ItemStart("render", 1)
	tr.ItemEnd("render", 1, "ok", nil)
	tr.StageEnd("render", "ok", nil)
	tr.Finish()

	dir := t.TempDir()
	if err := tr.WriteJSON(dir); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pipeline_timing.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"pipeline", "started_at", "ended_at", "total_ms", "stages"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}
	stages := m["stages"].([]any)
	if len(stages) != 2 {
		t.Fatalf("got %d stages, want 2", len(stages))
	}
	prompts := stages[0].(map[string]any)
	for _, key := range []string{"id", "type", "started_at", "duration_ms", "status", "notes"} {
		if _, ok := prompts[key]; !ok {
			t.Errorf("missing stage key %q in %v", key, prompts)
		}
	}
	render := stages[1].(map[string]any)
	if _, ok := render["items"]; !ok {
		t.Errorf("foreach stage missing items: %v", render)
	}
	items := render["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	item0 := items[0].(map[string]any)
	for _, key := range []string{"index", "duration_ms", "status"} {
		if _, ok := item0[key]; !ok {
			t.Errorf("missing item key %q in %v", key, item0)
		}
	}
}

// TestTracker_FormatTable_AlignedColumns verifies the rendered table has the
// column header line, one row per stage, and a "total: ... overhead" footer.
// We don't pixel-match every space because column widths float with input;
// we assert structural properties instead.
func TestTracker_FormatTable_AlignedColumns(t *testing.T) {
	tr := NewTracker("table")
	tr.StageStart("prompts", "text")
	time.Sleep(5 * time.Millisecond)
	tr.StageEnd("prompts", "ok", map[string]any{"max_tokens": 1024})
	tr.StageStart("render", "comfyui")
	tr.ItemStart("render", 0)
	tr.ItemEnd("render", 0, "ok", nil)
	tr.ItemStart("render", 1)
	tr.ItemEnd("render", 1, "ok", nil)
	time.Sleep(5 * time.Millisecond)
	tr.StageEnd("render", "ok", nil)
	tr.Finish()

	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "pipeline \"table\" finished in") {
		t.Errorf("missing pipeline header: %q", out)
	}
	if !strings.Contains(out, "stage") || !strings.Contains(out, "type") ||
		!strings.Contains(out, "duration") || !strings.Contains(out, "notes") {
		t.Errorf("table header missing columns: %q", out)
	}
	if !strings.Contains(out, "prompts") || !strings.Contains(out, "render") {
		t.Errorf("table missing stage rows: %q", out)
	}
	if !strings.Contains(out, "2 items, foreach") {
		t.Errorf("foreach stage row missing item count: %q", out)
	}
	if !strings.Contains(out, "max_tokens") {
		t.Errorf("text stage row missing notes: %q", out)
	}
	// Footer is one of two shapes; we accept either since timing precision
	// can flip the parallel/sequential classification on fast machines.
	if !strings.Contains(out, "total:") {
		t.Errorf("missing total line: %q", out)
	}
	if !strings.Contains(out, "overhead") && !strings.Contains(out, "parallel") {
		t.Errorf("missing overhead/parallel annotation: %q", out)
	}

	// Column alignment: every body row's first column should start with two
	// spaces of indent (matches the spec sample). We exclude the header line
	// and the total line.
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "pipeline ") || strings.HasPrefix(line, "total:") {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("body line missing 2-space indent: %q", line)
		}
	}
}

// TestTracker_OverheadPercent_Sequential verifies the overhead math: a stage
// taking 50ms inside a 100ms run reports 50% overhead. Uses the helper
// directly so we don't depend on time.Sleep precision.
func TestTracker_OverheadPercent_Sequential(t *testing.T) {
	pct, parallel := overheadPercent(100*time.Millisecond, 50*time.Millisecond)
	if parallel {
		t.Errorf("parallel = true, want false")
	}
	if pct < 49.9 || pct > 50.1 {
		t.Errorf("overhead = %.2f%%, want ~50%%", pct)
	}
}

// TestTracker_OverheadPercent_ParallelClamps verifies that when stages sum
// to MORE than wall time (the parallel case), the helper returns parallel=true
// and the rendered table footer mentions "parallel".
func TestTracker_OverheadPercent_ParallelClamps(t *testing.T) {
	pct, parallel := overheadPercent(100*time.Millisecond, 200*time.Millisecond)
	if !parallel {
		t.Errorf("parallel = false, want true")
	}
	if pct != 0 {
		t.Errorf("pct = %.2f%%, want 0 in parallel case", pct)
	}

	tr := NewTracker("par")
	tr.StageStart("a", "text")
	// Force the stage's recorded duration to exceed wall time by hand:
	// rewrite the start to look earlier than the tracker's own start. We do
	// this through the internal map because there's no public knob (and we
	// only need it for the parallel-classification edge case).
	tr.mu.Lock()
	tr.stages["a"].startedAt = tr.startedAt.Add(-time.Second)
	tr.mu.Unlock()
	tr.StageEnd("a", "ok", nil)
	tr.Finish()
	var buf bytes.Buffer
	_ = tr.FormatTable(&buf)
	if !strings.Contains(buf.String(), "parallel") {
		t.Errorf("expected 'parallel' in footer when stages sum > wall time, got:\n%s", buf.String())
	}
}

// TestTracker_NilSafe verifies that every exported method on (*Tracker)(nil)
// is a no-op. The runner relies on this to keep stage executors safe when
// invoked outside of Run (e.g. some unit tests).
func TestTracker_NilSafe(t *testing.T) {
	var tr *Tracker
	tr.StageStart("a", "text")
	tr.StageEnd("a", "ok", nil)
	tr.ItemStart("a", 0)
	tr.ItemEnd("a", 0, "ok", nil)
	tr.Finish()
	rep := tr.Report()
	if rep.Pipeline != "" || len(rep.Stages) != 0 {
		t.Errorf("nil tracker produced non-empty report: %+v", rep)
	}
	if err := tr.WriteJSON(t.TempDir()); err != nil {
		t.Errorf("WriteJSON on nil tracker: %v", err)
	}
	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Errorf("FormatTable on nil tracker: %v", err)
	}
}

// TestFormatTable_RendersStageStatus is the assertion the timing table
// went 27 review phases without: that it says whether the run worked.
//
// Before this, a run with two failed stages rendered EXACTLY like a
// clean one — same rows, same durations, same closing "total:" line.
// The status lived in pipeline_timing.json, which is not the thing a
// human reads at the end of a run.
func TestFormatTable_RendersStageStatus(t *testing.T) {
	tr := NewTracker("p")
	tr.StageStart("good", "text")
	tr.StageEnd("good", "ok", nil)
	tr.StageStart("bad", "ffmpeg")
	tr.StageEnd("bad", "error", nil)
	tr.StageStart("stopped", "audio")
	tr.StageEnd("stopped", "cancelled", nil)
	// A run_when-gated stage that did not run. NOT a failure: a
	// `run_when: failure` notify stage is skipped on every successful
	// pipeline, and a verdict line that cried wolf on those would stop
	// being read.
	tr.StageStart("notify", "webhook")
	tr.StageEnd("notify", "skipped", nil)
	tr.Finish()

	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "status") {
		t.Errorf("table has no status column:\n%s", got)
	}
	for _, want := range []string{"error", "cancelled"} {
		if !strings.Contains(got, want) {
			t.Errorf("table never renders %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "skipped") {
		t.Errorf("table never renders the skipped status:\n%s", got)
	}
	// The one-line verdict, so "did it work" survives a table that has
	// scrolled off the top of the terminal.
	if !strings.Contains(got, "FAILED: 2 of 4 stage(s) did not succeed") {
		t.Errorf("table lacks the pass/fail summary line, or miscounts:\n%s", got)
	}
	if !strings.Contains(got, "bad (error)") || !strings.Contains(got, "stopped (cancelled)") {
		t.Errorf("summary line must name every failed stage:\n%s", got)
	}
	if strings.Contains(got, "notify (skipped)") {
		t.Errorf("a run_when-gated stage is not a failure:\n%s", got)
	}
}

// TestFormatTable_SkippedStageIsNotAFailure is the false-alarm case on
// its own: a pipeline whose only non-ok stage is a `run_when: failure`
// notifier ran perfectly, and must not print a failure line.
func TestFormatTable_SkippedStageIsNotAFailure(t *testing.T) {
	tr := NewTracker("p")
	tr.StageStart("work", "text")
	tr.StageEnd("work", "ok", nil)
	tr.StageStart("notify_on_failure", "webhook")
	tr.StageEnd("notify_on_failure", "skipped", nil)
	tr.Finish()
	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if strings.Contains(buf.String(), "FAILED") {
		t.Errorf("a clean run with a skipped run_when stage must not print a failure line:\n%s", buf.String())
	}
}

// TestFormatTable_CleanRunSaysNothingAboutFailure is the other half:
// the verdict line must not appear when there is no verdict to give.
func TestFormatTable_CleanRunSaysNothingAboutFailure(t *testing.T) {
	tr := NewTracker("p")
	tr.StageStart("a", "text")
	tr.StageEnd("a", "ok", nil)
	tr.Finish()
	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	if strings.Contains(buf.String(), "FAILED") {
		t.Errorf("a clean run must not print a failure line:\n%s", buf.String())
	}
}

// TestFormatTable_UnmeasuredDurationIsNotZero: a stage that ended
// without ever starting has no clock reading, and its DurationMS is
// zero. Printing that as "0ms" claims the stage failed INSTANTLY, when
// the truth is that it never ran — the opposite reading, and exactly
// the sort of number an operator uses to decide where to look.
func TestFormatTable_UnmeasuredDurationIsNotZero(t *testing.T) {
	tr := NewTracker("p")
	// StageEnd with no StageStart: the tracker synthesises the record.
	tr.StageEnd("never-ran", "error", nil)
	tr.Finish()

	rep := tr.Report()
	if len(rep.Stages) != 1 || !rep.Stages[0].DurationUnmeasured {
		t.Fatalf("synthesised stage record should be marked unmeasured: %+v", rep.Stages)
	}
	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	// Assert on the stage's own row, not the whole table: the header and
	// the "total:" footer legitimately say 0ms for a run this short.
	var row string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "never-ran") && !strings.Contains(line, "FAILED") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("no row for the stage:\n%s", buf.String())
	}
	if strings.Contains(row, "0ms") {
		t.Errorf("an unmeasured duration must not render as an instant one: %q", row)
	}
	if !strings.Contains(row, "-") {
		t.Errorf("an unmeasured duration should render as a dash: %q", row)
	}
}
