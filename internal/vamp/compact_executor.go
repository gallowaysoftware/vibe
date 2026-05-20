package vamp

import (
	"context"
	"fmt"
	"strings"
)

// compactExecutor implements StageExecutor for `type: compact` stages: the
// "summarize instead of truncate" path. When a downstream LLM call would
// overflow its context window, route the oversized input through one of
// these stages first. The executor chunks the input (paragraph-boundary
// aware), runs an LLM call per chunk that compresses to a proportional
// share of the global target, concatenates the per-chunk summaries, and
// — if the result still overshoots — recurses up to `compactMaxIters`
// times.
//
// Lossy by design (the model decides what's compressible) but the
// `preserve:` directive lets pipeline authors name the facts that MUST
// survive: numerical values, process steps, equipment names, etc.
type compactExecutor struct {
	inference InferenceFunc
}

// Compile-time guarantee that compactExecutor satisfies StageExecutor.
var _ StageExecutor = (*compactExecutor)(nil)

// compactMaxIters bounds the recursive-compact loop. Two iterations is
// enough for the common case (a single pass reducing 4x); a third covers
// pathological inputs where the first pass barely compresses (e.g.
// already-dense source) without spending unbounded LLM time on a stage.
const compactMaxIters = 3

// defaultCompactChunkMultiple sets the implicit chunk size as a multiple
// of target_chars. Each chunk plus its summary fits within typical
// 131K-context windows: at 4 chars/token, 4x = ~32K tokens of input and
// the summary is ~8K tokens of output.
const defaultCompactChunkMultiple = 4

// Execute runs the compact stage. Returns the compacted text in
// StageOutput.Text so the runner writes it to the stage's output path
// like any other text/render result.
func (c *compactExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, fmt.Errorf("compact: missing stage")
	}
	if st.Source == "" {
		return nil, fmt.Errorf("stage %s: source template is required for type: compact", st.ID)
	}
	if st.TargetChars <= 0 {
		return nil, fmt.Errorf("stage %s: target_chars must be > 0 for type: compact", st.ID)
	}

	extra := map[string]any{
		"pipeline_status": in.PipelineStatus,
		"failure_summary": in.FailureSummary,
	}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}
	source, err := renderTemplate(st.ID+":source", st.Source, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render source: %w", st.ID, err)
	}

	if len(source) <= st.TargetChars {
		// Already fits — pass through verbatim. The whole point of compact
		// is to avoid lossy work when it isn't needed.
		return &StageOutput{Text: source}, nil
	}

	chunkChars := st.ChunkChars
	if chunkChars <= 0 {
		chunkChars = st.TargetChars * defaultCompactChunkMultiple
	}

	current := source
	for iter := 0; iter < compactMaxIters && len(current) > st.TargetChars; iter++ {
		next, err := c.compactPass(ctx, in, current, chunkChars, st.TargetChars, st.Preserve, iter+1)
		if err != nil {
			return nil, err
		}
		// Defensive: if a pass didn't shrink anything (model refused to
		// compress, or input is already minimal), stop rather than spin.
		if len(next) >= len(current) {
			current = next
			break
		}
		current = next
	}
	return &StageOutput{Text: current}, nil
}

// compactPass runs one round of chunk-and-summarize. The per-chunk
// target is proportional: each chunk gets `(globalTarget / len(source))`
// of its current length, so the concatenated result lands near the
// global target without per-chunk over- or under-shooting.
func (c *compactExecutor) compactPass(ctx context.Context, in StageInput, source string, chunkChars, globalTarget int, preserve string, iter int) (string, error) {
	chunks := splitForCompaction(source, chunkChars)
	ratio := float64(globalTarget) / float64(len(source))
	if ratio > 1 {
		ratio = 1
	}
	parts := make([]string, 0, len(chunks))
	for ci, chunk := range chunks {
		chunkTarget := int(float64(len(chunk)) * ratio)
		if chunkTarget < 200 {
			chunkTarget = 200
		}
		prompt := buildCompactPrompt(chunk, chunkTarget, preserve)
		var onToken StreamFunc
		if in.Log != nil {
			onToken = func(delta string) {
				_, _ = in.Log.Write([]byte(delta))
			}
		}
		out, err := c.inference(ctx, in.BaseURL+"/v1", in.ModelID, prompt, nil, onToken)
		if err != nil {
			return "", fmt.Errorf("stage %s: compact iter %d chunk %d/%d: %w", in.Stage.ID, iter, ci+1, len(chunks), err)
		}
		if in.Log != nil {
			_, _ = in.Log.Write([]byte("\n"))
		}
		parts = append(parts, strings.TrimSpace(stripModelArtifacts(out)))
	}
	return strings.Join(parts, "\n\n"), nil
}

// splitForCompaction breaks source into chunks no larger than chunkChars,
// preferring to break on blank lines (paragraph/JSON-item boundaries) so
// individual concepts aren't bisected mid-sentence. When no blank line
// falls within a chunk's window, splits at the nearest newline; when
// no newline exists either, splits at the exact char count.
func splitForCompaction(source string, chunkChars int) []string {
	if len(source) <= chunkChars {
		return []string{source}
	}
	var out []string
	for start := 0; start < len(source); {
		end := start + chunkChars
		if end >= len(source) {
			out = append(out, source[start:])
			break
		}
		// Prefer a paragraph boundary near the chunk's end.
		boundary := strings.LastIndex(source[start:end], "\n\n")
		if boundary < 0 || boundary < chunkChars/2 {
			// Fall back to any newline within the chunk's second half.
			boundary = strings.LastIndex(source[start:end], "\n")
		}
		if boundary < 0 || boundary < chunkChars/2 {
			boundary = chunkChars
		} else {
			// Skip the boundary itself so the next chunk starts on a
			// fresh line.
			boundary++
		}
		out = append(out, source[start:start+boundary])
		start += boundary
	}
	return out
}

// buildCompactPrompt assembles the LLM-facing instruction. The preserve
// directive is interpolated into the body; an empty preserve becomes a
// generic "facts, numbers, names, process steps" default so the prompt
// is never blank on that line.
func buildCompactPrompt(chunk string, targetChars int, preserve string) string {
	if preserve == "" {
		preserve = "every numerical value, every name (people, places, products, equipment), every process step, every cause-and-effect relationship, and every detail an exam would test on"
	}
	var b strings.Builder
	b.WriteString("You are a context compactor. The text below is one chunk of a larger document that does not fit a downstream LLM's context window. Produce a compressed version of THIS CHUNK with the following rules:\n\n")
	b.WriteString(fmt.Sprintf("- Target length: approximately %d characters.\n", targetChars))
	b.WriteString(fmt.Sprintf("- Preserve: %s.\n", preserve))
	b.WriteString("- Maintain the original structure when applicable (if input is JSON, output JSON of the same shape with shorter field values; if input is markdown, output markdown of the same heading hierarchy).\n")
	b.WriteString("- Cut: redundant prose, repeated framing, motivation/recap paragraphs, anything that would survive a careful editor's pass.\n")
	b.WriteString("- Do NOT add new information. Do NOT include commentary, preamble, or explanation of what you did. Return ONLY the compressed text.\n\n")
	b.WriteString("# Chunk to compact\n\n")
	b.WriteString(chunk)
	return b.String()
}
