package vamp

// diff.go implements `vamp diff <run-a> <run-b>`: a side-by-side comparison
// of two pipeline runs so the user can answer "what actually changed between
// these two runs?" without manually `diff`-ing two directory trees.
//
// The compare pipeline is intentionally pure and CLI-agnostic: Compare()
// returns a DiffReport struct, and the DiffReport knows how to render itself as
// either human-readable text (with optional ANSI colour) or as JSON. The
// CLI layer (cli/cmd_diff.go) is a thin shell around these primitives.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// CompareOpts controls which slices of the comparison get computed and
// rendered. The zero value compares everything; callers flip the flags
// to mask out sections they don't want to read.
type CompareOpts struct {
	// StageFilter, when non-empty, restricts the per-stage section to a
	// single stage id. The pipeline / inputs / timing sections are still
	// computed because they provide framing context.
	StageFilter string
	// NoContent skips reading stage output files; only metadata
	// (status, duration_ms, prompts) is compared. Useful when the run
	// dirs are large (lots of binary outputs) and the user just wants
	// the structural diff.
	NoContent bool
}

// DiffReport is the comparison result for one pair of runs. It is the source
// of truth for both the human and JSON renderers; everything in the
// human view is derived from these fields (no separate state, so the two
// renderings can't drift).
type DiffReport struct {
	RunA RunSide `json:"run_a"`
	RunB RunSide `json:"run_b"`

	// PipelineYAMLDiff is the unified diff of pipeline.yaml.snapshot bytes
	// (a → b). Empty string when the two snapshots are byte-identical.
	PipelineYAMLDiff string `json:"pipeline_yaml_diff,omitempty"`

	// Inputs lists every input key seen in either run's inputs.json,
	// sorted by key. Entries with A == B are still included so a JSON
	// consumer can render a full table; the human renderer suppresses
	// them in the default view.
	Inputs []InputDiff `json:"inputs,omitempty"`

	// Stages is the per-stage comparison list. Stages that appear in
	// only one run are included with the missing side's fields zero and
	// Presence set to "only_a" / "only_b". Stages present in both runs
	// have Presence == "both".
	Stages []StageDiff `json:"stages"`

	// TotalDurationMS is the wall-clock duration (start → end) recorded
	// in pipeline.json. Zero when the field is missing on a side.
	TotalDurationMS DurationPair `json:"total_duration_ms"`
}

// RunSide is the per-run metadata shown at the top of the report so the
// user can confirm at a glance which two runs are being compared.
type RunSide struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

// InputDiff captures a single `key: a -> b` row.
type InputDiff struct {
	Key string `json:"key"`
	A   string `json:"a"`
	B   string `json:"b"`
}

// Equal reports whether the input was unchanged between the two runs.
// Used by the human renderer to drop noise rows from the default view.
func (i InputDiff) Equal() bool { return i.A == i.B }

// StageDiff is the comparison for a single stage id. For stages present
// in only one run, the absent side's struct fields are zero and Presence
// is "only_a" / "only_b".
type StageDiff struct {
	ID         string       `json:"id"`
	Presence   string       `json:"presence"` // "both" | "only_a" | "only_b"
	Status     StringPair   `json:"status"`
	DurationMS DurationPair `json:"duration_ms"`
	// PromptDiff is the unified diff of the stage's rendered prompt /
	// text / argv / body. Empty when the two renderings match, or when
	// the stage type doesn't expose a renderable prompt (e.g. comfyui
	// workflows that are JSON blobs are rendered as pretty-printed JSON
	// when both sides parse, otherwise as raw bytes).
	PromptDiff string `json:"prompt_diff,omitempty"`
	// OutputDiff is the unified diff of the stage's output content for
	// text-shaped outputs; for binary outputs it is empty and the JSON
	// consumer reads OutputA/OutputB to surface size+sha256.
	OutputDiff string `json:"output_diff,omitempty"`
	// OutputA / OutputB describe the on-disk output(s) for each side.
	// For text outputs Content is populated (subject to NoContent); for
	// binary outputs Content is empty and Size/SHA256 are filled.
	OutputA StageOutputSide `json:"output_a"`
	OutputB StageOutputSide `json:"output_b"`
}

