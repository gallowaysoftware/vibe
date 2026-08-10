package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetnotify"
)

// dryRunForeachStubItems is the canned 2-item array we substitute for a
// foreach.from reference whose upstream stage we did not (and will not)
// execute in dry-run mode. Two items is enough to surface per-item template
// errors and output-path collisions without inflating the printed plan.
var dryRunForeachStubItems = []any{"item-1", "item-2"}

// dryRunItemFieldPaths returns the dotted field paths a stage's templates
// read off its foreach item, sorted and deduplicated:
// `output: "{{.item.slug}}.md"` yields ["slug"], and
// `{{.shot.timing.seconds}}` yields ["timing.seconds"].
//
// This exists because a dry run used to fail a CORRECT pipeline. The stub
// items were plain strings, and `{{.item.slug}}` against a string is a
// TYPE error ("can't evaluate field slug in type interface {}"), not a
// missing key — so relaxing missingkey could not have helped; only an
// item that actually has the field can. The fields a stage reads are
// written down in the stage itself, so we read them from there rather
// than guessing.
//
// The scan is over the stage's JSON encoding rather than a hand-kept list
// of template-bearing fields, because a hand-kept list of the places a
// rule applies is the exact defect this package keeps re-finding: a new
// templated field would silently stop being covered. AssetFS is cleared
// first — it is an interface, it never carries a template, and it is the
// one field that could make the encoding fail.
func dryRunItemFieldPaths(st *Stage) []string {
	if st == nil || st.Foreach == nil {
		return nil
	}
	varName := st.Foreach.Var
	if varName == "" {
		varName = "item"
	}
	cp := *st
	cp.AssetFS = nil
	data, err := json.Marshal(&cp)
	if err != nil {
		// Nothing to scan: fall back to the scalar stubs, which is
		// exactly the old behaviour.
		return nil
	}
	re, err := regexp.Compile(`\.` + regexp.QuoteMeta(varName) + `((?:\.[A-Za-z_][A-Za-z0-9_]*)+)`)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		path := strings.TrimPrefix(m[1], ".")
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// dryRunObjectifyItems upgrades scalar stub items to objects carrying the
// fields the stage's own templates read, so the preview can render the
// documented object-item shape. Items that are already objects (a real
// upstream JSON array of maps) are left alone.
//
// The value bound to each field is "<last path segment>-<1-based index>",
// which keeps per-item output paths DISTINCT — a single shared placeholder
// would have made the foreach collision check fire on every correct
// pipeline, trading one false failure for another.
func dryRunObjectifyItems(st *Stage, items []any) ([]any, []string) {
	paths := dryRunItemFieldPaths(st)
	if len(paths) == 0 {
		return items, nil
	}
	out := make([]any, len(items))
	for i, item := range items {
		if _, ok := item.(map[string]any); ok {
			out[i] = item
			continue
		}
		obj := map[string]any{}
		for _, path := range paths {
			segments := strings.Split(path, ".")
			insertStubField(obj, segments, fmt.Sprintf("%s-%d", segments[len(segments)-1], i+1))
		}
		out[i] = obj
	}
	return out, paths
}

// insertStubField writes value at the nested path, creating intermediate
// maps. A shallower path that already bound a scalar where a deeper one
// needs a map loses: `.item.a` and `.item.a.b` cannot both be satisfied,
// and the deeper read is the one that errors if unsatisfied.
func insertStubField(obj map[string]any, segments []string, value string) {
	for i, seg := range segments {
		if i == len(segments)-1 {
			if _, exists := obj[seg].(map[string]any); !exists {
				obj[seg] = value
			}
			return
		}
		next, ok := obj[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			obj[seg] = next
		}
		obj = next
	}
}

// DryRun walks the pipeline in scheduling order and prints, for each stage,
// the plan that Run would execute — without making any network calls, without
// invoking subprocesses, and without writing output files. It surfaces
// template rendering errors, capability-mapping gaps, output-path collisions
// (across foreach items), and per-stage type-specific shape problems
// (e.g. missing workflow file, voice .onnx not found, ffmpeg binary not on
// PATH).
//
// The exit semantics mirror Run: any problem that would stop the real run
// makes DryRun return a non-nil error; less severe issues (missing voice
// model when the audio binary itself is reachable, etc.) are surfaced as
// warnings printed to Log but do not fail the dry run.
//
// A stage that cannot be planned does NOT stop the walk. A dry run exists
// to find everything wrong in one pass, and this one used to abort at the
// first defect — which, combined with a stub-item bug and four unhandled
// stage types, meant the operator fixed one problem per invocation on
// pipelines that were often not even broken. Each failed stage prints its
// error, is counted, gets a synthetic output seeded so its dependents can
// still be planned, and the accumulated errors are returned as one at the
// end. That is also what makes the `errors` counter in the summary mean
// something: every increment used to be followed immediately by a return
// that unwound past the summary line, so it could only ever print 0.
//
// A final "dry-run: N errors, M warnings" summary is always emitted —
// including on the failing path, where it previously was not emitted at
// all.
//
// DryRun never touches Vibe (no EnsureActive, no inference, no comfyui
// submit, no piper/ffmpeg/youtube). It does perform read-only filesystem
// operations: stat prompt_file paths, stat workflow JSON files, load and
// parameter-substitute workflows in memory, look up ffmpeg/piper on PATH.
func (e *Executor) DryRun(ctx context.Context) error {
	if e.Pipeline == nil {
		return errors.New("dry-run: pipeline is nil")
	}
	// Treat an empty PipelineDir defensively the same way Run would —
	// relative prompt_file / workflow paths simply resolve against the cwd
	// when PipelineDir is unset, which matches the runtime behaviour.
	state := &dryRunState{
		executor:   e,
		stageOuts:  make(map[string]*stageResult, len(e.Pipeline.Stages)),
		stubbedFor: make(map[string]bool),
	}
	// Pre-build the stage-id lookup so a stage's prior-output snapshot can be
	// built from explicit inputs in the same way the real runner does.
	byID := make(map[string]*Stage, len(e.Pipeline.Stages))
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		byID[st.ID] = st
	}

	// Schedule in topological waves, exactly like Run: stages whose inputs are
	// all "resolved" run next. In dry-run we mark a stage resolved by inserting
	// a placeholder stageResult into state.stageOuts. The placeholder text is
	// the stage id so downstream template renders produce stable, debuggable
	// strings.
	indeg := make(map[string]int, len(e.Pipeline.Stages))
	for i := range e.Pipeline.Stages {
		st := &e.Pipeline.Stages[i]
		indeg[st.ID] = len(st.Inputs)
	}

	stageIdx := 0
	totalStages := len(e.Pipeline.Stages)
	for len(indeg) > 0 {
		var ready []*Stage
		for id, deg := range indeg {
			if deg == 0 {
				ready = append(ready, byID[id])
			}
		}
		if len(ready) == 0 {
			// A deadlock is not a per-stage defect: there is no next
			// stage to walk to, so this one does abort. The summary is
			// still emitted first, because "always emitted" is what the
			// doc above promises.
			e.dryRunLogf("dry-run: %d errors, %d warnings.", state.errors+1, state.warnings)
			return fmt.Errorf("dry-run: scheduler deadlock (%d stages remain with unmet dependencies)", len(indeg))
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		for _, st := range ready {
			delete(indeg, st.ID)
			stageIdx++
			if err := state.dryRunStage(ctx, st, stageIdx, totalStages); err != nil {
				// Report it, count it, keep walking. Seeding a synthetic
				// output for the failed stage is what lets its dependents
				// be planned instead of collapsing into a cascade of
				// "input has no output yet" noise.
				state.errors++
				state.failures = append(state.failures, err)
				e.dryRunLogf("  error: %v", err)
				if _, seeded := state.stageOuts[st.ID]; !seeded {
					state.stageOuts[st.ID] = &stageResult{Output: state.syntheticOutputFor(st)}
				}
			}
			// Decrement indegree of dependents.
			for _, other := range e.Pipeline.Stages {
				if other.ID == st.ID {
					continue
				}
				for _, dep := range other.Inputs {
					if dep == st.ID {
						if _, ok := indeg[other.ID]; ok {
							indeg[other.ID]--
						}
					}
				}
			}
		}
	}
	// stubbedFor was write-only state with a doc comment promising a flag
	// that did not exist ("so the plan print can flag exactly which stages
	// relied on stubbed data"). It is the plan's most important caveat:
	// the previews of these stages rest on stand-in items, so their item
	// COUNT and their per-item paths are illustrative, not predictions.
	if len(state.stubbedFor) > 0 {
		stubbed := make([]string, 0, len(state.stubbedFor))
		for id := range state.stubbedFor {
			stubbed = append(stubbed, id)
		}
		sort.Strings(stubbed)
		e.dryRunLogf("dry-run: previewed with stand-in foreach data: %s (item count and per-item paths come from the real upstream output)", strings.Join(stubbed, ", "))
	}
	e.dryRunLogf("dry-run: %d errors, %d warnings.", state.errors, state.warnings)
	// errors.Join keeps every failure's text, so a caller that only reads
	// the returned error learns as much as one reading the plan.
	return errors.Join(state.failures...)
}

// dryRunState carries the per-DryRun mutable bookkeeping. It is intentionally
// not part of the Executor itself so a future concurrent dry-run (e.g. for
// multi-pipeline batch checks) doesn't share state across executors.
type dryRunState struct {
	executor *Executor
	// stageOuts mirrors Executor.stageOutputs but is only populated with
	// dry-run synthetic outputs (the stage id and a stub JSON array for
	// foreach producers) so downstream template renders resolve.
	stageOuts map[string]*stageResult
	// stubbedFor records foreach stages whose upstream was not run (in this
	// dry-run, the upstream's real output is never produced) and therefore
	// used the canned stub. The map is keyed by the consumer stage id; the
	// per-stage note in the plan is written from it at the point the stub
	// is chosen, and it is kept as state so a later summary can name the
	// stages whose preview rested on stand-in data.
	stubbedFor map[string]bool
	// failures accumulates one error per stage that could not be planned.
	// DryRun keeps walking and joins them at the end: a dry run that stops
	// at the first defect makes the operator iterate one defect per
	// invocation.
	failures []error
	errors   int
	warnings int
}

// dryRunStage prints the plan for one stage and seeds state.stageOuts with a
// synthetic output so downstream stages can render templates that reference
// `.stages.<id>.output` / `.outputs`.
func (s *dryRunState) dryRunStage(ctx context.Context, st *Stage, idx, total int) error {
	stageType := stageTypeOrDefault(st)
	header, err := s.formatStageHeader(st, stageType, idx, total)
	if err != nil {
		// Header errors come from capability resolution. The header line
		// is printed anyway (it names the stage) so the error that
		// follows has something to attach to.
		s.executor.dryRunLogf("[%d/%d] %s", idx, total, header)
		return err
	}
	s.executor.dryRunLogf("[%d/%d] %s", idx, total, header)

	if st.Foreach != nil {
		return s.dryRunForeachStage(ctx, st, stageType)
	}
	return s.dryRunOrdinaryStage(ctx, st, stageType)
}

// formatStageHeader builds the "stage \"<id>\" (...) ->" line that opens each
// stage's plan block. For profile-bearing stage types we resolve the
// capability to a vibe profile and include it in the header so a missing
// mapping fails the dry-run early with a precise error.
//
// Every entry in allStageTypes has an arm. The four that did not —
// compact, pandoc, mix and short — reached `default:` and returned
// `unknown stage type %q`, which dryRunStage propagated and DryRun
// returned, so a pipeline containing any of them could not be dry-run AT
// ALL. They are also the four most expensive stage types in the package
// (EPUB conversion, m4b assembly, vertical-video assembly, context
// compaction), i.e. precisely the ones a dry run exists to protect, and
// Validate accepts every one of them — so the operator got a green
// `vamp validate` and a red `vamp run --dry-run` on the same file,
// phrased as their mistake. TestEveryStageTypeIsPreviewedByDryRun and
// TestStageTypeSwitchesAreExhaustive are what keep the next stage type
// from landing here.
func (s *dryRunState) formatStageHeader(st *Stage, stageType StageType, idx, total int) (string, error) {
	_ = idx
	_ = total
	var b strings.Builder
	fmt.Fprintf(&b, "stage %q", st.ID)
	// foreach is orthogonal to type: five arms restated the same suffix,
	// and the four new ones would have made nine copies of it.
	foreach := func() {
		if st.Foreach != nil {
			fmt.Fprintf(&b, ", foreach over %s", st.Foreach.From)
		}
	}
	switch stageType {
	case StageTypeText, StageTypeComfyUI, StageTypeCompact:
		// These stage types require a capability and a vibe profile
		// activation (stageRequiresVibeProfile agrees: compact makes LLM
		// calls of its own). Resolve the capability now so a missing entry
		// surfaces with the capability name verbatim — that's the same
		// shape Capabilities.Profile returns at run time, but here we want
		// to fail the dry-run rather than continue.
		if s.executor.Capabilities == nil {
			return b.String(), fmt.Errorf("stage %s: no capabilities map loaded (cannot resolve capability %q)", st.ID, st.Capability)
		}
		profile, err := s.executor.Capabilities.Profile(st.Capability)
		if err != nil {
			return b.String(), fmt.Errorf("stage %s: %w", st.ID, err)
		}
		fmt.Fprintf(&b, " (capability: %s -> profile: %s", st.Capability, profile)
		foreach()
		b.WriteString(")")
	case StageTypeAudio:
		b.WriteString(" (subprocess: piper")
		foreach()
		b.WriteString(")")
	case StageTypeFFmpeg:
		b.WriteString(" (subprocess: ffmpeg")
		foreach()
		b.WriteString(")")
	case StageTypePandoc:
		b.WriteString(" (subprocess: pandoc")
		foreach()
		b.WriteString(")")
	case StageTypeMix:
		b.WriteString(" (subprocess: ffmpeg, m4b mix")
		foreach()
		b.WriteString(")")
	case StageTypeShort:
		b.WriteString(" (subprocess: ffmpeg, vertical short")
		foreach()
		b.WriteString(")")
	case StageTypeYouTube:
		b.WriteString(" (network: youtube")
		foreach()
		b.WriteString(")")
	case StageTypeWebhook:
		b.WriteString(" (network: webhook")
		foreach()
		b.WriteString(")")
	case StageTypeConfirm:
		b.WriteString(" (human-in-the-loop: confirm")
		if st.Timeout > 0 {
			fmt.Fprintf(&b, ", timeout %s", st.Timeout)
		}
		b.WriteString(")")
	case StageTypeRender:
		b.WriteString(" (template render, no LLM)")
	default:
		return b.String(), fmt.Errorf("stage %s: unknown stage type %q", st.ID, stageType)
	}
	// run_when: the real scheduler gates every stage on it (exec.go's
	// runWhenOrDefault + the skip branch), and the plan used to print
	// every stage as unconditional — so a `run_when: failure` cleanup
	// stage was listed exactly like the stages that always run, and the
	// printed stage count disagreed with the real one for any pipeline
	// using conditional stages. We print the declared gate rather than
	// evaluating it: the evaluation needs a pipeline status that does not
	// exist yet in a dry run, and over-reporting a stage is the safe
	// direction.
	switch rw := strings.TrimSpace(st.RunWhen); {
	case rw == "" || rw == RunWhenSuccess:
		// The default. Annotating it would put a gate on every line and
		// make the ones that matter invisible.
	case hasRunWhenTemplate(st):
		fmt.Fprintf(&b, " [run_when: %s — evaluated at run time]", rw)
	case rw == RunWhenFailure:
		fmt.Fprintf(&b, " [run_when: %s — SKIPPED unless an earlier stage failed]", rw)
	case rw == RunWhenAlways:
		fmt.Fprintf(&b, " [run_when: %s — runs even after an earlier stage failed]", rw)
	default:
		fmt.Fprintf(&b, " [run_when: %s]", rw)
	}
	return b.String(), nil
}

// dryRunOrdinaryStage renders prompt/argv/workflow templates and the output
// path for a non-foreach stage and prints them. It seeds state.stageOuts
// with a synthetic stageResult (the stage id) so dependents can render.
func (s *dryRunState) dryRunOrdinaryStage(ctx context.Context, st *Stage, stageType StageType) error {
	_ = ctx
	prior := s.snapshotPrior(st.Inputs)
	outPath, err := s.renderOutputPath(st, prior, nil)
	if err != nil {
		return fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	if err := s.dryRunRenderPerType(st, stageType, prior, nil, outPath); err != nil {
		return err
	}
	s.executor.dryRunLogf("  output: %s", outPath)
	// Seed downstream visibility. We deliberately use the stage id (not the
	// rendered output path) as the synthetic .output: dependents that embed
	// `.stages.X.output` into prompts then produce predictable rendered text
	// rather than a real filesystem path that might confuse a user reading
	// the plan.
	syn := s.syntheticOutputFor(st)
	s.stageOuts[st.ID] = &stageResult{Output: syn}
	return nil
}

// dryRunForeachStage handles the fan-out preview for a foreach stage. The
// items come from the upstream stage's synthesised output when present
// (e.g. an upstream text stage with OutputFormat=json whose synthetic
// .output we set to a JSON array literal), otherwise we substitute
// dryRunForeachStubItems and warn so the user knows.
func (s *dryRunState) dryRunForeachStage(ctx context.Context, st *Stage, stageType StageType) error {
	_ = ctx
	items, stubbed, err := s.resolveDryRunForeachItems(st)
	if err != nil {
		return err
	}
	if stubbed {
		s.stubbedFor[st.ID] = true
		s.warnings++
		s.executor.dryRunLogf("  note: upstream %q not yet executed; using %d stub item(s) for preview", st.Foreach.From, len(items))
	}
	// Every item in a dry run is synthetic — the upstream never ran — so
	// the stand-ins have to have the SHAPE the stage's templates expect.
	// `output: "{{.item.slug}}.md"` is the pattern ensureUnderRunDir's own
	// comment gives as the motivating example, and against a string stub
	// it is a template error that used to fail the whole dry run on a
	// perfectly good pipeline.
	items, fields := dryRunObjectifyItems(st, items)
	if len(fields) > 0 {
		s.stubbedFor[st.ID] = true
		s.executor.dryRunLogf("  note: item fields %s are stubs synthesised from this stage's own templates; the real values come from %q", strings.Join(fields, ", "), st.Foreach.From)
	}

	// Pre-render each item's output path so we surface collisions at dry-run
	// time even though no real writes will happen. Mirrors the executor's
	// own pre-flight collision check.
	outPaths := make([]string, len(items))
	seenPaths := make(map[string]int, len(items))
	prior := s.snapshotPrior(st.Inputs)
	for i, item := range items {
		extra := map[string]any{st.Foreach.Var: item, "i": i}
		path, err := s.renderOutputPath(st, prior, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render output path for item %d: %w", st.ID, i, err)
		}
		if prev, ok := seenPaths[path]; ok {
			return fmt.Errorf("stage %s: foreach output path collision: items %d and %d both produce %q", st.ID, prev, i, path)
		}
		seenPaths[path] = i
		outPaths[i] = path
	}

	// Per-item plan. Cap the printed items at a small ceiling so a 1000-item
	// foreach doesn't bury the rest of the plan in noise. Every item's
	// OUTPUT PATH is rendered above regardless of the cap, so collisions
	// and output-template errors surface for all of them; the per-type
	// preview below runs only for the items that are printed.
	const maxItemsToPrint = 4
	printed := 0
	for i, item := range items {
		if printed >= maxItemsToPrint && i < len(items)-1 {
			continue
		}
		s.executor.dryRunLogf("  item %d (id=%v):", i, item)
		extra := map[string]any{st.Foreach.Var: item, "i": i}
		if err := s.dryRunRenderPerType(st, stageType, prior, extra, outPaths[i]); err != nil {
			return err
		}
		s.executor.dryRunLogf("    output: %s", outPaths[i])
		printed++
	}
	// Count what was actually skipped rather than assuming it is
	// len(items)-maxItemsToPrint. The loop above ALWAYS prints the last
	// item (the `i < len(items)-1` clause), so that arithmetic
	// over-counted by one for every foreach longer than the cap — and
	// for exactly maxItemsToPrint+1 items it announced an elision when
	// nothing had been elided at all.
	if elided := len(items) - printed; elided > 0 {
		s.executor.dryRunLogf("  ... (%d more item(s) elided)", elided)
	}

	// Seed downstream visibility with the per-item synthesised outputs and the
	// newline-joined .output. Dependents that range over .outputs see N entries.
	outs := make([]string, len(items))
	for i := range items {
		outs[i] = fmt.Sprintf("%s[%d]", st.ID, i)
	}
	combined := strings.Join(outs, "\n\n")
	s.stageOuts[st.ID] = &stageResult{Output: combined, Outputs: outs}
	return nil
}

// dryRunRenderPerType performs the per-stage-type rendering preview and
// prints the result. prior is the dependency snapshot already restricted to
// the stage's declared inputs; extra carries the foreach item bindings when
// rendering for one item.
//
// outRel is the stage's ALREADY-RENDERED output path, relative to RunDir.
// It is a parameter rather than something this function re-renders because
// the ffmpeg arm used to re-render st.Output through a second, unguarded
// path — a copy of the containment rule renderOutputPath's own comment
// claims is "shared code, not a copy, so the two can't diverge".
//
// Five arms were missing, and the missing ones fail differently from a
// missing header arm: nothing is printed and nothing errors, so the stage
// appears in the plan as a header and an output path with no account of
// what it would actually do. webhook was the fifth — a dry run of a
// notification stage said nothing about where the notification goes.
func (s *dryRunState) dryRunRenderPerType(st *Stage, stageType StageType, prior map[string]*stageResult, extra map[string]any, outRel string) error {
	indent := "  "
	if extra != nil {
		// Foreach per-item rendering nests under the "item N" header so the
		// extra indent keeps the visual hierarchy intact.
		indent = "    "
	}
	render := func(what, raw string) (string, error) {
		return renderTemplate(st.ID+":"+what, raw, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
	}
	switch stageType {
	case StageTypeText:
		prompt, err := renderPrompt(st, s.executor.PipelineDir, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render prompt: %w", st.ID, err)
		}
		s.executor.dryRunLogf("%sprompt (%d chars):", indent, len(prompt))
		s.dryRunWriteIndented(indent+"  ", prompt)
	case StageTypeComfyUI:
		workflow, err := loadWorkflow(st, s.executor.PipelineDir)
		if err != nil {
			return fmt.Errorf("stage %s: %w", st.ID, err)
		}
		// Build a StageInput-shaped record for applyParameters; it only uses
		// PipelineDir, Inputs, Prior, and RunDir.
		in := StageInput{
			Stage:       st,
			Inputs:      s.executor.Inputs,
			Prior:       prior,
			RunDir:      s.executor.RunDir,
			PipelineDir: s.executor.PipelineDir,
		}
		if err := applyParameters(workflow, st, in, extra); err != nil {
			return fmt.Errorf("stage %s: %w", st.ID, err)
		}
		// Print only the node inputs we actually rewrote so the plan stays
		// scannable; iteration is over the user's Parameters map in sorted
		// order for stable test output.
		keys := make([]string, 0, len(st.Parameters))
		for k := range st.Parameters {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s.executor.dryRunLogf("%sworkflow: %s", indent, st.Workflow)
		s.executor.dryRunLogf("%sparameters:", indent)
		for _, key := range keys {
			nodeID, inputName, _ := strings.Cut(key, ".")
			node, ok := workflow[nodeID].(map[string]any)
			if !ok {
				continue
			}
			inputs, ok := node["inputs"].(map[string]any)
			if !ok {
				continue
			}
			s.executor.dryRunLogf("%s  %s = %v", indent, key, inputs[inputName])
		}
	case StageTypeAudio:
		// Render the text and resolve the voice path. We stat the voice
		// model so a missing .onnx is surfaced as a warning (since the user
		// may legitimately not have voices installed on this machine yet),
		// not a hard error.
		text, err := renderTemplate(st.ID+":text", st.Text, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render text: %w", st.ID, err)
		}
		voicesDir := st.VoicesDir
		if voicesDir == "" {
			voicesDir = defaultVoicesDirPath
		}
		voicesDir = expandAudioTilde(voicesDir)
		voicePath := filepath.Join(voicesDir, st.Voice+".onnx")
		binary := st.Binary
		if binary == "" {
			binary = "piper"
		}
		s.executor.dryRunLogf("%spiper:", indent)
		s.executor.dryRunLogf("%s  binary: %s", indent, binary)
		s.executor.dryRunLogf("%s  voice:  %s", indent, voicePath)
		s.executor.dryRunLogf("%s  text (%d chars):", indent, len(text))
		s.dryRunWriteIndented(indent+"    ", text)
		if _, err := os.Stat(voicePath); err != nil {
			s.warnings++
			s.executor.dryRunLogf("%s  warning: voice model %s not found (%v)", indent, voicePath, err)
		}
		if _, err := exec.LookPath(binary); err != nil {
			s.warnings++
			s.executor.dryRunLogf("%s  warning: piper binary %q not on PATH", indent, binary)
		}
	case StageTypeFFmpeg:
		args := make([]string, 0, len(st.FFmpegArgs)+2)
		for i, raw := range st.FFmpegArgs {
			rendered, err := renderTemplate(fmt.Sprintf("%s:arg[%d]", st.ID, i), raw, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
			if err != nil {
				return fmt.Errorf("stage %s: render ffmpeg_args[%d]: %w", st.ID, i, err)
			}
			args = append(args, rendered)
		}
		// Mirror the real executor's argv assembly: append `-y <abs out>` so
		// the printed argv matches what the subprocess would receive.
		outAbs := filepath.Join(s.executor.RunDir, outRel)
		args = append(args, "-y", outAbs)
		binary := st.Binary
		if binary == "" {
			binary = "ffmpeg"
		}
		s.executor.dryRunLogf("%sargv: %s", indent, formatArgv(append([]string{binary}, args...)))
		if _, err := exec.LookPath(binary); err != nil {
			s.warnings++
			s.executor.dryRunLogf("%s  warning: ffmpeg binary %q not on PATH", indent, binary)
		}
	case StageTypeConfirm:
		message, err := renderTemplate(st.ID+":message", st.Message, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render message: %w", st.ID, err)
		}
		s.executor.dryRunLogf("%smessage (%d chars):", indent, len(message))
		s.dryRunWriteIndented(indent+"  ", message)
		if st.Timeout > 0 {
			s.executor.dryRunLogf("%stimeout: %s", indent, st.Timeout)
		}
	case StageTypeYouTube:
		title, err := renderTemplate(st.ID+":title", st.Title, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render title: %w", st.ID, err)
		}
		description, err := renderTemplate(st.ID+":description", st.Description, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render description: %w", st.ID, err)
		}
		video, err := renderTemplate(st.ID+":video", st.Video, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render video path: %w", st.ID, err)
		}
		privacy := st.Privacy
		if privacy == "" {
			privacy = "private"
		}
		s.executor.dryRunLogf("%syoutube:", indent)
		s.executor.dryRunLogf("%s  video:       %s", indent, video)
		s.executor.dryRunLogf("%s  title:       %s", indent, title)
		s.executor.dryRunLogf("%s  description (%d chars):", indent, len(description))
		s.dryRunWriteIndented(indent+"    ", description)
		s.executor.dryRunLogf("%s  privacy:     %s", indent, privacy)
	case StageTypeRender:
		rendered, err := renderPrompt(st, s.executor.PipelineDir, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render template: %w", st.ID, err)
		}
		s.executor.dryRunLogf("%stemplate output (%d chars):", indent, len(rendered))
		s.dryRunWriteIndented(indent+"  ", rendered)
	case StageTypeWebhook:
		return s.dryRunWebhook(st, prior, extra, indent)
	case StageTypeCompact:
		source, err := render("source", st.Source)
		if err != nil {
			return fmt.Errorf("stage %s: render source: %w", st.ID, err)
		}
		chunkChars := st.ChunkChars
		if chunkChars <= 0 && st.TargetChars > 0 {
			chunkChars = st.TargetChars * defaultCompactChunkMultiple
		}
		s.executor.dryRunLogf("%scompact:", indent)
		s.executor.dryRunLogf("%s  target_chars: %d", indent, st.TargetChars)
		s.executor.dryRunLogf("%s  chunk_chars:  %d", indent, chunkChars)
		if st.Preserve != "" {
			s.executor.dryRunLogf("%s  preserve:     %s", indent, st.Preserve)
		}
		s.executor.dryRunLogf("%s  source (%d chars):", indent, len(source))
		s.dryRunWriteIndented(indent+"    ", source)
		// The source a dry run can see is built from stand-ins for
		// upstream outputs, so its LENGTH is not the real one. Say so
		// rather than let the operator read the call count as a promise.
		switch {
		case st.TargetChars <= 0:
			s.warnings++
			s.executor.dryRunLogf("%s  warning: target_chars is %d; the real run refuses a compact stage without a positive target", indent, st.TargetChars)
		case len(source) <= st.TargetChars:
			s.executor.dryRunLogf("%s  note: this stand-in source already fits target_chars, so no LLM call is previewed — the real upstream output is what decides", indent)
		default:
			s.executor.dryRunLogf("%s  llm calls (first pass over the stand-in source): %d", indent, len(splitForCompaction(source, chunkChars)))
		}
	case StageTypePandoc:
		srcRel, err := render("source_file", st.SourceFile)
		if err != nil {
			return fmt.Errorf("stage %s: render source_file: %w", st.ID, err)
		}
		srcAbs := srcRel
		if !filepath.IsAbs(srcAbs) {
			srcAbs = filepath.Join(s.executor.RunDir, srcRel)
		}
		var coverAbs string
		if st.CoverImage != "" {
			coverRel, err := render("cover_image", st.CoverImage)
			if err != nil {
				return fmt.Errorf("stage %s: render cover_image: %w", st.ID, err)
			}
			coverAbs = coverRel
			if !filepath.IsAbs(coverAbs) {
				coverAbs = filepath.Join(s.executor.RunDir, coverRel)
			}
		}
		// Mirror the executor's engine choice exactly, including the
		// docker fallback — which is a thing an operator wants to learn
		// from a dry run and not from a surprise `docker pull` at 02:00.
		useDocker := false
		binary := st.Binary
		switch binary {
		case "":
			if _, err := exec.LookPath("pandoc"); err == nil {
				binary = "pandoc"
			} else {
				useDocker = true
				binary = "docker"
			}
		case "docker":
			useDocker = true
		}
		containerName := ""
		if useDocker {
			containerName = pandocContainerName(s.executor.RunDir, st.ID, dryRunItemIdx(extra))
		}
		outAbs := filepath.Join(s.executor.RunDir, outRel)
		args := buildPandocArgs(st, srcAbs, outAbs, coverAbs, useDocker, s.executor.RunDir, containerName)
		s.executor.dryRunLogf("%spandoc:", indent)
		s.executor.dryRunLogf("%s  engine: %s", indent, binary)
		s.executor.dryRunLogf("%s  source: %s", indent, srcAbs)
		if coverAbs != "" {
			s.executor.dryRunLogf("%s  cover:  %s", indent, coverAbs)
		}
		s.executor.dryRunLogf("%s  argv: %s", indent, formatArgv(append([]string{binary}, args...)))
		if st.Binary == "" && useDocker {
			s.warnings++
			s.executor.dryRunLogf("%s  warning: pandoc is not on PATH; the run would fall back to docker", indent)
		}
		if _, err := exec.LookPath(binary); err != nil {
			s.warnings++
			s.executor.dryRunLogf("%s  warning: %q not on PATH", indent, binary)
		}
		// Only an ABSOLUTE source can be checked: a relative one names a
		// file this very run has not produced yet, and warning about it
		// would be noise on every correct pipeline.
		if filepath.IsAbs(srcRel) {
			if _, err := os.Stat(srcAbs); err != nil {
				s.warnings++
				s.executor.dryRunLogf("%s  warning: source_file %s not found (%v)", indent, srcAbs, err)
			}
		}
	case StageTypeMix, StageTypeShort:
		scriptRel, err := render("script_file", st.ScriptFile)
		if err != nil {
			return fmt.Errorf("stage %s: render script_file: %w", st.ID, err)
		}
		binary := st.Binary
		if binary == "" {
			binary = "ffmpeg"
		}
		loudness := st.LoudnessTarget
		if loudness == 0 {
			loudness = -16
		}
		label := "mix"
		if stageType == StageTypeShort {
			label = "short"
		}
		s.executor.dryRunLogf("%s%s:", indent, label)
		s.executor.dryRunLogf("%s  binary:      %s", indent, binary)
		s.executor.dryRunLogf("%s  script_file: %s", indent, scriptRel)
		if stageType == StageTypeShort {
			w, h, fps := normalizeVideoDims(st.ShortWidth, st.ShortHeight, st.ShortFPS)
			s.executor.dryRunLogf("%s  video:       %dx%d @%dfps (the script may set its own; the stage's values win when non-zero)", indent, w, h, fps)
			s.executor.dryRunLogf("%s  stretch_video: %t", indent, st.ShortStretchVideo)
		} else {
			if st.IntroMusic != "" {
				intro, err := render("intro_music", st.IntroMusic)
				if err != nil {
					return fmt.Errorf("stage %s: render intro_music: %w", st.ID, err)
				}
				s.executor.dryRunLogf("%s  intro_music: %s", indent, intro)
			}
			if st.OutroMusic != "" {
				outro, err := render("outro_music", st.OutroMusic)
				if err != nil {
					return fmt.Errorf("stage %s: render outro_music: %w", st.ID, err)
				}
				s.executor.dryRunLogf("%s  outro_music: %s", indent, outro)
			}
		}
		s.executor.dryRunLogf("%s  loudness:    %g LUFS", indent, loudness)
		if len(st.Metadata) > 0 {
			keys := make([]string, 0, len(st.Metadata))
			for k := range st.Metadata {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			s.executor.dryRunLogf("%s  metadata:", indent)
			for _, k := range keys {
				val, err := render("metadata:"+k, st.Metadata[k])
				if err != nil {
					return fmt.Errorf("stage %s: render metadata %s: %w", st.ID, k, err)
				}
				s.executor.dryRunLogf("%s    %s = %s", indent, k, val)
			}
		}
		if _, err := exec.LookPath(binary); err != nil {
			s.warnings++
			s.executor.dryRunLogf("%s  warning: ffmpeg binary %q not on PATH", indent, binary)
		}
		// The script is normally an upstream stage's output, so its
		// absence is the expected state and only a PRESENT one is worth
		// reporting on. The shots/segments it declares are what the real
		// run reads; a dry run cannot know them yet.
		scriptAbs := resolveInRunDir(scriptRel, s.executor.RunDir)
		if info, err := os.Stat(scriptAbs); err == nil {
			s.executor.dryRunLogf("%s  note: the script exists already (%d bytes); the real run reads its segments and their media paths from it", indent, info.Size())
		} else {
			s.executor.dryRunLogf("%s  note: the script does not exist yet — an upstream stage produces it, so its segments and their media paths cannot be checked here", indent)
		}
	}
	return nil
}

// dryRunItemIdx pulls the foreach index out of the per-item bindings so a
// preview that has to name something per item (a docker container, say)
// matches what the real run would name.
func dryRunItemIdx(extra map[string]any) int {
	if extra == nil {
		return 0
	}
	if i, ok := extra["i"].(int); ok {
		return i
	}
	return 0
}

// dryRunWebhook previews a webhook stage: where the request goes, with
// what method, which header NAMES it carries, and the rendered body.
//
// The URL is redacted through the same fleetnotify.Redact the executor's
// own log line uses. A Slack/Discord/ntfy incoming-webhook URL carries
// its bearer in the PATH, and a dry-run plan is a thing operators paste
// into bug reports; Redact keeps scheme+host and a stable 8-hex id, so
// two different endpoints still look different without the credential
// being present. Header VALUES are never printed for the same reason —
// `Authorization: Bearer …` is the whole point of the field.
func (s *dryRunState) dryRunWebhook(st *Stage, prior map[string]*stageResult, extra map[string]any, indent string) error {
	in := StageInput{
		Stage:        st,
		Inputs:       s.executor.Inputs,
		Prior:        prior,
		RunDir:       s.executor.RunDir,
		PipelineDir:  s.executor.PipelineDir,
		PipelineName: s.executor.Pipeline.Name,
	}
	url, err := renderWebhookTemplate(st, "url", st.URL, in, extra)
	if err != nil {
		return fmt.Errorf("stage %s: render url: %w", st.ID, err)
	}
	method := strings.ToUpper(st.Method)
	if method == "" {
		method = "POST"
	}
	s.executor.dryRunLogf("%swebhook:", indent)
	s.executor.dryRunLogf("%s  %s %s", indent, method, fleetnotify.Redact(url))
	if strings.TrimSpace(url) == "" {
		s.warnings++
		s.executor.dryRunLogf("%s  warning: the rendered url is empty; the real run refuses this stage", indent)
	}
	if len(st.Headers) > 0 {
		names := make([]string, 0, len(st.Headers))
		for k := range st.Headers {
			names = append(names, k)
		}
		sort.Strings(names)
		s.executor.dryRunLogf("%s  headers: %s (names only — values may carry credentials)", indent, strings.Join(names, ", "))
	}
	switch {
	case len(st.Body) > 0:
		rendered := renderBodyMap(st.Body, func(name, raw string) string {
			out, rerr := renderWebhookTemplate(st, "body:"+name, raw, in, extra)
			if rerr != nil {
				return raw
			}
			return out
		})
		data, merr := json.MarshalIndent(rendered, "", "  ")
		if merr != nil {
			return fmt.Errorf("stage %s: marshal body: %w", st.ID, merr)
		}
		s.executor.dryRunLogf("%s  body (%d bytes):", indent, len(data))
		s.dryRunWriteIndented(indent+"    ", string(data))
	case st.BodyTemplateFile != "":
		raw, rerr := readStageAsset(st, s.executor.PipelineDir, st.BodyTemplateFile)
		if rerr != nil {
			s.warnings++
			s.executor.dryRunLogf("%s  body_template_file: %s (warning: unreadable: %v)", indent, st.BodyTemplateFile, rerr)
			return nil
		}
		body, rerr := renderWebhookTemplate(st, "body_template_file", string(raw), in, extra)
		if rerr != nil {
			return fmt.Errorf("stage %s: render body_template_file: %w", st.ID, rerr)
		}
		s.executor.dryRunLogf("%s  body_template_file: %s (%d chars rendered):", indent, st.BodyTemplateFile, len(body))
		s.dryRunWriteIndented(indent+"    ", body)
	}
	return nil
}

// resolveDryRunForeachItems mirrors resolveForeachItems but tolerates a
// missing upstream output: when the upstream stage hasn't been executed in
// this dry-run (the common case for a foreach whose producer is another
// real stage), we synthesise dryRunForeachStubItems and report stubbed=true.
func (s *dryRunState) resolveDryRunForeachItems(st *Stage) ([]any, bool, error) {
	from := st.Foreach.From
	res, ok := s.stageOuts[from]
	if !ok {
		return append([]any(nil), dryRunForeachStubItems...), true, nil
	}
	rendered := strings.TrimSpace(res.Output)
	if rendered == "" {
		return append([]any(nil), dryRunForeachStubItems...), true, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(rendered), &raw); err != nil {
		// The upstream's synthetic output isn't JSON. That's fine in dry-run
		// — fall back to the stub rather than reporting a parse error a real
		// run would never see (the real upstream emits real JSON).
		return append([]any(nil), dryRunForeachStubItems...), true, nil
	}
	switch v := raw.(type) {
	case []any:
		return v, false, nil
	case map[string]any:
		inner, ok := v["items"]
		if !ok {
			return append([]any(nil), dryRunForeachStubItems...), true, nil
		}
		arr, ok := inner.([]any)
		if !ok {
			return append([]any(nil), dryRunForeachStubItems...), true, nil
		}
		return arr, false, nil
	default:
		return append([]any(nil), dryRunForeachStubItems...), true, nil
	}
}

// snapshotPrior limits the dry-run state's stage outputs to the declared
// dependency set, mirroring (*Executor).snapshotPrior. Returning a copy keeps
// the template renderer's data map free of unrelated stage references.
func (s *dryRunState) snapshotPrior(deps []string) map[string]*stageResult {
	out := make(map[string]*stageResult, len(deps))
	for _, dep := range deps {
		if res, ok := s.stageOuts[dep]; ok {
			out[dep] = res
		}
	}
	return out
}

// renderOutputPath is the dry-run analogue of (*Executor).renderOutputPath:
// it uses the dry-run state's synthesised stageOutputs rather than the live
// executor's so the render can succeed before any real stage has run.
//
// It applies the same run-dir containment rule. A dry run exists to tell
// the operator what the real run will do; one that prints
// `output: ../../etc/passwd` as part of a clean plan and then fails at
// stage 9 has answered the wrong question. The rule is shared code, not
// a copy, so the two can't diverge.
func (s *dryRunState) renderOutputPath(st *Stage, prior map[string]*stageResult, extra map[string]any) (string, error) {
	out, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
	if err != nil {
		return "", err
	}
	if err := ensureUnderRunDir(out); err != nil {
		return "", err
	}
	return out, nil
}

// syntheticOutputFor produces a placeholder string for a stage's downstream
// .output reference. For text stages whose OutputFormat is "json" we emit a
// canonical 2-element JSON array literal so a downstream foreach can parse
// it without falling back to the stub items, exercising the real foreach
// rendering path against representative data.
func (s *dryRunState) syntheticOutputFor(st *Stage) string {
	stageType := stageTypeOrDefault(st)
	if stageType == StageTypeText && st.OutputFormat == "json" {
		// Canonical small array so downstream consumers can render two
		// realistic items. Matches the same shape dryRunForeachStubItems
		// would have provided so users see consistent stub data.
		return `["item-1","item-2"]`
	}
	return fmt.Sprintf("<dry-run output of %s>", st.ID)
}

// dryRunWriteIndented writes raw text to e.Log with each line prefixed by
// indent. Empty trailing lines are preserved so multi-line prompts retain
// their structure.
func (s *dryRunState) dryRunWriteIndented(indent, body string) {
	if s.executor.Log == nil {
		return
	}
	s.executor.logMu.Lock()
	defer s.executor.logMu.Unlock()
	w := s.executor.Log
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		_, _ = io.WriteString(w, indent)
		_, _ = io.WriteString(w, line)
		if i < len(lines)-1 {
			_, _ = io.WriteString(w, "\n")
		}
	}
	_, _ = io.WriteString(w, "\n")
}

// dryRunLogf is a logf variant tailored for DryRun: it skips the timestamp
// prefix (the dry-run plan is not a log of runtime events) and falls back to
// a newline-terminated Fprintf under logMu.
func (e *Executor) dryRunLogf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	fmt.Fprintf(e.Log, format+"\n", args...)
}

// formatArgv renders an argv slice as a single space-separated line with
// arguments containing whitespace or quotes wrapped in double quotes. The
// printed form is not shell-safe (we don't intend the user to copy-paste it
// into bash); it is a readable representation of what the executor would
// pass to exec.Command.
func formatArgv(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if argvNeedsQuote(a) {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(a, `"`, `\"`))
			b.WriteByte('"')
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

func argvNeedsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\\' {
			return true
		}
	}
	return false
}
