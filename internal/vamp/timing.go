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

// stageStatusOK is the one status that means the stage did its job. It
// is a named constant because the table's pass/fail line and the
// tracker's own defaults have to agree on it; the string appears in the
// JSON contract, so it must not drift.
const stageStatusOK = "ok"

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
	// capabilities records the per-capability profile resolution for the
	// run; populated via SetCapabilities and surfaced under Report.Capabilities.
	capabilities map[string]string
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
	// metrics is set via StageThroughput for LLM (text) stages; nil otherwise.
	metrics *InferenceMetrics
	// startSynthesized marks a record StageEnd had to invent because no
	// StageStart preceded it. Its startedAt is end-time, so its duration
	// is zero — which is not a measurement of anything and must not be
	// printed as one. See StageReport.DurationUnmeasured.
	startSynthesized bool
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
	metrics   *InferenceMetrics
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
	// Capabilities records which vibe profile satisfied each capability
	// during this run. Empty when the run touched no capability-bearing
	// stages (Render-only pipelines) or when the executor ran without
	// the capability resolver. Populated by Tracker.SetCapabilities at
	// run end; downstream consumers (episodic-pipeline timings, vamp runs show) use
	// it to surface "this episode ran on long_form_exl3" vs "fell back
	// to fast" without grepping the per-run log.
	Capabilities map[string]string `json:"capabilities,omitempty"`
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
	// DurationUnmeasured says DurationMS is not a measurement: the stage
	// ended without ever having started, so there was no clock to read.
	// The distinction matters because the value in that case is ZERO, and
	// a failed stage printed as "0ms" reads as "failed instantly" — the
	// opposite of the usual truth, which is that it never ran at all.
	// omitempty keeps the JSON byte-identical for every stage that did.
	DurationUnmeasured bool `json:"duration_unmeasured,omitempty"`
	// Throughput carries inference tokens/sec + TTFT for LLM stages; nil for
	// non-LLM stages (ffmpeg, comfyui, …). Surfaced as table columns + JSON.
	Throughput *InferenceMetrics `json:"throughput,omitempty"`
}

// ItemReport is one foreach item's contribution to a stage's report. Items
// appear in input-array order (sorted by Index) regardless of the order
// goroutines completed.
type ItemReport struct {
	Index      int               `json:"index"`
	DurationMS int64             `json:"duration_ms"`
	Status     string            `json:"status"`
	Notes      map[string]any    `json:"notes,omitempty"`
	Throughput *InferenceMetrics `json:"throughput,omitempty"`
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
		status:    stageStatusOK,
		notes:     map[string]any{},
	}
	t.order = append(t.order, id)
}

// StageThroughput attaches inference metrics (tokens/sec, TTFT) to a stage,
// recorded by the scheduler after a text stage's LLM call returns. No-op when
// m is nil or carries no completion tokens (non-LLM stages never populate it).
func (t *Tracker) StageThroughput(id string, m *InferenceMetrics) {
	if t == nil || m == nil || m.CompletionTokens == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.stages[id]; st != nil {
		st.metrics = m
	}
}

// ItemThroughput attaches inference metrics to a foreach item, matched by the
// caller-supplied index (the item record already exists from ItemStart). No-op
// for nil/empty metrics or an unknown stage/item.
func (t *Tracker) ItemThroughput(stageID string, index int, m *InferenceMetrics) {
	if t == nil || m == nil || m.CompletionTokens == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stages[stageID]
	if st == nil {
		return
	}
	for _, it := range st.items {
		if it.index == index {
			it.metrics = m
			return
		}
	}
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
			id:               id,
			startedAt:        time.Now(),
			notes:            map[string]any{},
			startSynthesized: true,
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
			status:    stageStatusOK,
			notes:     map[string]any{},
		}
		t.stages[stageID] = st
		t.order = append(t.order, stageID)
	}
	st.items = append(st.items, &itemTiming{
		index:     index,
		startedAt: time.Now(),
		status:    stageStatusOK,
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
			Throughput: st.metrics,
			Notes:      copyNotes(st.notes),

			DurationUnmeasured: st.startSynthesized,
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
					Throughput: it.metrics,
				}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Index < items[j].Index })
			sr.Items = items
			// A foreach parent makes no LLM call itself; surface an aggregate
			// of its items' throughput in the stage row so the table column is
			// meaningful for foreach stages too.
			if sr.Throughput == nil {
				sr.Throughput = aggregateItemMetrics(st.items)
			}
		}
		rep.Stages = append(rep.Stages, sr)
	}
	if len(t.capabilities) > 0 {
		rep.Capabilities = make(map[string]string, len(t.capabilities))
		for k, v := range t.capabilities {
			rep.Capabilities[k] = v
		}
	}
	return rep
}

// aggregateItemMetrics folds foreach item metrics into one stage-level view:
// tokens summed, gen/prefill throughput as token-weighted aggregates (total
// tokens / total seconds), TTFT averaged. Returns nil when no item carried
// metrics.
func aggregateItemMetrics(items []*itemTiming) *InferenceMetrics {
	agg := &InferenceMetrics{Source: "aggregate"}
	var genSecs, prefillSecs, ttftSum float64
	var n, ttftN int
	for _, it := range items {
		m := it.metrics
		if m == nil {
			continue
		}
		n++
		agg.CompletionTokens += m.CompletionTokens
		agg.PromptTokens += m.PromptTokens
		if m.GenTPS > 0 && m.CompletionTokens > 1 {
			genSecs += float64(m.CompletionTokens-1) / m.GenTPS
		}
		if m.PrefillTPS > 0 && m.PromptTokens > 0 {
			prefillSecs += float64(m.PromptTokens) / m.PrefillTPS
		}
		if m.TTFTms > 0 {
			ttftSum += float64(m.TTFTms)
			ttftN++
		}
	}
	if n == 0 {
		return nil
	}
	if genSecs > 0 {
		// total decode tokens (one token per item is prefill, not decode).
		agg.GenTPS = float64(agg.CompletionTokens-n) / genSecs
	}
	if prefillSecs > 0 {
		agg.PrefillTPS = float64(agg.PromptTokens) / prefillSecs
	}
	if ttftN > 0 {
		agg.TTFTms = int64(ttftSum / float64(ttftN))
	}
	return agg
}