// StageOutputSide describes one side of a stage's output payload. For
// text-shaped outputs (text/audio-rendered-text/youtube/webhook) Content
// is the actual string and Binary is false; for binary outputs
// (comfyui/audio-wav/ffmpeg) Size and SHA256 are populated, Content is
// empty, and Binary is true. Missing is true when the side has no
// recorded output (e.g. stage absent, or output file deleted).
type StageOutputSide struct {
	Missing bool   `json:"missing,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Size    int64  `json:"size,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// StringPair is a (a, b) pair of strings used for status comparisons.
type StringPair struct {
	A string `json:"a"`
	B string `json:"b"`
}

// Equal reports whether the two sides match.
func (p StringPair) Equal() bool { return p.A == p.B }

// DurationPair is a (a, b) pair of millisecond durations.
type DurationPair struct {
	A int64 `json:"a"`
	B int64 `json:"b"`
}

// Equal reports whether the two sides match.
func (p DurationPair) Equal() bool { return p.A == p.B }

// Compare reads the two run dirs and returns a DiffReport. Errors are limited
// to "directory missing" / "unreadable pipeline.json" — every other slice
// of the comparison degrades to a "missing" marker rather than aborting
// (one run lacking a snapshot shouldn't hide the stage-status comparison
// that's still meaningful).
func Compare(runA, runB string, opts CompareOpts) (DiffReport, error) {
	a, err := summarizeForDiff(runA)
	if err != nil {
		return DiffReport{}, fmt.Errorf("run a: %w", err)
	}
	b, err := summarizeForDiff(runB)
	if err != nil {
		return DiffReport{}, fmt.Errorf("run b: %w", err)
	}

	rep := DiffReport{
		RunA: a.side,
		RunB: b.side,
		TotalDurationMS: DurationPair{
			A: a.totalDurationMS,
			B: b.totalDurationMS,
		},
	}

	// pipeline.yaml.snapshot diff: byte-compare first to fast-path the
	// common case where the user re-ran the same pipeline without edits.
	snapA, _ := os.ReadFile(filepath.Join(a.path, "pipeline.yaml.snapshot"))
	snapB, _ := os.ReadFile(filepath.Join(b.path, "pipeline.yaml.snapshot"))
	if !bytes.Equal(snapA, snapB) {
		rep.PipelineYAMLDiff = unifiedDiff(string(snapA), string(snapB), "a/pipeline.yaml.snapshot", "b/pipeline.yaml.snapshot")
	}

	// Inputs diff: union of keys, sorted alphabetically.
	rep.Inputs = compareInputs(a.inputs, b.inputs)

	// Stages: union of stage ids, ordered by (a) pipeline declaration
	// order from run A when available, falling back to (b) the order
	// they appear in run B's pipeline.json, then (c) any extras
	// alphabetically. This keeps the per-stage block reading the same
	// way the user reads the YAML even when stages were added/removed.
	rep.Stages = compareStages(a, b, opts)
	if opts.StageFilter != "" {
		filtered := rep.Stages[:0]
		for _, s := range rep.Stages {
			if s.ID == opts.StageFilter {
				filtered = append(filtered, s)
			}
		}
		rep.Stages = filtered
	}
	return rep, nil
}

// diffRun is the internal collection of state we lift out of a run dir
// before comparing. We do the disk reads up front so compareStages can
// be a pure function over already-loaded data.
type diffRun struct {
	path            string
	side            RunSide
	totalDurationMS int64
	inputs          map[string]string
	record          *RunRecord
	pipeline        *Pipeline // parsed pipeline.yaml.snapshot; nil if unparseable
}

func summarizeForDiff(runPath string) (*diffRun, error) {
	info, err := os.Stat(runPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", runPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", runPath)
	}
	r := &diffRun{path: runPath}
	r.side.ID = filepath.Base(runPath)
	r.side.Path = runPath

	// pipeline.json: canonical record for status / per-stage timing.
	if data, rerr := os.ReadFile(filepath.Join(runPath, "pipeline.json")); rerr == nil {
		var rec RunRecord
		if jerr := json.Unmarshal(data, &rec); jerr == nil {
			r.record = &rec
			r.side.Name = rec.Name
			r.side.Status = rec.Status
			if !rec.StartTime.IsZero() && !rec.EndTime.IsZero() {
				d := rec.EndTime.Sub(rec.StartTime)
				if d > 0 {
					r.totalDurationMS = d.Milliseconds()
					r.side.DurationMS = r.totalDurationMS
				}
			}
		}
	}
	// inputs.json: a map[string]any in principle, but vamp only writes
	// flat string-valued maps today. We stringify defensively so a
	// future schema change doesn't crash the differ.
	if data, rerr := os.ReadFile(filepath.Join(runPath, "inputs.json")); rerr == nil {
		var raw map[string]any
		if jerr := json.Unmarshal(data, &raw); jerr == nil {
			r.inputs = make(map[string]string, len(raw))
			for k, v := range raw {
				r.inputs[k] = anyToString(v)
			}
		}
	}
	// pipeline.yaml.snapshot: parse so we can read stage type / prompt
	// templates / argv. Parse failures are silent — the differ falls
	// back to whatever it can extract without a Pipeline.
	if data, rerr := os.ReadFile(filepath.Join(runPath, "pipeline.yaml.snapshot")); rerr == nil && len(data) > 0 {
		var p Pipeline
		dec := yaml.NewDecoder(bytes.NewReader(data))
		// We deliberately don't enable KnownFields here: an old run's
		// snapshot may use fields the current binary doesn't know
		// about and we still want to compare its stage list rather
		// than rejecting the whole run with "unknown field".
		if jerr := dec.Decode(&p); jerr == nil {
			r.pipeline = &p
		}
	}
	return r, nil
}

