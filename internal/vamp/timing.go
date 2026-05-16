package vamp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tracker records wall-clock timing for every stage (and per-item slice of
// foreach stages) of a single pipeline run. It is safe for concurrent use:
// the DAG runner may start multiple stages in parallel and a single foreach
// stage may have many items in flight, so all mutators take a mutex.
//
// The tracker has no opinion on stage status semantics beyond the string the
// caller supplies; "ok" / "error" / "cancelled" are conventions enforced by
// the runner, not the tracker.
type Tracker struct {
	pipelineName string
	startedAt    time.Time
	endedAt      time.Time

	mu     sync.Mutex
	stages map[string]*stageTiming
	// order preserves first-seen ordering of stage ids so Report and
	// FormatTable produce a deterministic layout independent of map iteration.
	order []string
}

// stageTiming is the tracker-internal record for one stage. Items is non-nil
// only for foreach stages; the runner appends per-item records as each foreach
// goroutine completes. notes is a free-form bag the executor populates with
// stage-specific facts surfaced in the table's "notes" column and the JSON.
type stageTiming struct {
	id        string
	stageType string
	startedAt time.Time
	endedAt   time.Time
	status    string
	notes     map[string]any
	items     []*itemTiming
}

// itemTiming records one foreach item's slice of wall time. index is the
// caller-supplied per-item index (matches the foreach output ordering); it is
// not necessarily equal to the slot in items[] because items are appended in
// completion order, not in input-array order.
type itemTiming struct {
	index     int
	startedAt time.Time
	endedAt   time.Time
	status    string
	notes     map[string]any
}

// Report is the externally consumable view of a run's timing. It is what
// Tracker.Report() returns and what WriteJSON serializes. Field names and JSON
// tags are part of the public contract (see pipeline_timing.json spec).
type Report struct {
	Pipeline  string        `json:"pipeline"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	TotalMS   int64         `json:"total_ms"`
	Stages    []StageReport `json:"stages"`
}

// StageReport is one stage's slice of the Report. DurationMS is the
// wall-clock time from StageStart -> StageEnd; for foreach stages it is the
// time from the parent stage's start (when the runner enters the foreach) to
// when the last item finishes, NOT the sum of item durations (which can be
// larger than wall time when items run in parallel).
type StageReport struct {
	ID         string         `json:"id"`
	Type       string         `json:"type,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	DurationMS int64          `json:"duration_ms"`
	Status     string         `json:"status"`
	Notes      map[string]any `json:"notes,omitempty"`
	Items      []ItemReport   `json:"items,omitempty"`
}

// ItemReport is one foreach item's contribution to a stage's report. Items
// appear in input-array order (sorted by Index) regardless of the order
// goroutines completed.
type ItemReport struct {
	Index      int            `json:"index"`
	DurationMS int64          `json:"duration_ms"`
	Status     string         `json:"status"`
	Notes      map[string]any `json:"notes,omitempty"`
}

// NewTracker creates a Tracker for a run. The returned tracker's start time
// is fixed at this call; Run() should construct the tracker the moment it is
// committed to executing, so the "overhead" line in the table reflects actual
// runner overhead (snapshot writes, scheduler setup) rather than time the
// pipeline spent parked in cmd-level setup.
func NewTracker(pipelineName string) *Tracker {
	return &Tracker{
		pipelineName: pipelineName,
		startedAt:    time.Now(),
		stages:       make(map[string]*stageTiming),
	}
}

// StageStart records the moment a stage's executor work begins. For ordinary
// stages this is right before runWithRetry; for foreach stages it is right
// before the fan-out semaphore loop. The stage's status defaults to "ok" and
// becomes whatever StageEnd's caller supplies.
func (t *Tracker) StageStart(id, stageType string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.stages[id]; ok {
		// Duplicate start (shouldn't happen — runner only calls once per
		// stage). Be defensive: reset the start so the duration still
		// reflects the most recent attempt rather than corrupting the
		// existing record.
		t.stages[id].startedAt = time.Now()
		t.stages[id].stageType = stageType
		return
	}
	t.stages[id] = &stageTiming{
		id:        id,
		stageType: stageType,
		startedAt: time.Now(),
		status:    "ok",
		notes:     map[string]any{},
	}
	t.order = append(t.order, id)
}

// StageEnd records the moment a stage completed and merges any per-stage
// notes the caller supplies. notes may be nil; existing notes are preserved
// (the runner can call this multiple times only on misuse — StageStart resets
// the record). Status is the runner's classification of the result ("ok",
// "error", "cancelled"); the tracker stores whatever it is given.
func (t *Tracker) StageEnd(id, status string, notes map[string]any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.stages[id]
	if !ok {
		// Missing start — synthesize one at end-time so the report still
		// contains the stage with zero duration rather than dropping it.
		st = &stageTiming{
			id:        id,
			startedAt: time.Now(),
			notes:     map[string]any{},
		}
		t.stages[id] = st
		t.order = append(t.order, id)
	}
	st.endedAt = time.Now()
	st.status = status
	for k, v := range notes {
		st.notes[k] = v
	}
}

// ItemStart records the moment a foreach item begins its slice of work. The
// runner calls it from inside the per-item goroutine right before invoking
// the executor. index is the input-array index (so per-item ordering in the
// report is deterministic regardless of completion order).
func (t *Tracker) ItemStart(stageID string, index int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.stages[stageID]
	if !ok {
		// Same defensive synthesis as StageEnd: if the runner forgot to
		// call StageStart for the parent foreach, materialize it now so
		// items still attach to a real parent record.
		st = &stageTiming{
			id:        stageID,
			startedAt: time.Now(),
			status:    "ok",
			notes:     map[string]any{},
		}
		t.stages[stageID] = st
		t.order = append(t.order, stageID)
	}
	st.items = append(st.items, &itemTiming{
		index:     index,
		startedAt: time.Now(),
		status:    "ok",
		notes:     map[string]any{},
	})
}

// ItemEnd records the moment a foreach item finished. The tracker matches the
// caller's index back to the record appended by ItemStart; if multiple
// records share the same index (shouldn't happen) the most recent one wins.
// notes are merged onto the matched record.
func (t *Tracker) ItemEnd(stageID string, index int, status string, notes map[string]any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.stages[stageID]
	if !ok {
		return
	}
	// Scan in reverse so a duplicate-index record (defensive case) resolves
	// to the most recent in-flight entry.
	for i := len(st.items) - 1; i >= 0; i-- {
		if st.items[i].index == index && st.items[i].endedAt.IsZero() {
			st.items[i].endedAt = time.Now()
			st.items[i].status = status
			for k, v := range notes {
				st.items[i].notes[k] = v
			}
			return
		}
	}
	// No matching open record — synthesize one so the index isn't lost.
	st.items = append(st.items, &itemTiming{
		index:     index,
		startedAt: time.Now(),
		endedAt:   time.Now(),
		status:    status,
		notes:     copyNotes(notes),
	})
}

// Finish stamps the end of the run. Idempotent: calling Finish twice keeps the
// later end-time so the runner can defer it without worrying about ordering
// vs. the final StageEnd.
func (t *Tracker) Finish() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endedAt = time.Now()
}