// SetCapabilities records the per-capability profile resolution for the
// run. The executor calls this once near end-of-run after every wave has
// completed; later calls overwrite earlier ones with the same key (the
// LAST profile to serve a capability wins, mirroring the end-of-run
// summary in the live log).
func (t *Tracker) SetCapabilities(m map[string]string) {
	if t == nil || len(m) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.capabilities == nil {
		t.capabilities = make(map[string]string, len(m))
	}
	for k, v := range m {
		t.capabilities[k] = v
	}
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
//	  stage          type     status  duration  notes
//	  prompts        text     ok         31.8s  <notes>
//	  render         comfyui  error          -  <notes>
//	total: <sum-of-stages> (X% overhead)
//	FAILED: 1 of 2 stage(s) did not succeed: render (error)
//	outputs: <run-dir>     (caller appends; see runner)
//
// The status column is not decoration. Without it this table rendered a
// run with two failed stages exactly like a clean one — same rows, same
// numbers, same closing "total:" line — and the report a human actually
// reads said nothing about whether the run worked.
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
	statusW := len("status")
	durW := len("duration")
	tpsW := len("tok/s")
	tokW := len("tokens")
	ttftW := len("ttft")
	var stageMS int64
	var failed []string
	// row columns: id, type, status, duration, tok/s, tokens, ttft, notes.
	rows := make([][8]string, 0, len(rep.Stages))
	for _, s := range rep.Stages {
		tps, tok, ttft := "", "", ""
		if m := s.Throughput; m != nil {
			if m.GenTPS > 0 {
				tps = fmt.Sprintf("%.0f", m.GenTPS)
			}
			if m.CompletionTokens > 0 {
				tok = fmt.Sprintf("%d", m.CompletionTokens)
			}
			if m.TTFTms > 0 {
				ttft = formatDuration(time.Duration(m.TTFTms) * time.Millisecond)
			}
		}
		status := s.Status
		if status == "" {
			// A stage the runner never classified. "?" rather than blank:
			// an empty cell reads as "nothing to report", and the whole
			// point of this column is that an unreported status is not
			// the same claim as a successful one.
			status = "?"
		}
		if status != stageStatusOK {
			failed = append(failed, fmt.Sprintf("%s (%s)", s.ID, status))
		}
		// A duration of zero on a stage that never started is not a
		// measurement of zero. Print a dash so nobody reads "failed
		// instantly" off a stage that in fact never ran.
		durCell := "-"
		if !s.DurationUnmeasured {
			durCell = formatDuration(time.Duration(s.DurationMS) * time.Millisecond)
		}
		notes := formatNotes(s)
		row := [8]string{s.ID, s.Type, status, durCell, tps, tok, ttft, notes}
		rows = append(rows, row)
		if len(s.ID) > idW {
			idW = len(s.ID)
		}
		if len(s.Type) > typeW {
			typeW = len(s.Type)
		}
		if len(status) > statusW {
			statusW = len(status)
		}
		if len(durCell) > durW {
			durW = len(durCell)
		}
		if len(tps) > tpsW {
			tpsW = len(tps)
		}
		if len(tok) > tokW {
			tokW = len(tok)
		}
		if len(ttft) > ttftW {
			ttftW = len(ttft)
		}
		stageMS += s.DurationMS
	}
	// Header. Numeric throughput columns are right-aligned like duration;
	// they render blank for non-LLM stages.
	fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %*s  %*s  %*s  %*s  notes\n",
		idW, "stage", typeW, "type", statusW, "status", durW, "duration", tpsW, "tok/s", tokW, "tokens", ttftW, "ttft")
	for _, r := range rows {
		line := fmt.Sprintf("  %-*s  %-*s  %-*s  %*s  %*s  %*s  %*s",
			idW, r[0], typeW, r[1], statusW, r[2], durW, r[3], tpsW, r[4], tokW, r[5], ttftW, r[6])
		if r[7] != "" {
			line += "  " + r[7]
		}
		fmt.Fprintln(w, line)
	}

	stageDur := time.Duration(stageMS) * time.Millisecond
	overheadPct, parallel := overheadPercent(totalDur, stageDur)
	if parallel {
		fmt.Fprintf(w, "total: %s (parallel; stages sum %s)\n", formatDuration(totalDur), formatDuration(stageDur))
	} else {
		fmt.Fprintf(w, "total: %s (%.1f%% overhead)\n", formatDuration(stageDur), overheadPct)
	}
	// One line that answers "did this work?" without reading the column.
	// The failure summary below it explains the FIRST failure in detail;
	// this names all of them, which is what a partially-failed foreach
	// pipeline needs before deciding whether to resume.
	if len(failed) > 0 {
		fmt.Fprintf(w, "FAILED: %d of %d stage(s) did not succeed: %s\n",
			len(failed), len(rep.Stages), strings.Join(failed, ", "))
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