// anyToString flattens a JSON-decoded value into a string suitable for
// surface-level diffing. We intentionally don't pretty-print nested
// objects (they'd swamp the inputs block); arrays/maps stringify via
// json.Marshal so the user sees something stable rather than Go's
// map-ordering noise.
func anyToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		// json.Unmarshal turns every number into float64. Format
		// integers without a decimal so "3" stays "3" rather than "3.0".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// compareInputs returns one InputDiff per key seen in either run, sorted
// alphabetically. Missing keys render with an empty value on the absent
// side so the human renderer can show "+ key: ..." for new inputs.
func compareInputs(a, b map[string]string) []InputDiff {
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := make([]InputDiff, 0, len(sorted))
	for _, k := range sorted {
		out = append(out, InputDiff{Key: k, A: a[k], B: b[k]})
	}
	return out
}

// compareStages produces the per-stage diff list. Stage ordering
// follows run A's pipeline.yaml when available so the report reads in
// declaration order rather than alphabetical / map-iteration order.
func compareStages(a, b *diffRun, opts CompareOpts) []StageDiff {
	stagesA := stageMap(a)
	stagesB := stageMap(b)
	order := stageOrder(a, b)

	out := make([]StageDiff, 0, len(order))
	for _, id := range order {
		sa, okA := stagesA[id]
		sb, okB := stagesB[id]
		sd := StageDiff{ID: id}
		switch {
		case okA && okB:
			sd.Presence = "both"
		case okA:
			sd.Presence = "only_a"
		case okB:
			sd.Presence = "only_b"
		}
		if okA {
			sd.Status.A = sa.rec.Status
			sd.DurationMS.A = sa.rec.DurationMS
		}
		if okB {
			sd.Status.B = sb.rec.Status
			sd.DurationMS.B = sb.rec.DurationMS
		}
		// Prompt / argv / body diff. We render against the loaded
		// pipeline when available; the renderer returns an empty
		// string when the stage type has no prompt or rendering
		// failed (e.g. unresolved upstream stage reference).
		promptA := renderStagePromptForDiff(a, id)
		promptB := renderStagePromptForDiff(b, id)
		if promptA != promptB {
			sd.PromptDiff = unifiedDiff(promptA, promptB, "a/"+id+"/prompt", "b/"+id+"/prompt")
		}
		// Output content diff.
		if !opts.NoContent {
			sd.OutputA = loadStageOutput(a, id)
			sd.OutputB = loadStageOutput(b, id)
			if !sd.OutputA.Binary && !sd.OutputB.Binary {
				if sd.OutputA.Content != sd.OutputB.Content {
					sd.OutputDiff = unifiedDiff(sd.OutputA.Content, sd.OutputB.Content, "a/"+id+"/output", "b/"+id+"/output")
				}
			}
		} else {
			// Metadata-only mode: still record path + presence so
			// the human renderer can say "(skipped)" rather than
			// implying the stage produced nothing.
			sd.OutputA = stageOutputMetadataOnly(a, id)
			sd.OutputB = stageOutputMetadataOnly(b, id)
		}
		out = append(out, sd)
	}
	return out
}

// stageRec pairs a StageRecord (status / duration) with the Stage
// definition pulled from pipeline.yaml.snapshot. The Stage pointer is
// nil when the snapshot was unparseable or the stage was removed from
// the yaml in a resumed run; the StageRecord is always present (it's
// what makes the stage exist in the report at all).
type stageRec struct {
	rec   StageRecord
	stage *Stage
}

// stageMap builds an id -> stageRec lookup combining pipeline.json (for
// status/duration) and pipeline.yaml.snapshot (for stage type / prompt
// template). We trust pipeline.json as the source of truth for "did this
// stage run?"; the Stage pointer is best-effort framing context.
func stageMap(r *diffRun) map[string]*stageRec {
	out := map[string]*stageRec{}
	if r.record != nil {
		for _, rec := range r.record.Stages {
			out[rec.ID] = &stageRec{rec: rec}
		}
	}
	if r.pipeline != nil {
		for i := range r.pipeline.Stages {
			st := &r.pipeline.Stages[i]
			if entry, ok := out[st.ID]; ok {
				entry.stage = st
			} else {
				// Stage in yaml but not in record: include it so
				// the user can see it was defined-but-skipped.
				out[st.ID] = &stageRec{
					rec:   StageRecord{ID: st.ID},
					stage: st,
				}
			}
		}
	}
	return out
}

// stageOrder picks an ordering for the stages section. Preference list:
// (1) A's pipeline.yaml declaration order; (2) B's; (3) A's pipeline.json
// stage list; (4) B's; (5) anything left alphabetical. Each fallback
// only contributes ids not already emitted.
func stageOrder(a, b *diffRun) []string {
	seen := map[string]bool{}
	var order []string
	push := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		order = append(order, id)
	}
	if a.pipeline != nil {
		for _, st := range a.pipeline.Stages {
			push(st.ID)
		}
	}
	if b.pipeline != nil {
		for _, st := range b.pipeline.Stages {
			push(st.ID)
		}
	}
	if a.record != nil {
		for _, st := range a.record.Stages {
			push(st.ID)
		}
	}
	if b.record != nil {
		for _, st := range b.record.Stages {
			push(st.ID)
		}
	}
	// Any extras (defensive — shouldn't happen given the four passes
	// above already cover both runs) sort alphabetically.
	return order
}

