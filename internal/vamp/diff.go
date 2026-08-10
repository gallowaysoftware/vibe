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
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
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
//
// The three artefact flags exist because Compare used to discard
// os.ReadFile's error into `_`: a run with no pipeline.yaml.snapshot
// compared empty-to-empty, found no difference, and the renderer printed
// `(identical)` — a comparison that never ran, rendered as agreement.
// exec.go writes an EMPTY snapshot by design when PipelineSource is
// unpopulated, so it is a reachable state and not a hypothetical.
type RunSide struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// SnapshotMissing is true when pipeline.yaml.snapshot is absent,
	// unreadable or empty.
	SnapshotMissing bool `json:"snapshot_missing,omitempty"`
	// SnapshotUnparsed is true when the snapshot exists but did not
	// decode — in which case every per-stage prompt and output
	// comparison for this side silently degrades to nothing.
	SnapshotUnparsed bool `json:"snapshot_unparsed,omitempty"`
	// InputsMissing is true when inputs.json is absent or unreadable.
	InputsMissing bool `json:"inputs_missing,omitempty"`
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
	Missing bool `json:"missing,omitempty"`
	Binary  bool `json:"binary,omitempty"`
	// Foreach marks the aggregated entry for a fan-out stage: Content is
	// a per-item manifest (one `path  size  sha256` line per item),
	// Items is how many were enumerated, Size is their total, and Path
	// is empty because there is no single file. Diffing the manifest is
	// what makes a changed item visible even when the per-item outputs
	// are binary.
	Foreach bool `json:"foreach,omitempty"`
	Items   int  `json:"items,omitempty"`
	// NotCompared, when non-empty, is the REASON this side was not
	// compared. It is distinct from Missing on purpose: Missing means
	// "there is no output here", and returning it for "I could not work
	// out what the outputs are" is how a skipped comparison came to
	// render as agreement.
	NotCompared string `json:"not_compared,omitempty"`
	Path        string `json:"path,omitempty"`
	Content     string `json:"content,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
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
	// The bytes were read once, by summarizeForDiff, which also recorded
	// WHY they might be empty — an absent snapshot and an identical one
	// used to produce the same empty diff and the same `(identical)`.
	if !bytes.Equal(a.snapshot, b.snapshot) {
		rep.PipelineYAMLDiff = unifiedDiff(string(a.snapshot), string(b.snapshot), "a/pipeline.yaml.snapshot", "b/pipeline.yaml.snapshot")
	}

	// Inputs diff: union of keys, sorted alphabetically.
	rep.Inputs = compareInputs(a.inputs, b.inputs)
	for i := range rep.Inputs {
		if a.credentialInputs[rep.Inputs[i].Key] || b.credentialInputs[rep.Inputs[i].Key] {
			rep.Inputs[i].A = redactInputValue(rep.Inputs[i].A)
			rep.Inputs[i].B = redactInputValue(rep.Inputs[i].B)
		}
	}

	// Stages: union of stage ids, ordered by (a) pipeline declaration
	// order from run A when available, falling back to (b) the order
	// they appear in run B's pipeline.json, then (c) any extras
	// alphabetically. This keeps the per-stage block reading the same
	// way the user reads the YAML even when stages were added/removed.
	rep.Stages = compareStages(a, b, opts)
	if opts.StageFilter != "" {
		known := make([]string, 0, len(rep.Stages))
		filtered := rep.Stages[:0]
		for _, s := range rep.Stages {
			known = append(known, s.ID)
			if s.ID == opts.StageFilter {
				filtered = append(filtered, s)
			}
		}
		// A filter that matches nothing is a typo, not a finding. Left
		// silent it produced a report with the headers, zero stage
		// blocks and a zero exit status — indistinguishable from "these
		// two runs agree about that stage". The sibling command already
		// refuses this: RenderStageForPipeline returns
		// `stage %q not found (have: …)`.
		if len(filtered) == 0 {
			return DiffReport{}, fmt.Errorf("stage %q not found in either run (have: %s)", opts.StageFilter, strings.Join(known, ", "))
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
	snapshot        []byte    // raw pipeline.yaml.snapshot bytes; nil when absent
	// credentialInputs names the inputs whose VALUES reach a webhook's url
	// or one of its headers. Those are bearer-equivalent by construction
	// — a Slack/Discord/ntfy incoming-webhook URL carries its credential
	// in the path, and `Authorization: Bearer {{.inputs.tok}}` carries one
	// by definition — so the report shows that they changed without
	// showing what they are.
	credentialInputs map[string]bool
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
	r.side.InputsMissing = true
	if data, rerr := os.ReadFile(filepath.Join(runPath, "inputs.json")); rerr == nil {
		var raw map[string]any
		if jerr := json.Unmarshal(data, &raw); jerr == nil {
			r.inputs = make(map[string]string, len(raw))
			for k, v := range raw {
				r.inputs[k] = anyToString(v)
			}
			r.side.InputsMissing = false
		}
	}
	// pipeline.yaml.snapshot: parse so we can read stage type / prompt
	// templates / argv. Parse failures are silent — the differ falls
	// back to whatever it can extract without a Pipeline.
	//
	// The three outcomes are kept apart — absent, present-but-unparseable,
	// parsed — because the first two used to be indistinguishable from
	// "compared and equal" in the rendered report.
	data, rerr := os.ReadFile(filepath.Join(runPath, "pipeline.yaml.snapshot"))
	switch {
	case rerr != nil || len(data) == 0:
		r.side.SnapshotMissing = true
	default:
		r.snapshot = data
		var p Pipeline
		dec := yaml.NewDecoder(bytes.NewReader(data))
		// We deliberately don't enable KnownFields here: an old run's
		// snapshot may use fields the current binary doesn't know
		// about and we still want to compare its stage list rather
		// than rejecting the whole run with "unknown field".
		if jerr := dec.Decode(&p); jerr == nil {
			r.pipeline = &p
		} else {
			r.side.SnapshotUnparsed = true
		}
	}
	r.credentialInputs = credentialInputKeys(r.pipeline)
	return r, nil
}

// inputRefRE matches a template's reference to a named CLI input.
var inputRefRE = regexp.MustCompile(`\.inputs\.([A-Za-z_][A-Za-z0-9_]*)`)

// credentialInputKeys returns the input names that a webhook stage's url
// or headers interpolate. Those values are bearer-equivalent, and this
// report is printed to a terminal, redirected as --json and pasted into
// bug reports. vamp already wrote them to <run-dir>/inputs.json at 0644,
// so the diff is not where the secret originates — it is a new place it
// gets DISTRIBUTED, which is the multiplier the run-log fix (#71) was
// about.
func credentialInputKeys(p *Pipeline) map[string]bool {
	keys := map[string]bool{}
	if p == nil {
		return keys
	}
	add := func(tmpl string) {
		for _, m := range inputRefRE.FindAllStringSubmatch(tmpl, -1) {
			keys[m[1]] = true
		}
	}
	for i := range p.Stages {
		st := &p.Stages[i]
		if stageTypeOrDefault(st) != StageTypeWebhook {
			continue
		}
		add(st.URL)
		for _, v := range st.Headers {
			add(v)
		}
	}
	return keys
}

// redactInputValue keeps the fact that a value CHANGED without printing
// it: a URL keeps its scheme and host (fleetnotify.Redact), anything else
// becomes a stable per-value id. Two different tokens still produce two
// different lines, which is the whole job of the inputs section.
func redactInputValue(v string) string {
	if v == "" {
		return v
	}
	if strings.Contains(v, "://") {
		return fleetnotify.Redact(v)
	}
	sum := sha256.Sum256([]byte(v))
	return "(redacted, id " + hex.EncodeToString(sum[:4]) + ")"
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

	// ONCE per run, not once per stage per consumer. stagePriorOutputs
	// walks every stage in the pipeline and reads each one's output file,
	// and it used to be called four times per stage id — twice through
	// renderStagePromptForDiff and twice through stageOutputMetadataOnly —
	// so Compare read each run dir 2N+1 times over. Measured on synthetic
	// runs with ~104 KB per stage: 40 stages, 8.6 MB on disk, 700 MB read.
	// --no-content, documented as the escape hatch for large run dirs,
	// removed exactly one of those 2N+1 reads (a 1.2% reduction), and
	// --stage <id> read all 81x because the filter runs after the work.
	//
	// The two maps are pure functions of their *diffRun, so hoisting them
	// is behaviour-preserving and takes 2N+1 to 3. They are PARAMETERS of
	// the three consumers rather than something each recomputes so the
	// defect cannot come back: there is no longer a way to ask for prior
	// state without being handed it.
	priorA := stagePriorOutputs(a)
	priorB := stagePriorOutputs(b)

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
		promptA := renderStagePromptForDiff(a, id, priorA)
		promptB := renderStagePromptForDiff(b, id, priorB)
		if promptA != promptB {
			sd.PromptDiff = unifiedDiff(promptA, promptB, "a/"+id+"/prompt", "b/"+id+"/prompt")
		}
		// Output content diff.
		if !opts.NoContent {
			sd.OutputA = loadStageOutput(a, id, priorA)
			sd.OutputB = loadStageOutput(b, id, priorB)
			if !sd.OutputA.Binary && !sd.OutputB.Binary {
				if sd.OutputA.Content != sd.OutputB.Content {
					sd.OutputDiff = unifiedDiff(sd.OutputA.Content, sd.OutputB.Content, "a/"+id+"/output", "b/"+id+"/output")
				}
			}
		} else {
			// Metadata-only mode: still record path + presence so
			// the human renderer can say "(skipped)" rather than
			// implying the stage produced nothing.
			sd.OutputA = stageOutputSide(a, id, priorA, false)
			sd.OutputB = stageOutputSide(b, id, priorB, false)
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
func renderStagePromptForDiff(r *diffRun, stageID string, prior map[string]*stageResult) string {
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
	// One best-effort render, used by every arm below: a template error
	// leaves the raw template in place so the diff still shows the change
	// rather than degrading to the empty string, which every caller reads
	// as "these two runs agree".
	render := func(name, raw string) string {
		out, err := renderTemplate(name, raw, st.Inputs, r.inputs, prior, r.path, nil)
		if err != nil {
			return raw
		}
		return out
	}
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
		var b strings.Builder
		// url / method / headers determine WHERE the request went and
		// with what authorisation, and none of the three used to be part
		// of the comparison: `vamp diff` could not answer "did these two
		// runs notify the same endpoint?" — it printed
		// `output: (identical)` for two runs that hit different servers.
		//
		// Redact, not the raw URL: an incoming-webhook URL carries its
		// bearer in the path, and this report is printed to a terminal,
		// redirected as --json and pasted into bug reports. The digest is
		// stable per URL, so two different endpoints still produce two
		// different lines — which is strictly more than the nothing that
		// was here before, without the credential.
		method := strings.ToUpper(st.Method)
		if method == "" {
			method = "POST"
		}
		fmt.Fprintf(&b, "method: %s\n", method)
		fmt.Fprintf(&b, "url: %s\n", fleetnotify.Redact(renderWebhookForDiff(st, r, prior, "url", st.URL)))
		if len(st.Headers) > 0 {
			// Names only, sorted. A header VALUE is exactly where the
			// credential lives (`Authorization: Bearer …`), so the diff
			// reports that the set of headers changed and never what is
			// in them.
			names := make([]string, 0, len(st.Headers))
			for k := range st.Headers {
				names = append(names, k)
			}
			sort.Strings(names)
			fmt.Fprintf(&b, "headers: %s\n", strings.Join(names, ", "))
		}
		// BodyTemplateFile takes precedence over Body when both are
		// set (validation enforces mutual exclusion; we honour the
		// same precedence the executor uses).
		if st.BodyTemplateFile != "" {
			fmt.Fprintf(&b, "body_template_file: %s\n", st.BodyTemplateFile)
			return b.String()
		}
		// Render the Body map by rendering each leaf string value,
		// preserving the keys. We emit JSON so the result is stable
		// across map-iteration shuffles.
		rendered := renderBodyMap(st.Body, func(name, raw string) string {
			return renderWebhookForDiff(st, r, prior, "body:"+name, raw)
		})
		data, err := json.MarshalIndent(rendered, "", "  ")
		if err != nil {
			return b.String()
		}
		b.WriteString("body: ")
		b.Write(data)
		b.WriteString("\n")
		return b.String()
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
			} else {
				// Identical to the text branch, which had this and
				// this one did not. Without it raw stays "", both
				// sides render to "", compareStages sees them as
				// equal, and a render stage whose prompt_file CHANGED
				// between the two runs shows no diff at all —
				// precisely when the file isn't reachable from the run
				// dir, which the note above says is the normal case.
				return "prompt_file: " + st.PromptFile
			}
		}
		out, err := renderTemplate(stageID+":prompt", raw, st.Inputs, r.inputs, prior, r.path, nil)
		if err != nil {
			return raw
		}
		return out
	case StageTypeConfirm:
		// The message is the stage's whole payload: it is what the human
		// is asked, and a run that asked a different question is a
		// different run.
		return "message: " + render(stageID+":message", st.Message)
	case StageTypeCompact:
		var b strings.Builder
		fmt.Fprintf(&b, "target_chars: %d\n", st.TargetChars)
		if st.ChunkChars > 0 {
			fmt.Fprintf(&b, "chunk_chars: %d\n", st.ChunkChars)
		}
		if st.Preserve != "" {
			fmt.Fprintf(&b, "preserve: %s\n", st.Preserve)
		}
		fmt.Fprintf(&b, "source: %s\n", render(stageID+":source", st.Source))
		return b.String()
	case StageTypePandoc:
		var b strings.Builder
		fmt.Fprintf(&b, "source_file: %s\n", render(stageID+":source_file", st.SourceFile))
		fmt.Fprintf(&b, "pandoc_from: %s\n", st.PandocFrom)
		fmt.Fprintf(&b, "pandoc_to: %s\n", st.PandocTo)
		if st.CoverImage != "" {
			fmt.Fprintf(&b, "cover_image: %s\n", render(stageID+":cover_image", st.CoverImage))
		}
		// pandoc_metadata and pandoc_args are shown RAW, not rendered,
		// because buildPandocArgs passes them to the subprocess exactly
		// as written (pandoc_executor.go): rendering them here would
		// report a difference between two runs whose actual argv was
		// identical. The differ's job is to describe the run that
		// happened, not the one the field name suggests.
		for _, k := range sortedKeys(st.PandocMetadata) {
			fmt.Fprintf(&b, "metadata: %s = %s (not template-rendered by the executor)\n", k, st.PandocMetadata[k])
		}
		for i, arg := range st.PandocArgs {
			fmt.Fprintf(&b, "arg[%d]: %s\n", i, arg)
		}
		return b.String()
	case StageTypeMix, StageTypeShort:
		var b strings.Builder
		fmt.Fprintf(&b, "script_file: %s\n", render(stageID+":script_file", st.ScriptFile))
		if st.Binary != "" {
			fmt.Fprintf(&b, "binary: %s\n", st.Binary)
		}
		if st.LoudnessTarget != 0 {
			fmt.Fprintf(&b, "loudness_target: %g\n", st.LoudnessTarget)
		}
		if stageTypeOrDefault(st) == StageTypeShort {
			if st.ShortWidth != 0 || st.ShortHeight != 0 || st.ShortFPS != 0 {
				fmt.Fprintf(&b, "video: %dx%d @%dfps\n", st.ShortWidth, st.ShortHeight, st.ShortFPS)
			}
			fmt.Fprintf(&b, "stretch_video: %t\n", st.ShortStretchVideo)
		} else {
			if st.IntroMusic != "" {
				fmt.Fprintf(&b, "intro_music: %s\n", render(stageID+":intro_music", st.IntroMusic))
			}
			if st.OutroMusic != "" {
				fmt.Fprintf(&b, "outro_music: %s\n", render(stageID+":outro_music", st.OutroMusic))
			}
		}
		for _, k := range sortedKeys(st.Metadata) {
			fmt.Fprintf(&b, "metadata: %s = %s\n", k, render(stageID+":metadata:"+k, st.Metadata[k]))
		}
		return b.String()
	}
	return ""
}

// sortedKeys returns a map's keys in a deterministic order, so a rendered
// block is stable across the map-iteration shuffle Go deliberately
// introduces. Two runs of the same pipeline must produce the same string
// or the differ invents changes.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// renderWebhookForDiff renders one webhook template with the same
// bindings the webhook executor uses (it adds `.pipeline_name` and the
// `env` function on top of the standard set), falling back to the raw
// template on error the way the rest of this file does.
func renderWebhookForDiff(st *Stage, r *diffRun, prior map[string]*stageResult, name, raw string) string {
	in := StageInput{
		Stage:  st,
		Inputs: r.inputs,
		Prior:  prior,
		RunDir: r.path,
	}
	if r.pipeline != nil {
		in.PipelineName = r.pipeline.Name
	}
	out, err := renderWebhookTemplate(st, name, raw, in, nil)
	if err != nil {
		return raw
	}
	return out
}

// renderBodyMap walks the webhook stage's Body map and template-renders
// every leaf string value through render. Non-string leaves pass through
// untouched so numeric / boolean fields don't get accidentally
// stringified. render is expected to be best-effort: a template error on
// one leaf should leave that leaf as the raw template, so the diff still
// shows everything else.
//
// render is a parameter rather than (st, r, prior) because the dry-run
// preview needs the identical walk over the identical map with a
// different binding — and a second copy of a recursive renderer is how
// the two surfaces start disagreeing about what a webhook stage sends.
func renderBodyMap(in map[string]any, render func(name, raw string) string) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = renderBodyValue(v, k, render)
	}
	return out
}

// renderBodyValue is the recursive helper for renderBodyMap that handles
// nested maps / slices / leaf strings. We intentionally don't try to
// be clever about types: anything that isn't a string or a container
// is passed through as-is.
func renderBodyValue(v any, name string, render func(name, raw string) string) any {
	switch t := v.(type) {
	case string:
		return render(name, t)
	case map[string]any:
		return renderBodyMap(t, render)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = renderBodyValue(item, fmt.Sprintf("%s[%d]", name, i), render)
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
		// Same containment rule the executor and the dry run apply. The
		// pipeline this path renders came from the run dir's
		// pipeline.yaml.snapshot, which Executor.snapshot() writes
		// VERBATIM before any stage runs — so a pipeline whose output
		// template the executor would have refused still leaves the
		// template on disk for the differ to render. filepath.Join
		// cleans but does not contain, so without this an `output:`
		// of "../../../../etc/passwd" is read and, being textual, is
		// embedded whole into the diff report.
		if err := ensureUnderRunDir(outPath); err != nil {
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

// maxContentBytesToDiff bounds what loadStageOutput will hold in memory
// as diffable text. Above it the side keeps its size and digest and says
// it was not compared, which is a REPORTED state — the alternative is a
// multi-hundred-megabyte string that then feeds a quadratic diff.
const maxContentBytesToDiff = 32 << 20

// loadStageOutput resolves the on-disk output for one stage and
// classifies it as text or binary. Foreach stages take the manifest path
// in stageOutputSide instead: there is no single file to read.
func loadStageOutput(r *diffRun, stageID string, prior map[string]*stageResult) StageOutputSide {
	side := stageOutputSide(r, stageID, prior, true)
	if side.Missing || side.Foreach || side.NotCompared != "" || side.Path == "" {
		return side
	}
	// Stream rather than os.ReadFile: this used to read the WHOLE file
	// into memory purely to sha256 it, so a 2 GB mp4 became a 2 GB
	// allocation. internal/vamp/cache already does it this way. Only the
	// first 8 KiB is needed to classify, and only textual content is
	// retained.
	full := filepath.Join(r.path, side.Path)
	f, err := os.Open(full)
	if err != nil {
		side.Missing = true
		return side
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, rerr := io.ReadFull(f, head)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		side.Missing = true
		return side
	}
	head = head[:n]
	textual := looksTextual(head)
	h := sha256.New()
	h.Write(head)
	switch {
	case !textual:
		side.Binary = true
		if _, err := io.Copy(h, f); err != nil {
			side.Missing = true
			return side
		}
	case side.Size > maxContentBytesToDiff:
		side.NotCompared = fmt.Sprintf("output is %d MB — too large to diff inline", side.Size>>20)
		if _, err := io.Copy(h, f); err != nil {
			side.Missing = true
			return side
		}
	default:
		var buf bytes.Buffer
		buf.Grow(int(side.Size))
		buf.Write(head)
		if _, err := io.Copy(io.MultiWriter(h, &buf), f); err != nil {
			side.Missing = true
			return side
		}
		side.Content = buf.String()
	}
	sum := h.Sum(nil)
	side.SHA256 = hex.EncodeToString(sum)
	return side
}

// stageOutputSide resolves the on-disk output for a stage without
// reading its content. withDigests asks the foreach path to hash every
// per-item file; --no-content skips that, which is what finally makes
// that flag mean something (it used to remove one read out of 2N+1).
func stageOutputSide(r *diffRun, stageID string, prior map[string]*stageResult, withDigests bool) StageOutputSide {
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
	if st.Foreach != nil {
		return foreachOutputSide(r, st, prior, withDigests)
	}
	outPath, err := renderTemplate(stageID+":output", st.Output, st.Inputs, r.inputs, prior, r.path, nil)
	if err != nil {
		return StageOutputSide{Missing: true}
	}
	// Second of the two run-dir-escape paths in this file; see the note
	// in stagePriorOutputs. This one feeds loadStageOutput, which reads
	// the file and puts its bytes in StageOutputSide.Content.
	if err := ensureUnderRunDir(outPath); err != nil {
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

// maxForeachItemsToCompare bounds the per-item enumeration. A fan-out
// larger than this is compared for its first N items and says so; the
// alternative is an unbounded stat+hash walk driven by whatever the
// upstream stage emitted.
const maxForeachItemsToCompare = 500

// foreachOutputSide is the fan-out stage's output entry: one manifest
// line per item, `path  size  sha256`, concatenated into Content so the
// ordinary text diff shows exactly which items changed.
//
// This is what the doc comment above loadStageOutput claimed for months
// while NO code did it. What happened instead: st.Output was rendered
// with no per-item binding, `{{.i}}` failed under missingkey=error, and
// the failure returned Missing:true — the same value that means "the
// stage does not exist" — so writeOutputBlock printed nothing at all.
// Two runs whose four fan-out files differed completely produced two
// lines of status and duration, and foreach is where the CONTENT of a
// run lives.
func foreachOutputSide(r *diffRun, st *Stage, prior map[string]*stageResult, withDigests bool) StageOutputSide {
	items, err := diffForeachItems(r, st, prior)
	if err != nil {
		// A stated reason, never Missing: "I could not work out what the
		// items are" and "there is nothing here" are different facts.
		return StageOutputSide{NotCompared: fmt.Sprintf("foreach over %q: %v", st.Foreach.From, err)}
	}
	varName := st.Foreach.Var
	if varName == "" {
		varName = "item"
	}
	side := StageOutputSide{Foreach: true, Items: len(items)}
	var b strings.Builder
	for i, item := range items {
		if i >= maxForeachItemsToCompare {
			fmt.Fprintf(&b, "... %d more item(s) not compared (cap: %d)\n", len(items)-i, maxForeachItemsToCompare)
			break
		}
		extra := map[string]any{varName: item, "i": i}
		rel, rerr := renderTemplate(st.ID+":output", st.Output, st.Inputs, r.inputs, prior, r.path, extra)
		if rerr != nil {
			fmt.Fprintf(&b, "item %d: (output path did not render: %v)\n", i, rerr)
			continue
		}
		// Third of the run-dir-escape sites, and the one a fan-out
		// reaches: the per-item template is as capable of climbing out of
		// the run dir as the plain one, and this path stats and hashes
		// what it resolves.
		if cerr := ensureUnderRunDir(rel); cerr != nil {
			fmt.Fprintf(&b, "item %d: (refused: output path escapes the run dir)\n", i)
			continue
		}
		full := filepath.Join(r.path, rel)
		info, serr := os.Stat(full)
		if serr != nil {
			fmt.Fprintf(&b, "%s  (missing)\n", rel)
			continue
		}
		side.Size += info.Size()
		if !withDigests {
			fmt.Fprintf(&b, "%s  %d B\n", rel, info.Size())
			continue
		}
		sum, herr := hashFileStreaming(full)
		if herr != nil {
			fmt.Fprintf(&b, "%s  %d B  (unreadable: %v)\n", rel, info.Size(), herr)
			continue
		}
		fmt.Fprintf(&b, "%s  %d B  sha256:%s\n", rel, info.Size(), shortHash(sum))
	}
	side.Content = b.String()
	return side
}

// diffForeachItems recovers a fan-out stage's item list from the upstream
// stage's ON-DISK output, which stagePriorOutputs has already read. It
// mirrors the executor's resolveForeachItems (array, or {"items": [...]})
// so the differ enumerates the same items the run did.
func diffForeachItems(r *diffRun, st *Stage, prior map[string]*stageResult) ([]any, error) {
	from := st.Foreach.From
	res, ok := prior[from]
	if !ok || res == nil || strings.TrimSpace(res.Output) == "" {
		return nil, fmt.Errorf("upstream output is not readable in this run dir")
	}
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Output)), &raw); err != nil {
		return nil, fmt.Errorf("upstream output is not JSON")
	}
	switch v := raw.(type) {
	case []any:
		return v, nil
	case map[string]any:
		inner, ok := v["items"].([]any)
		if !ok {
			return nil, fmt.Errorf(`upstream object has no "items" array`)
		}
		return inner, nil
	default:
		return nil, fmt.Errorf("upstream output is %T, not a JSON array", raw)
	}
}

// hashFileStreaming sha256s a file without holding it in memory.
func hashFileStreaming(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
	// The LCS table below is (n+1)*(m+1) ints with nothing bounding
	// either dimension. The comment on lineHunks reasoned about PROMPTS,
	// but unifiedDiff is also called on stage OUTPUT content — which is
	// LLM-generated documents — and looksTextual only samples 8 KiB, so
	// an arbitrarily large .md or .log reaches it. Measured: 622 KB of
	// text (20k lines) allocated 3.3 GB, a clean 4x per doubling; two
	// 2 MB outputs project to ~34 GB on a box that also hosts live model
	// servers. A stated ceiling beats an OOM kill.
	if n, m := len(aLines), len(bLines); n > maxDiffCells || m > maxDiffCells || (n+1)*(m+1) > maxDiffCells {
		return tooLargeDiff(a, b, aLines, bLines, labelA, labelB)
	}
	hunks := lineHunks(aLines, bLines)
	var out strings.Builder
	if len(hunks) == 0 {
		// a != b, yet every LINE matched. The difference is below line
		// granularity: splitLinesKeepEmpty strips exactly one trailing
		// newline, so "x\n" and "x" both become ["x"].
		//
		// Returning "" here is the sentinel for "no change", and every
		// caller reads it that way — compareStages stores it, and the
		// renderer then prints "output: (identical)" for two outputs
		// that are NOT identical. Emit the difference git emits for
		// the same case instead, so a real change is never rendered as
		// its own absence.
		fmt.Fprintf(&out, "--- %s\n", labelA)
		fmt.Fprintf(&out, "+++ %s\n", labelB)
		out.WriteString("@@ trailing-newline @@\n")
		if strings.HasSuffix(a, "\n") {
			out.WriteString("-\\ file ends with a newline\n")
		} else {
			out.WriteString("-\\ No newline at end of file\n")
		}
		if strings.HasSuffix(b, "\n") {
			out.WriteString("+\\ file ends with a newline\n")
		} else {
			out.WriteString("+\\ No newline at end of file\n")
		}
		return out.String()
	}
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

// maxDiffCells bounds the LCS table at ~4M ints (~32 MB on a 64-bit
// build), which is roughly a 2000x2000-line diff. Above it the two sides
// are reported by size and digest instead.
const maxDiffCells = 4_000_000

// tooLargeDiff is the reported form of "these differ and I will not build
// a quadratic table to say how". It keeps the unified-diff frame so
// anything parsing the output still sees a well-formed header, and it
// carries the line count, byte size and digest of each side — enough to
// confirm they differ and to fetch the two files if the user wants a real
// diff.
func tooLargeDiff(a, b string, aLines, bLines []string, labelA, labelB string) string {
	sumA := sha256.Sum256([]byte(a))
	sumB := sha256.Sum256([]byte(b))
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n", labelA)
	fmt.Fprintf(&out, "+++ %s\n", labelB)
	out.WriteString("@@ too large to diff inline @@\n")
	fmt.Fprintf(&out, "-%d lines, %d bytes, sha256:%s\n", len(aLines), len(a), shortHash(hex.EncodeToString(sumA[:])))
	fmt.Fprintf(&out, "+%d lines, %d bytes, sha256:%s\n", len(bLines), len(b), shortHash(hex.EncodeToString(sumB[:])))
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
// The implementation is O(n*m) in both time and MEMORY, which is why its
// only caller refuses to enter it above maxDiffCells: the sizes "we see
// in practice" are prompts, and the same function is handed stage output
// content, which has no bound of its own.
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
	// One loud banner rather than N silent stage blocks: without a parsed
	// snapshot there is no stage list, no prompt template and no output
	// path, so every per-stage comparison below degrades to nothing.
	for label, side := range map[string]RunSide{"A": r.RunA, "B": r.RunB} {
		switch {
		case side.SnapshotUnparsed:
			fmt.Fprintf(w, "note: run %s's pipeline.yaml.snapshot could not be parsed — its prompts and outputs are NOT compared below\n", label)
		case side.SnapshotMissing:
			fmt.Fprintf(w, "note: run %s has no pipeline.yaml.snapshot — its prompts and outputs are NOT compared below\n", label)
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, bold("=== pipeline.yaml ==="))
	switch {
	case r.RunA.SnapshotMissing && r.RunB.SnapshotMissing:
		fmt.Fprintln(w, dim("(not compared: no pipeline.yaml.snapshot in either run)"))
	case r.RunA.SnapshotMissing:
		fmt.Fprintln(w, dim("(not compared: no pipeline.yaml.snapshot in run A)"))
	case r.RunB.SnapshotMissing:
		fmt.Fprintln(w, dim("(not compared: no pipeline.yaml.snapshot in run B)"))
	case r.PipelineYAMLDiff == "":
		fmt.Fprintln(w, dim("(identical)"))
	}
	if r.PipelineYAMLDiff != "" {
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
	switch {
	case r.RunA.InputsMissing && r.RunB.InputsMissing:
		fmt.Fprintln(w, dim("(not compared: no inputs.json in either run)"))
	case r.RunA.InputsMissing:
		fmt.Fprintln(w, dim("(not compared: no inputs.json in run A)"))
	case r.RunB.InputsMissing:
		fmt.Fprintln(w, dim("(not compared: no inputs.json in run B)"))
	case !changed:
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
	// A stated reason first: "not compared" must never look like "same".
	if a.NotCompared != "" || b.NotCompared != "" {
		reason := a.NotCompared
		if reason == "" {
			reason = b.NotCompared
		}
		fmt.Fprintf(w, "output: (not compared: %s)\n", reason)
		if a.Size > 0 || b.Size > 0 {
			fmt.Fprintf(w, "  A: %s\n", formatBinarySide(a))
			fmt.Fprintf(w, "  B: %s\n", formatBinarySide(b))
		}
		return
	}
	if a.Foreach || b.Foreach {
		fmt.Fprintf(w, "output (foreach, %d item(s) -> %d item(s)):\n", a.Items, b.Items)
		if s.OutputDiff != "" {
			writeColoredDiff(w, s.OutputDiff, useColor)
		} else {
			fmt.Fprintln(w, dim("(every item identical: same paths, sizes and digests)"))
		}
		return
	}
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
