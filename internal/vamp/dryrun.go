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
	"sort"
	"strings"
)

// dryRunForeachStubItems is the canned 2-item array we substitute for a
// foreach.from reference whose upstream stage we did not (and will not)
// execute in dry-run mode. Two items is enough to surface per-item template
// errors and output-path collisions without inflating the printed plan.
var dryRunForeachStubItems = []any{"item-1", "item-2"}

// DryRun walks the pipeline in scheduling order and prints, for each stage,
// the plan that Run would execute — without making any network calls, without
// invoking subprocesses, and without writing output files. It surfaces
// template rendering errors, capability-mapping gaps, output-path collisions
// (across foreach items), and per-stage type-specific shape problems
// (e.g. missing workflow file, voice .onnx not found, ffmpeg binary not on
// PATH).
//
// The exit semantics mirror Run: any fatal problem returns an error; less
// severe issues (missing voice model when audio binary itself is reachable,
// etc.) are surfaced as warnings printed to Log but do not fail the dry run.
// A final "dry-run: N errors, M warnings" summary is always emitted.
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
			return fmt.Errorf("dry-run: scheduler deadlock (%d stages remain with unmet dependencies)", len(indeg))
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
		for _, st := range ready {
			delete(indeg, st.ID)
			stageIdx++
			if err := state.dryRunStage(ctx, st, stageIdx, totalStages); err != nil {
				// Per-stage fatal errors abort the whole dry-run with a clear
				// error. (Per-stage warnings, by contrast, are recorded
				// in state.warnings and printed inline; they don't abort.)
				return err
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
	e.dryRunLogf("dry-run: %d errors, %d warnings.", state.errors, state.warnings)
	return nil
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
	// used the canned stub. The map is keyed by the consumer stage id so the
	// plan print can flag exactly which stages relied on stubbed data.
	stubbedFor map[string]bool
	errors     int
	warnings   int
}

// dryRunStage prints the plan for one stage and seeds state.stageOuts with a
// synthetic output so downstream stages can render templates that reference
// `.stages.<id>.output` / `.outputs`.
func (s *dryRunState) dryRunStage(ctx context.Context, st *Stage, idx, total int) error {
	stageType := stageTypeOrDefault(st)
	header, err := s.formatStageHeader(st, stageType, idx, total)
	if err != nil {
		// Header errors come from capability resolution; fail fast with a
		// clean message.
		s.errors++
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
func (s *dryRunState) formatStageHeader(st *Stage, stageType StageType, idx, total int) (string, error) {
	_ = idx
	_ = total
	var b strings.Builder
	fmt.Fprintf(&b, "stage %q", st.ID)
	switch stageType {
	case StageTypeText, StageTypeComfyUI:
		// Both stage types require a capability and a vibe profile activation.
		// Resolve the capability now so a missing entry surfaces with the
		// capability name verbatim — that's the same shape Capabilities.Profile
		// returns at run time, but here we want to fail the dry-run rather
		// than continue.
		if s.executor.Capabilities == nil {
			return b.String(), fmt.Errorf("stage %s: no capabilities map loaded (cannot resolve capability %q)", st.ID, st.Capability)
		}
		profile, err := s.executor.Capabilities.Profile(st.Capability)
		if err != nil {
			return b.String(), fmt.Errorf("stage %s: %w", st.ID, err)
		}
		fmt.Fprintf(&b, " (capability: %s -> profile: %s", st.Capability, profile)
		if st.Foreach != nil {
			fmt.Fprintf(&b, ", foreach over %s", st.Foreach.From)
		}
		b.WriteString(")")
	case StageTypeAudio:
		b.WriteString(" (subprocess: piper")
		if st.Foreach != nil {
			fmt.Fprintf(&b, ", foreach over %s", st.Foreach.From)
		}
		b.WriteString(")")
	case StageTypeFFmpeg:
		b.WriteString(" (subprocess: ffmpeg")
		if st.Foreach != nil {
			fmt.Fprintf(&b, ", foreach over %s", st.Foreach.From)
		}
		b.WriteString(")")
	case StageTypeYouTube:
		b.WriteString(" (network: youtube")
		if st.Foreach != nil {
			fmt.Fprintf(&b, ", foreach over %s", st.Foreach.From)
		}
		b.WriteString(")")
	case StageTypeWebhook:
		b.WriteString(" (network: webhook")
		if st.Foreach != nil {
			fmt.Fprintf(&b, ", foreach over %s", st.Foreach.From)
		}
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
		s.errors++
		return fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	if err := s.dryRunRenderPerType(st, stageType, prior, nil); err != nil {
		s.errors++
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
		s.errors++
		return err
	}
	if stubbed {
		s.stubbedFor[st.ID] = true
		s.warnings++
		s.executor.dryRunLogf("  note: upstream %q not yet executed; using %d stub item(s) for preview", st.Foreach.From, len(items))
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
			s.errors++
			return fmt.Errorf("stage %s: render output path for item %d: %w", st.ID, i, err)
		}
		if prev, ok := seenPaths[path]; ok {
			s.errors++
			return fmt.Errorf("stage %s: foreach output path collision: items %d and %d both produce %q", st.ID, prev, i, path)
		}
		seenPaths[path] = i
		outPaths[i] = path
	}

	// Per-item plan. Cap the printed items at a small ceiling so a 1000-item
	// foreach doesn't bury the rest of the plan in noise, but always render
	// every item internally so collisions and template errors surface.
	const maxItemsToPrint = 4
	printed := 0
	for i, item := range items {
		if printed >= maxItemsToPrint && i < len(items)-1 {
			continue
		}
		s.executor.dryRunLogf("  item %d (id=%v):", i, item)
		extra := map[string]any{st.Foreach.Var: item, "i": i}
		if err := s.dryRunRenderPerType(st, stageType, prior, extra); err != nil {
			s.errors++
			return err
		}
		s.executor.dryRunLogf("    output: %s", outPaths[i])
		printed++
	}
	if len(items) > maxItemsToPrint {
		s.executor.dryRunLogf("  ... (%d more item(s) elided)", len(items)-maxItemsToPrint)
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
func (s *dryRunState) dryRunRenderPerType(st *Stage, stageType StageType, prior map[string]*stageResult, extra map[string]any) error {
	indent := "  "
	if extra != nil {
		// Foreach per-item rendering nests under the "item N" header so the
		// extra indent keeps the visual hierarchy intact.
		indent = "    "
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
		outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
		if err != nil {
			return fmt.Errorf("stage %s: render output path: %w", st.ID, err)
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
func (s *dryRunState) renderOutputPath(st *Stage, prior map[string]*stageResult, extra map[string]any) (string, error) {
	return renderTemplate(st.ID+":output", st.Output, st.Inputs, s.executor.Inputs, prior, s.executor.RunDir, extra)
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