// renderStagePromptForDiff produces a string suitable for diffing one
// stage's "prompt-equivalent" payload. Per stage type:
//   - text: rendered Prompt / contents of PromptFile after template subst.
//   - audio: rendered Text template (the string fed to piper stdin).
//   - ffmpeg: rendered argv joined by newlines (one entry per line) so
//     unified diff shows per-arg changes cleanly.
//   - comfyui: workflow file path + sorted Parameters list, rendered.
//   - webhook: rendered Body marshaled to JSON, or BodyTemplateFile
//     contents rendered.
//   - youtube: rendered Title / Description / Video lines.
//
// Returns "" when the stage doesn't exist in the pipeline or rendering
// failed (we'd rather show nothing than a misleading template-error
// blob). Template rendering uses the inputs / prior-stage outputs the
// differ already loaded; foreach stages render with an empty per-item
// binding so the surface form is comparable across runs.
func renderStagePromptForDiff(r *diffRun, stageID string) string {
	if r.pipeline == nil {
		return ""
	}
	var st *Stage
	for i := range r.pipeline.Stages {
		if r.pipeline.Stages[i].ID == stageID {
			st = &r.pipeline.Stages[i]
			break
		}
	}
	if st == nil {
		return ""
	}
	prior := stagePriorOutputs(r)
	switch stageTypeOrDefault(st) {
	case StageTypeText:
		// PromptFile loads are best-effort: in the diff context the
		// referenced file may live next to the original pipeline yaml
		// (the run dir doesn't necessarily store the prompt_file
		// content). We try the run dir first (sometimes the file is
		// snapshotted), then bail with the raw template if that fails.
		raw := st.Prompt
		if raw == "" && st.PromptFile != "" {
			path := st.PromptFile
			if !filepath.IsAbs(path) {
				path = filepath.Join(r.path, path)
			}
			if data, err := os.ReadFile(path); err == nil {
				raw = string(data)
			} else {
				// Surface the unrendered prompt_file path so the
				// user sees something even when the file isn't
				// reachable from the run dir.
				return "prompt_file: " + st.PromptFile
			}
		}
		out, err := renderTemplate(stageID, raw, st.Inputs, r.inputs, prior, r.path, nil)
		if err != nil {
			return raw
		}
		return out
	case StageTypeAudio:
		out, err := renderTemplate(stageID, st.Text, st.Inputs, r.inputs, prior, r.path, nil)
		if err != nil {
			return st.Text
		}
		return out
	case StageTypeFFmpeg:
		// Render each argv entry individually so a single argument
		// change produces a clean one-line edit in the unified diff.
		rendered := make([]string, 0, len(st.FFmpegArgs))
		for i, arg := range st.FFmpegArgs {
			out, err := renderTemplate(fmt.Sprintf("%s:argv[%d]", stageID, i), arg, st.Inputs, r.inputs, prior, r.path, nil)
			if err != nil {
				rendered = append(rendered, arg)
				continue
			}
			rendered = append(rendered, out)
		}
		return strings.Join(rendered, "\n")
	case StageTypeComfyUI:
		// Workflow is a static file path. Surface it plus the
		// rendered parameter substitutions in deterministic order.
		var b strings.Builder
		fmt.Fprintf(&b, "workflow: %s\n", st.Workflow)
		keys := make([]string, 0, len(st.Parameters))
		for k := range st.Parameters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out, err := renderTemplate(stageID+":param:"+k, st.Parameters[k], st.Inputs, r.inputs, prior, r.path, nil)
			if err != nil {
				fmt.Fprintf(&b, "%s = %s\n", k, st.Parameters[k])
				continue
			}
			fmt.Fprintf(&b, "%s = %s\n", k, out)
		}
		return b.String()
	case StageTypeWebhook:
		// BodyTemplateFile takes precedence over Body when both are
		// set (validation enforces mutual exclusion; we honour the
		// same precedence the executor uses).
		if st.BodyTemplateFile != "" {
			return "body_template_file: " + st.BodyTemplateFile
		}
		// Render the Body map by rendering each leaf string value,
		// preserving the keys. We emit JSON so the result is stable
		// across map-iteration shuffles.
		rendered := renderBodyMap(st.Body, st, r, prior)
		data, err := json.MarshalIndent(rendered, "", "  ")
		if err != nil {
			return ""
		}
		return string(data)
	case StageTypeYouTube:
		var b strings.Builder
		render := func(label, raw string) {
			if raw == "" {
				return
			}
			out, err := renderTemplate(stageID+":"+label, raw, st.Inputs, r.inputs, prior, r.path, nil)
			if err != nil {
				fmt.Fprintf(&b, "%s: %s\n", label, raw)
				return
			}
			fmt.Fprintf(&b, "%s: %s\n", label, out)
		}
		render("title", st.Title)
		render("description", st.Description)
		render("video", st.Video)
		if st.Thumbnail != "" {
			render("thumbnail", st.Thumbnail)
		}
		return b.String()
	case StageTypeRender:
		// Render stages are pure template → text. Same path as text
		// stages but without LLM involvement.
		raw := st.Prompt
		if raw == "" && st.PromptFile != "" {
			path := st.PromptFile
			if !filepath.IsAbs(path) {
				path = filepath.Join(r.path, path)
			}
			if data, err := os.ReadFile(path); err == nil {
				raw = string(data)
			}
		}
		out, err := renderTemplate(stageID+":prompt", raw, st.Inputs, r.inputs, prior, r.path, nil)
		if err != nil {
			return raw
		}
		return out
	}
	return ""
}

