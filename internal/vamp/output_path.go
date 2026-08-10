package vamp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file holds the two rules every file-producing stage obeys, in ONE
// copy each. Both defects they exist to prevent were born the same way:
// the rule existed, and the executors each kept their own copy of the
// code that was supposed to apply it.
//
//  1. WHERE a stage may write — inside the run dir (stageOutputPath).
//  2. WHEN its output becomes visible there — only once the producer has
//     finished and the result has been checked (beginPartialOutput /
//     finalizeOutput / writeOutputAtomically).
//
// Rule 1 was previously applied at four of the fourteen places a rendered
// `output:` template reaches the run dir, and at NONE of the seven that
// write. Rule 2 was applied on the text path only (writeFile's temp +
// rename), so every media stage wrote its container in place and a killed
// encode left a truncated file that `--resume` then accepted as a
// completed stage.
//
// The precedent is subprocess.go's command() wrapper, whose own doc
// comment names this shape: "guard in one of N call paths" is how two of
// four ffmpeg output checks went missing.

// stageOutputPath resolves the run-dir-relative path this invocation of a
// stage must write to, and refuses one that resolves outside the run dir.
//
// Executors call it instead of rendering `st.Output` themselves. When the
// runner already rendered and checked the path (executeStage does, before
// dispatch, so the check happens even for the stage types that report
// their result in StageOutput.Files and never come back for a write-back)
// that value is returned verbatim — one render per stage, not two, so the
// path in the argv and the path the runner validated cannot diverge.
//
// The fallback render is live: foreach items arrive one per Execute call
// with their own binding, so their paths cannot be pre-rendered by the
// single-stage path. They are rendered here, and checked here.
//
// The path is model-controlled in the shape that matters: a foreach item
// map is parsed from a PRIOR STAGE'S LLM output, and `output:` templates
// interpolate its fields (`output: "{{.item.slug}}.wav"`) or another
// stage's raw text (`output: "{{.stages.pick.output}}.mp4"`).
func stageOutputPath(st *Stage, in StageInput, extra map[string]any) (string, error) {
	if in.OutPath != "" {
		return in.OutPath, nil
	}
	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return "", err
	}
	if err := ensureUnderRunDir(outRel); err != nil {
		return "", err
	}
	return outRel, nil
}

// scratchPrefix marks a run-dir file as vamp's own working state rather
// than a stage's artefact. Dot-prefixed to match the scratch this package
// already writes beside the outputs (.ffmpeg-concat.*, .caption-*,
// .mix-meta.*) and, more importantly, so isScratchName can recognise it
// from the filename alone — the concat walk sees a name, not a provenance.
const scratchPrefix = ".vamp-partial."

// isScratchName reports whether a directory entry is vamp's working state.
// The walks that collect a stage's inputs by extension (concat_wavs, and
// the cache key that must hash the same set) use it to skip a partially
// written file: a scratch WAV is still a .wav, and gluing half a segment
// into the audiobook is the same defect one layer down. It fires without
// a crash, too — a sibling foreach item writing while the walk runs.
func isScratchName(name string) bool {
	return strings.HasPrefix(name, ".")
}

// partialOutputPath is where a stage's producer writes while it is still
// producing.
//
// Same directory (so the rename is atomic — a cross-filesystem rename is
// a copy), same EXTENSION (ffmpeg and pandoc both infer the container /
// output format from it, so "book.m4b.partial" would not be an m4b), and
// hidden, which is what keeps it out of the walks above.
func partialOutputPath(outAbs string) string {
	dir, base := filepath.Split(outAbs)
	return filepath.Join(dir, scratchPrefix+base)
}

// beginPartialOutput returns the path to hand the producer, having first
// removed any scratch left by an earlier attempt that died. Callers pass
// the FINAL path they want the output to land at.
func beginPartialOutput(outAbs string) string {
	tmp := partialOutputPath(outAbs)
	_ = os.Remove(tmp)
	return tmp
}

// discardPartialOutput drops the scratch file after a failed attempt.
// Best-effort by design: the stage has already failed, and a leftover
// scratch file is inert (hidden, skipped by the walks, overwritten by the
// next attempt) — worth removing, never worth failing over.
func discardPartialOutput(tmpAbs string) {
	if tmpAbs == "" {
		return
	}
	_ = os.Remove(tmpAbs)
}

// finalizeOutput publishes a completed scratch file at its final path.
//
// The non-empty check runs BEFORE the rename, so the two failures a media
// producer can report as success — exiting 0 with nothing, and exiting 0
// with a 0-byte container — never reach the output path either. That
// makes "a file exists at the stage's output path" mean "that stage
// completed", which is precisely what --resume assumes and what
// tryResumeStage has no other way to verify for a binary stage.
func finalizeOutput(stageID, what, tmpAbs, outAbs string) error {
	if err := requireNonEmptyOutput(stageID, what, tmpAbs); err != nil {
		discardPartialOutput(tmpAbs)
		return err
	}
	if err := os.Rename(tmpAbs, outAbs); err != nil {
		discardPartialOutput(tmpAbs)
		return fmt.Errorf("stage %s: %s: publish output %s: %w", stageID, what, outAbs, err)
	}
	return nil
}

// writeOutputAtomically is finalizeOutput for the one file-producing path
// that is not a subprocess: bytes already in memory (the Kokoro TTS
// response). os.WriteFile truncates the destination before it writes, so
// a kill part-way through leaves a short file at the real path — the same
// resume hazard, reached by a shorter route.
func writeOutputAtomically(stageID, what, outAbs string, data []byte) error {
	tmp := beginPartialOutput(outAbs)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		discardPartialOutput(tmp)
		return fmt.Errorf("stage %s: %s: write %s: %w", stageID, what, outAbs, err)
	}
	return finalizeOutput(stageID, what, tmp, outAbs)
}