// Report materializes the tracker's current state as a serializable Report.
// Stages appear in the order they were first started; items inside each stage
// are sorted by Index so the JSON ordering matches the pipeline's foreach
// input-array ordering.
func (t *Tracker) Report() Report {
	if t == nil {
		return Report{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	end := t.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	rep := Report{
		Pipeline:  t.pipelineName,
		StartedAt: t.startedAt,
		EndedAt:   end,
		TotalMS:   end.Sub(t.startedAt).Milliseconds(),
	}
	for _, id := range t.order {
		st := t.stages[id]
		stEnd := st.endedAt
		if stEnd.IsZero() {
			// Stage was started but never ended (cancel mid-flight). Report
			// duration up to now so we don't emit a negative or zero value
			// that hides the partial work.
			stEnd = end
		}
		sr := StageReport{
			ID:         st.id,
			Type:       st.stageType,
			StartedAt:  st.startedAt,
			DurationMS: stEnd.Sub(st.startedAt).Milliseconds(),
			Status:     st.status,
			Notes:      copyNotes(st.notes),
		}
		if len(st.items) > 0 {
			items := make([]ItemReport, len(st.items))
			for i, it := range st.items {
				itEnd := it.endedAt
				if itEnd.IsZero() {
					itEnd = stEnd
				}
				items[i] = ItemReport{
					Index:      it.index,
					DurationMS: itEnd.Sub(it.startedAt).Milliseconds(),
					Status:     it.status,
					Notes:      copyNotes(it.notes),
				}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Index < items[j].Index })
			sr.Items = items
		}
		rep.Stages = append(rep.Stages, sr)
	}
	return rep
}

// WriteJSON writes pipeline_timing.json into dir. The file is pretty-printed
// (two-space indent) so users can hand-inspect it. WriteJSON is safe to call
// from inside a deferred runner-cleanup block; it returns the underlying
// os/json error verbatim so the caller can log without unwrapping.
func (t *Tracker) WriteJSON(dir string) error {
	if t == nil {
		return nil
	}
	rep := t.Report()
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal timing report: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create timing dir: %w", err)
	}
	path := filepath.Join(dir, "pipeline_timing.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// FormatTable renders the human-readable timing summary to w. The format
// matches the run-end summary documented in the timing-report spec:
//
//	pipeline "NAME" finished in <total>
//	  stage          type     duration  notes
//	  prompts        text       31.8s   <notes>
//	  render         comfyui     8.9s   <notes>
//	total: <sum-of-stages> (X% overhead)
//	outputs: <run-dir>     (caller appends; see runner)
//
// Overhead is the percentage of total wall time NOT inside any stage:
// 1 - (sum of stage durations) / total. For sequential pipelines that's the
// time spent in scheduler bookkeeping, snapshot writes, and profile
// activation; for parallel pipelines stage durations may sum to MORE than
// wall time, in which case overhead clamps to 0 and the table includes a
// "(parallel)" annotation so the number isn't misread.
func (t *Tracker) FormatTable(w io.Writer) error {
	if t == nil {
		return nil
	}
	rep := t.Report()
	totalDur := time.Duration(rep.TotalMS) * time.Millisecond
	fmt.Fprintf(w, "pipeline %q finished in %s\n", rep.Pipeline, formatDuration(totalDur))

	// Compute column widths from the data so notes don't get clipped and
	// short stage ids don't waste space. Header always participates in the
	// width so even an empty pipeline renders a usable header.
	idW := len("stage")
	typeW := len("type")
	durW := len("duration")
	var stageMS int64
	rows := make([][4]string, 0, len(rep.Stages))
	for _, s := range rep.Stages {
		dur := time.Duration(s.DurationMS) * time.Millisecond
		notes := formatNotes(s)
		row := [4]string{s.ID, s.Type, formatDuration(dur), notes}
		rows = append(rows, row)
		if len(s.ID) > idW {
			idW = len(s.ID)
		}
		if len(s.Type) > typeW {
			typeW = len(s.Type)
		}
		if len(row[2]) > durW {
			durW = len(row[2])
		}
		stageMS += s.DurationMS
	}
	// Header.
	fmt.Fprintf(w, "  %-*s  %-*s  %*s  notes\n", idW, "stage", typeW, "type", durW, "duration")
	for _, r := range rows {
		if r[3] == "" {
			fmt.Fprintf(w, "  %-*s  %-*s  %*s\n", idW, r[0], typeW, r[1], durW, r[2])
		} else {
			fmt.Fprintf(w, "  %-*s  %-*s  %*s  %s\n", idW, r[0], typeW, r[1], durW, r[2], r[3])
		}
	}

	stageDur := time.Duration(stageMS) * time.Millisecond
	overheadPct, parallel := overheadPercent(totalDur, stageDur)
	if parallel {
		fmt.Fprintf(w, "total: %s (parallel; stages sum %s)\n", formatDuration(totalDur), formatDuration(stageDur))
	} else {
		fmt.Fprintf(w, "total: %s (%.1f%% overhead)\n", formatDuration(stageDur), overheadPct)
	}
	return nil
}

// overheadPercent returns (pct, parallel). pct is the percentage of total
// wall time NOT inside any stage; parallel is true when stage durations sum
// to strictly more than total wall time, indicating parallel scheduling
// (in which case pct is not meaningful and the caller renders a different
// summary line). pct is clamped to [0, 100] for the sequential case.
func overheadPercent(total, stages time.Duration) (float64, bool) {
	// Stages summing to more than wall time means at least one wave ran in
	// parallel. We surface this case before the zero-total guard below
	// because a fast multi-stage pipeline can legitimately report
	// total==0ms in millisecond precision while stages still register
	// non-zero per-stage durations from sub-millisecond bookkeeping (or
	// from a test that manufactures the parallel case directly).
	if stages > total {
		return 0, true
	}
	if total <= 0 {
		return 0, false
	}
	pct := float64(total-stages) / float64(total) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, false
}

// formatDuration renders a duration the way the spec sample shows:
// milliseconds for sub-second runs, otherwise seconds with one decimal place
// (41.2s). Anything north of 10 minutes gets minute-precision so the column
// doesn't blow out (e.g. "12m4s"). The 's' suffix is always present so the
// column scans as duration.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < 10*time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	// Strip trailing zeros from the Duration.String form so 12m0s -> 12m.
	s := d.Truncate(time.Second).String()
	return s
}

// formatNotes renders a stage's notes into the table's free-form notes column.
// For foreach stages we surface item-count + "foreach" so users can see at a
// glance that the duration covers fan-out work; non-foreach stages just emit
// their notes map in a sorted "k=v, k=v" form.
func formatNotes(s StageReport) string {
	var parts []string
	if len(s.Items) > 0 {
		parts = append(parts, fmt.Sprintf("%d items, foreach", len(s.Items)))
	}
	if len(s.Notes) > 0 {
		keys := make([]string, 0, len(s.Notes))
		for k := range s.Notes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kvs []string
		for _, k := range keys {
			kvs = append(kvs, fmt.Sprintf("%v %s", s.Notes[k], k))
		}
		parts = append(parts, strings.Join(kvs, ", "))
	}
	return strings.Join(parts, ", ")
}

// copyNotes returns a defensive copy of a notes map so concurrent mutators
// can't observe partial state from a Report consumer. Returns nil when the
// input is empty so JSON serialization can omit the field via omitempty.
func copyNotes(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