// renderBodyMap walks the webhook stage's Body map and template-renders
// every leaf string value. Non-string leaves pass through untouched so
// numeric / boolean fields don't get accidentally stringified. The
// renderer is best-effort: a template error on one leaf leaves that
// leaf as the raw template, so the diff still shows everything else.
func renderBodyMap(in map[string]any, st *Stage, r *diffRun, prior map[string]*stageResult) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = renderBodyValue(v, st, r, prior, k)
	}
	return out
}

// renderBodyValue is the recursive helper for renderBodyMap that handles
// nested maps / slices / leaf strings. We intentionally don't try to
// be clever about types: anything that isn't a string or a container
// is passed through as-is.
func renderBodyValue(v any, st *Stage, r *diffRun, prior map[string]*stageResult, name string) any {
	switch t := v.(type) {
	case string:
		out, err := renderTemplate(st.ID+":body:"+name, t, st.Inputs, r.inputs, prior, r.path, nil)
		if err != nil {
			return t
		}
		return out
	case map[string]any:
		return renderBodyMap(t, st, r, prior)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = renderBodyValue(item, st, r, prior, fmt.Sprintf("%s[%d]", name, i))
		}
		return out
	default:
		return v
	}
}

// stagePriorOutputs loads enough prior-stage state to satisfy template
// rendering of downstream stages. We walk the pipeline declaration in
// order and, for each stage, read its rendered output path from the
// run dir if we can resolve it. Stages we can't resolve are filled in
// with empty stageResult entries so downstream renders that reference
// `.stages.X.output` succeed (with an empty string).
func stagePriorOutputs(r *diffRun) map[string]*stageResult {
	out := map[string]*stageResult{}
	if r.pipeline == nil {
		return out
	}
	for i := range r.pipeline.Stages {
		st := &r.pipeline.Stages[i]
		// Render output path against whatever prior state we've
		// accumulated so far. Foreach stages get an empty Outputs
		// slice; we don't try to enumerate items here (the diff
		// surface is per-stage, not per-item).
		outPath, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, r.inputs, out, r.path, nil)
		if err != nil {
			out[st.ID] = &stageResult{}
			continue
		}
		full := filepath.Join(r.path, outPath)
		data, err := os.ReadFile(full)
		if err != nil {
			out[st.ID] = &stageResult{}
			continue
		}
		// Only treat the file as text content when it looks textual;
		// binary outputs (PNGs etc.) get a path-shaped stand-in so
		// downstream argv templates that reference
		// `.stages.X.output` still get a usable string.
		if looksTextual(data) {
			out[st.ID] = &stageResult{Output: string(data)}
		} else {
			out[st.ID] = &stageResult{Output: full}
		}
	}
	return out
}

// loadStageOutput resolves the on-disk output for one stage and
// classifies it as text or binary. For foreach stages we concatenate
// every per-item output's metadata (size+sha256) into one
// StageOutputSide entry — the user can drill into a specific item via
// the directory layout if they need more detail.
func loadStageOutput(r *diffRun, stageID string) StageOutputSide {
	side := stageOutputMetadataOnly(r, stageID)
	if side.Missing || side.Path == "" {
		return side
	}
	data, err := os.ReadFile(filepath.Join(r.path, side.Path))
	if err != nil {
		side.Missing = true
		return side
	}
	side.Size = int64(len(data))
	sum := sha256.Sum256(data)
	side.SHA256 = hex.EncodeToString(sum[:])
	if looksTextual(data) {
		side.Content = string(data)
	} else {
		side.Binary = true
	}
	return side
}

// stageOutputMetadataOnly resolves the on-disk output path for a stage
// without reading the content. Used by --no-content and as the first
// half of loadStageOutput.
func stageOutputMetadataOnly(r *diffRun, stageID string) StageOutputSide {
	if r.pipeline == nil {
		return StageOutputSide{Missing: true}
	}
	var st *Stage
	for i := range r.pipeline.Stages {
		if r.pipeline.Stages[i].ID == stageID {
			st = &r.pipeline.Stages[i]
			break
		}
	}
	if st == nil {
		return StageOutputSide{Missing: true}
	}
	prior := stagePriorOutputs(r)
	outPath, err := renderTemplate(stageID+":output", st.Output, st.Inputs, r.inputs, prior, r.path, nil)
	if err != nil {
		return StageOutputSide{Missing: true}
	}
	full := filepath.Join(r.path, outPath)
	info, err := os.Stat(full)
	if err != nil {
		return StageOutputSide{Missing: true, Path: outPath}
	}
	side := StageOutputSide{Path: outPath, Size: info.Size()}
	// Cheap binary-ness check based on the path suffix. Helps the
	// --no-content view say "binary" without reading the file.
	if isBinaryByExtension(outPath) {
		side.Binary = true
	}
	return side
}

// isBinaryByExtension classifies a file as binary based on its
// extension when --no-content is set (no content has been read yet).
// We err on the side of "binary" for unknown extensions so the
// human renderer never accidentally tries to show raw bytes for a PNG.
func isBinaryByExtension(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".txt", ".md", ".json", ".yaml", ".yml", ".html", ".xml", ".log", ".csv", ".tsv", ".srt", ".vtt", "":
		return false
	}
	return true
}

// looksTextual returns true when data is plausibly UTF-8 text. We use
// a NUL-byte heuristic (binary formats almost universally contain NUL;
// real text almost never does) plus a UTF-8 validity check as a
// secondary screen. The 8 KiB sample is enough to catch a PNG header
// (NUL at offset 1) without eating a multi-megabyte audio file just
// to classify it.
func looksTextual(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.IndexByte(sample, 0) != -1 {
		return false
	}
	return utf8.Valid(sample)
}

// unifiedDiff produces a small unified-diff representation of two
// strings, line-oriented, with no context-line padding. The format is
// the standard "--- a / +++ b / @@ -L1,N1 +L2,N2 @@ / -line / +line"
// shape so output is readable by anything that consumes unified diffs;
// we just trim the surrounding context (each hunk lists only changed
// lines) to keep the output tight for the typical case where prompts
// share most of their structure with a single token swap.
//
// Returns "" when a == b, so callers can use the empty string as a
// "no change" sentinel without re-comparing the inputs.
//
// We're using a line-by-line LCS (longest common subsequence) here
// because the typical diff between two pipeline runs is "one token
// changed in a prompt" and line-level granularity matches what a user
// expects from `git diff`. A word-level diff would be more compact for
// some prompts but harder to scan when an entire output paragraph
// differs, which is the more common case.
func unifiedDiff(a, b, labelA, labelB string) string {
	if a == b {
		return ""
	}
	aLines := splitLinesKeepEmpty(a)
	bLines := splitLinesKeepEmpty(b)
	hunks := lineHunks(aLines, bLines)
	if len(hunks) == 0 {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n", labelA)
	fmt.Fprintf(&out, "+++ %s\n", labelB)
	for _, h := range hunks {
		// One-based hunk header line numbers, matching unified diff.
		// We use the "edited region" lengths even when one side is
		// empty (e.g. pure addition); the unified format requires a
		// non-negative length and we already gated on at-least-one
		// non-empty side via lineHunks.
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", h.aStart+1, h.aLen, h.bStart+1, h.bLen)
		for _, l := range h.aLines {
			out.WriteString("-" + l + "\n")
		}
		for _, l := range h.bLines {
			out.WriteString("+" + l + "\n")
		}
	}
	return out.String()
}

// splitLinesKeepEmpty splits s into lines, dropping the trailing newline
// from each. An empty input yields an empty slice (NOT a one-element
// slice containing ""), so the diff doesn't show a spurious "blank
// line removed" hunk when one side is missing.
func splitLinesKeepEmpty(s string) []string {
	if s == "" {
		return nil
	}
	// Strip exactly one trailing newline if present so a file ending
	// in "\n" doesn't yield a phantom empty last line. Files without
	// a trailing newline keep all their content.
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// hunk is one block of contiguous changes in a unified diff: aLines is
// the slice of A-only lines (rendered as "-"), bLines is the B-only
// lines (rendered as "+"). The Start/Len fields are 0-based offsets
// into the original line slices, used to compute the @@ header.
type hunk struct {
	aStart, aLen int
	bStart, bLen int
	aLines       []string
	bLines       []string
}

// lineHunks computes a list of edit hunks via a simple LCS-based diff.
// The implementation is O(n*m) which is fine for the sizes we see in
// practice (prompts are kilobytes at most; we don't try to diff binary
// outputs at this layer). For very long stage outputs the user can
// pass --no-content and inspect the size+sha256 difference instead.
func lineHunks(a, b []string) []hunk {
	// Compute LCS lengths table.
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	// Build the LCS DP table. We allocate (n+1)*(m+1) ints; for our
	// expected input sizes (hundreds of lines) this is trivial.
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else if lcs[i-1][j] >= lcs[i][j-1] {
				lcs[i][j] = lcs[i-1][j]
			} else {
				lcs[i][j] = lcs[i][j-1]
			}
		}
	}
	// Backtrack through the table to recover the edit script. We emit
	// triples (op, aIdx, bIdx) with op in {'=', '-', '+'}. Reversed at
	// the end because backtracking produces them in reverse order.
	type edit struct {
		op   byte
		line string
		ai   int
		bi   int
	}
	var script []edit
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			script = append(script, edit{op: '=', line: a[i-1], ai: i - 1, bi: j - 1})
			i--
			j--
		case j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]):
			script = append(script, edit{op: '+', line: b[j-1], bi: j - 1})
			j--
		default:
			script = append(script, edit{op: '-', line: a[i-1], ai: i - 1})
			i--
		}
	}
	// Reverse to forward order.
	for l, r := 0, len(script)-1; l < r; l, r = l+1, r-1 {
		script[l], script[r] = script[r], script[l]
	}
	// Group consecutive non-'=' edits into hunks.
	var hunks []hunk
	var cur *hunk
	curOpen := false
	for _, e := range script {
		if e.op == '=' {
			if curOpen {
				hunks = append(hunks, *cur)
				cur = nil
				curOpen = false
			}
			continue
		}
		if !curOpen {
			cur = &hunk{aStart: -1, bStart: -1}
			curOpen = true
		}
		if e.op == '-' {
			if cur.aStart == -1 {
				cur.aStart = e.ai
			}
			cur.aLen++
			cur.aLines = append(cur.aLines, e.line)
		} else { // '+'
			if cur.bStart == -1 {
				cur.bStart = e.bi
			}
			cur.bLen++
			cur.bLines = append(cur.bLines, e.line)
		}
	}
	if curOpen {
		hunks = append(hunks, *cur)
	}
	// For "pure addition" / "pure deletion" hunks one side's Start is
	// still -1. Normalise to 0 so the @@ header is non-negative; the
	// associated Len is zero, which matches unified-diff convention
	// for an empty edit region.
	for k := range hunks {
		if hunks[k].aStart < 0 {
			hunks[k].aStart = 0
		}
		if hunks[k].bStart < 0 {
			hunks[k].bStart = 0
		}
	}
	return hunks
}

// ----- rendering -----

// ansi colour helpers. The renderer accepts a useColor flag so the
// CLI layer can decide based on (tty + NO_COLOR) once and not pass
// the decision through every call.
const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
	ansiCyan  = "\x1b[36m"
)

// Markdown writes the report to w in the human-readable "vamp diff"
// surface format documented in cli/cmd_diff.go's help. Despite the name
// it is plain text with optional ANSI colour, not Markdown — the name
// pairs with DiffReport.JSON to express "human renderer" vs "machine
// renderer", not the literal output syntax.
func (r DiffReport) Markdown(w io.Writer, useColor bool) error {
	bold := func(s string) string {
		if !useColor {
			return s
		}
		return ansiBold + s + ansiReset
	}
	red := func(s string) string {
		if !useColor {
			return s
		}
		return ansiRed + s + ansiReset
	}
	green := func(s string) string {
		if !useColor {
			return s
		}
		return ansiGreen + s + ansiReset
	}
	dim := func(s string) string {
		if !useColor {
			return s
		}
		return ansiDim + s + ansiReset
	}
	cyan := func(s string) string {
		if !useColor {
			return s
		}
		return ansiCyan + s + ansiReset
	}

	fmt.Fprintln(w, bold("=== runs ==="))
	fmt.Fprintf(w, "A: %s\n", formatRunSide(r.RunA))
	fmt.Fprintf(w, "B: %s\n", formatRunSide(r.RunB))
	fmt.Fprintln(w)

	fmt.Fprintln(w, bold("=== pipeline.yaml ==="))
	if r.PipelineYAMLDiff == "" {
		fmt.Fprintln(w, dim("(identical)"))
	} else {
		writeColoredDiff(w, r.PipelineYAMLDiff, useColor)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, bold("=== inputs ==="))
	changed := false
	for _, in := range r.Inputs {
		if in.Equal() {
			continue
		}
		changed = true
		fmt.Fprintf(w, "%s %s: %s -> %s\n", cyan("~"), in.Key, quoteForDisplay(in.A), quoteForDisplay(in.B))
	}
	if !changed {
		fmt.Fprintln(w, dim("(identical)"))
	}
	fmt.Fprintln(w)

	for _, s := range r.Stages {
		fmt.Fprintf(w, "%s\n", bold(fmt.Sprintf("=== stage: %s ===", s.ID)))
		switch s.Presence {
		case "only_a":
			fmt.Fprintln(w, red("(removed in B; present only in A)"))
		case "only_b":
			fmt.Fprintln(w, green("(added in B; present only in B)"))
		}
		if s.Status.A != "" || s.Status.B != "" {
			if s.Status.Equal() {
				fmt.Fprintf(w, "status: %s\n", s.Status.A)
			} else {
				fmt.Fprintf(w, "status: %s -> %s\n", red(s.Status.A), green(s.Status.B))
			}
		}
		if s.DurationMS.A != 0 || s.DurationMS.B != 0 {
			fmt.Fprintf(w, "duration: %s -> %s\n", formatMS(s.DurationMS.A), formatMS(s.DurationMS.B))
		}
		if s.PromptDiff != "" {
			fmt.Fprintln(w, "prompt:")
			writeColoredDiff(w, s.PromptDiff, useColor)
		}
		writeOutputBlock(w, s, useColor, dim, red, green)
		fmt.Fprintln(w)
	}

	if r.TotalDurationMS.A != 0 || r.TotalDurationMS.B != 0 {
		fmt.Fprintln(w, bold("=== pipeline timing ==="))
		if r.TotalDurationMS.Equal() {
			fmt.Fprintf(w, "total: %s\n", formatMS(r.TotalDurationMS.A))
		} else {
			fmt.Fprintf(w, "total: %s -> %s\n", formatMS(r.TotalDurationMS.A), formatMS(r.TotalDurationMS.B))
		}
	}
	return nil
}

// writeOutputBlock renders the per-stage "output (text|binary):" block.
// Binary outputs always render as size + sha256 even when one side has
// text content and the other doesn't — the asymmetry tells the user
// something changed about the output's shape.
func writeOutputBlock(w io.Writer, s StageDiff, useColor bool, dim, red, green func(string) string) {
	a, b := s.OutputA, s.OutputB
	if a.Missing && b.Missing {
		return
	}
	if a.Binary || b.Binary {
		fmt.Fprintln(w, "output (binary):")
		fmt.Fprintf(w, "  A: %s\n", formatBinarySide(a))
		fmt.Fprintf(w, "  B: %s\n", formatBinarySide(b))
		return
	}
	if s.OutputDiff != "" {
		fmt.Fprintln(w, "output (text):")
		writeColoredDiff(w, s.OutputDiff, useColor)
		return
	}
	// Same text content (or both empty).
	if a.Content != "" || b.Content != "" {
		fmt.Fprintln(w, dim("output: (identical)"))
	}
}

// formatBinarySide renders one side of a binary output as
// "size sha256:..." for the human view. Missing entries render as a
// dim placeholder so the column still aligns.
func formatBinarySide(s StageOutputSide) string {
	if s.Missing {
		return "(missing)"
	}
	if s.SHA256 == "" {
		return fmt.Sprintf("%d B (no sha)", s.Size)
	}
	return fmt.Sprintf("%d B sha256:%s", s.Size, shortHash(s.SHA256))
}

// shortHash trims a hex digest to the prefix + ellipsis + suffix form
// vamp uses everywhere it surfaces a sha256 (matches the resume-drift
// message in exec.go). The full hash is kept in the JSON output for
// callers that need byte-for-byte verification.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:6] + "..." + h[len(h)-4:]
}

// writeColoredDiff prints a unified diff string, colouring "-" lines
// red and "+" lines green when useColor is true. Header lines (--- /
// +++ / @@) are dimmed so the content stands out.
func writeColoredDiff(w io.Writer, d string, useColor bool) {
	for _, line := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
		if !useColor {
			fmt.Fprintln(w, line)
			continue
		}
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "@@"):
			fmt.Fprintln(w, ansiDim+line+ansiReset)
		case strings.HasPrefix(line, "-"):
			fmt.Fprintln(w, ansiRed+line+ansiReset)
		case strings.HasPrefix(line, "+"):
			fmt.Fprintln(w, ansiGreen+line+ansiReset)
		default:
			fmt.Fprintln(w, line)
		}
	}
}

// formatRunSide renders the per-run header line ("ID  (duration, status)").
// Empty status / zero duration fall back to "-" so the format stays
// uniform across legacy runs and modern ones.
func formatRunSide(s RunSide) string {
	dur := "-"
	if s.DurationMS > 0 {
		dur = formatMS(s.DurationMS)
	}
	status := s.Status
	if status == "" {
		status = "-"
	}
	return fmt.Sprintf("%s  (%s, %s)", s.ID, dur, status)
}

// formatMS renders a millisecond duration as a compact human string. We
// echo timing.go's formatDuration shape so the diff output reads like
// the run-end summary the user already knows.
func formatMS(ms int64) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	secs := float64(ms) / 1000.0
	if secs < 60 {
		return fmt.Sprintf("%.1fs", secs)
	}
	mins := int(secs) / 60
	rem := int(secs) - mins*60
	return fmt.Sprintf("%dm%02ds", mins, rem)
}

// quoteForDisplay wraps a value in double quotes when it contains
// whitespace or is empty, so the inputs section reads unambiguously.
// Numeric / single-word values stay unquoted for compactness.
func quoteForDisplay(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// JSON writes the report as a pretty-printed JSON object to w. The
// shape matches the spec'd schema (run_a / run_b / pipeline_yaml_diff
// / inputs / stages / total_duration_ms); see the DiffReport struct tags
// for the exact field names.
func (r DiffReport) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
