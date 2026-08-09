package vamp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recordingCompactor is a stub InferenceFunc for the compact stage. Each
// call returns reply(callIndex, prompt), which lets a test describe a
// model that compresses, one that refuses to, or one that fails on the
// third chunk.
type recordingCompactor struct {
	mu      sync.Mutex
	prompts []string
	reply   func(call int, prompt string) (string, error)
}

func (r *recordingCompactor) fn(_ context.Context, _, _, prompt string, _ map[string]any, onToken StreamFunc) (string, error) {
	r.mu.Lock()
	call := len(r.prompts)
	r.prompts = append(r.prompts, prompt)
	fn := r.reply
	r.mu.Unlock()
	out, err := fn(call, prompt)
	if onToken != nil && out != "" {
		onToken(out)
	}
	return out, err
}

func (r *recordingCompactor) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prompts)
}

// shrinkTo returns a reply func that always emits n characters, i.e. a
// model that compresses as asked.
func shrinkTo(n int) func(int, string) (string, error) {
	return func(int, string) (string, error) { return strings.Repeat("z", n), nil }
}

// TestCompactExecutor_ShortSourcePassesThroughUntouched pins the
// stage's reason for existing in the negative. compact is LOSSY — the
// model decides what to drop — so running it on input that already fits
// spends an LLM call to make the pipeline's own data worse. The
// assertion that matters is the call count, not the text.
func TestCompactExecutor_ShortSourcePassesThroughUntouched(t *testing.T) {
	rec := &recordingCompactor{reply: shrinkTo(10)}
	out, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:          "squeeze",
			Type:        StageTypeCompact,
			Source:      "already short",
			TargetChars: 1000,
		},
		RunDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "already short" {
		t.Errorf("Text = %q, want the source verbatim", out.Text)
	}
	if rec.calls() != 0 {
		t.Errorf("an input that already fits was sent to the model %d time(s); compact is lossy", rec.calls())
	}
}

// TestCompactExecutor_EmptySourceIsANewlineNotNothing pins a
// one-character guard whose absence is an infinite loop, and which no
// assertion about the compaction itself would reach.
//
// vamp's resume layer treats a zero-byte stage output as "the stage
// crashed mid-write" and re-runs it. A legitimately-empty compact result
// — "this lesson has no diagrams" content — would therefore be re-run on
// every resume, forever, because the correct answer and the crash
// signature are the same file.
func TestCompactExecutor_EmptySourceIsANewlineNotNothing(t *testing.T) {
	rec := &recordingCompactor{reply: shrinkTo(10)}
	out, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:          "squeeze",
			Type:        StageTypeCompact,
			Source:      "{{ if false }}text{{ end }}",
			TargetChars: 1000,
		},
		RunDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text == "" {
		t.Fatal("an empty compact wrote a zero-byte file: every resume will re-run this stage forever")
	}
	if out.Text != "\n" {
		t.Errorf("Text = %q, want a bare newline", out.Text)
	}
}

// TestCompactExecutor_RejectsAnUnsetTarget. target_chars is the only
// number that says what "compacted" means; zero would make the per-chunk
// ratio zero and ask the model for a 200-character summary of everything.
func TestCompactExecutor_RejectsAnUnsetTarget(t *testing.T) {
	for _, target := range []int{0, -1} {
		rec := &recordingCompactor{reply: shrinkTo(10)}
		_, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
			Stage:  &Stage{ID: "squeeze", Type: StageTypeCompact, Source: "text", TargetChars: target},
			RunDir: t.TempDir(),
		})
		if err == nil {
			t.Fatalf("target_chars %d must fail the stage", target)
		}
		if !strings.Contains(err.Error(), "target_chars") {
			t.Errorf("error should name the field: %v", err)
		}
	}
}

// TestCompactExecutor_RejectsAnAbsentSource. Without source there is
// nothing to compact, and an empty template would silently produce the
// newline passthrough above — a stage that looks like it ran.
func TestCompactExecutor_RejectsAnAbsentSource(t *testing.T) {
	rec := &recordingCompactor{reply: shrinkTo(10)}
	_, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
		Stage:  &Stage{ID: "squeeze", Type: StageTypeCompact, TargetChars: 100},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a compact stage with no source must fail")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestCompactExecutor_AModelThatRefusesToCompressStopsInsteadOfSpinning
// pins the anti-spin guard.
//
// The recursion condition is `len(current) > TargetChars`, so a model
// that returns its input unchanged — already-dense source, a refusal, a
// server echoing the prompt — satisfies it forever. Two bounds stand
// between that and an unbounded LLM bill: compactMaxIters, and the
// "did this pass actually shrink anything" check that stops early. The
// call count is the assertion; the text is not.
func TestCompactExecutor_AModelThatRefusesToCompressStopsInsteadOfSpinning(t *testing.T) {
	source := strings.Repeat("a", 5000)
	rec := &recordingCompactor{reply: func(_ int, prompt string) (string, error) {
		// Echo something at least as long as the input chunk: no progress.
		_ = prompt
		return source, nil
	}}
	out, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:          "squeeze",
			Type:        StageTypeCompact,
			Source:      source,
			TargetChars: 100,
			ChunkChars:  5000,
		},
		RunDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Text) == 0 {
		t.Fatal("a stalled compact must still return the best text it has")
	}
	// One chunk per pass, and the no-progress check ends it after the
	// first. compactMaxIters (3) is the outer bound either way.
	if got := rec.calls(); got != 1 {
		t.Errorf("a model that made no progress was called %d times; want 1 (the no-progress early-out)", got)
	}
}

// TestCompactExecutor_HonoursTheIterationCeiling is the same class from
// the other side: a model that shrinks a LITTLE every time never reaches
// the target, and only compactMaxIters ends it.
func TestCompactExecutor_HonoursTheIterationCeiling(t *testing.T) {
	rec := &recordingCompactor{}
	rec.reply = func(call int, prompt string) (string, error) {
		// Always one character shorter than the chunk it was given, so
		// every pass makes progress and none of them is ever enough.
		body := prompt[strings.Index(prompt, "# Chunk to compact")+len("# Chunk to compact"):]
		return strings.Repeat("y", len(strings.TrimSpace(body))-1), nil
	}
	out, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:          "squeeze",
			Type:        StageTypeCompact,
			Source:      strings.Repeat("a", 4000),
			TargetChars: 10,
			ChunkChars:  4000,
		},
		RunDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Text) <= 10 {
		t.Fatalf("this model cannot reach the target; the test proves nothing if it did (len=%d)", len(out.Text))
	}
	// The LITERAL 3, not compactMaxIters. Comparing the observed count to
	// the constant the production loop reads is a tautology: raising the
	// ceiling to five would move both sides together and the test would
	// stay green while the bound it names had changed. Mutation-verified
	// by doing exactly that.
	if got := rec.calls(); got != 3 {
		t.Errorf("a never-converging model ran %d passes; the ceiling is 3 (compactMaxIters = %d)", got, compactMaxIters)
	}
}

// TestCompactExecutor_ChunkFailureNamesTheIterationAndChunk pins the
// message. A 40-chunk compact that dies on chunk 31 of pass 2 is
// undebuggable if the error says only "inference failed": the operator
// cannot tell whether it was the first pass on a huge input or the
// recursion on an already-compacted one.
func TestCompactExecutor_ChunkFailureNamesTheIterationAndChunk(t *testing.T) {
	rec := &recordingCompactor{}
	rec.reply = func(call int, _ string) (string, error) {
		if call == 1 {
			return "", errors.New("connection refused")
		}
		return strings.Repeat("z", 50), nil
	}
	_, err := (&compactExecutor{inference: rec.fn}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:          "squeeze",
			Type:        StageTypeCompact,
			Source:      strings.Repeat("a", 3000),
			TargetChars: 100,
			ChunkChars:  1000,
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("an inference failure inside a chunk must fail the stage")
	}
	for _, want := range []string{"squeeze", "iter 1", "chunk 2/", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q: %v", want, err)
		}
	}
}

// TestCompactExecutor_CancelledContextEndsTheStage. A compact over a
// hundred chunks is a long sequence of LLM calls; ctrl-C has to stop it
// at the next boundary rather than run to completion.
func TestCompactExecutor_CancelledContextEndsTheStage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recordingCompactor{}
	rec.reply = func(call int, _ string) (string, error) {
		// Order matters: the first chunk SUCCEEDS and cancels, so the
		// assertion below is about the chunks after it rather than about
		// the one that pulled the trigger.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if call == 0 {
			cancel()
		}
		return strings.Repeat("z", 10), nil
	}
	_, err := (&compactExecutor{inference: rec.fn}).Execute(ctx, StageInput{
		Stage: &Stage{
			ID:          "squeeze",
			Type:        StageTypeCompact,
			Source:      strings.Repeat("a", 5000),
			TargetChars: 100,
			ChunkChars:  1000,
		},
		RunDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("a cancelled compact must fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation must stay reachable through the chain (runWithRetryInner's early-out walks it): %v", err)
	}
	// The source splits into five chunks; chunks three onwards never ran.
	if got := rec.calls(); got != 2 {
		t.Errorf("calls = %d, want 2 of 5; a cancelled compact kept issuing them", got)
	}
}

// TestSplitForCompaction_PrefersParagraphBoundaries pins the chunker's
// three-tier fallback and, more importantly, that it is LOSSLESS.
//
// Losslessness is the non-obvious half: the chunks are summarised
// independently and concatenated, so a chunker that dropped or duplicated
// a byte would surface as content the compacted text invented or lost —
// blamed on the model, which is the failure mode this whole pipeline is
// worst at diagnosing.
func TestSplitForCompaction_PrefersParagraphBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		source string
		size   int
	}{
		{"paragraph boundaries", strings.Repeat("para body here.\n\n", 200), 300},
		{"newlines only", strings.Repeat("a line of text here\n", 200), 300},
		{"no boundary at all", strings.Repeat("x", 4000), 300},
		{"shorter than one chunk", "tiny", 300},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chunks := splitForCompaction(c.source, c.size)
			if len(chunks) == 0 {
				t.Fatal("no chunks")
			}
			if got := strings.Join(chunks, ""); got != c.source {
				t.Fatalf("chunking is lossy: %d bytes in, %d out", len(c.source), len(got))
			}
			for i, ch := range chunks {
				if len(ch) > c.size {
					t.Errorf("chunk %d is %d bytes, over the %d cap", i, len(ch), c.size)
				}
			}
		})
	}

	// And the boundary preference itself: with blank lines available,
	// chunks end on one rather than mid-sentence.
	chunks := splitForCompaction(strings.Repeat("para body here.\n\n", 200), 300)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, ch := range chunks[:len(chunks)-1] {
		if !strings.HasSuffix(ch, "\n\n") && !strings.HasSuffix(ch, "\n") {
			t.Errorf("chunk %d was cut mid-paragraph: %q", i, ch[len(ch)-20:])
		}
	}
}

// TestBuildCompactPrompt_PreserveIsNeverBlank pins the default. preserve
// is the pipeline author's only lever over WHAT survives a lossy pass;
// an empty one would render as "- Preserve: ." and quietly hand the model
// a blank instruction where the most important rule should be.
func TestBuildCompactPrompt_PreserveIsNeverBlank(t *testing.T) {
	withDefault := buildCompactPrompt("body", 500, "")
	if strings.Contains(withDefault, "Preserve: .") {
		t.Error("an unset preserve rendered as an empty instruction")
	}
	if !strings.Contains(withDefault, "numerical value") {
		t.Errorf("the default preserve directive is missing:\n%s", withDefault)
	}
	explicit := buildCompactPrompt("body", 500, "every still temperature")
	if !strings.Contains(explicit, "Preserve: every still temperature.") {
		t.Errorf("an explicit preserve did not reach the prompt:\n%s", explicit)
	}
	if !strings.Contains(explicit, fmt.Sprintf("approximately %d characters", 500)) {
		t.Errorf("the per-chunk target did not reach the prompt:\n%s", explicit)
	}
	if !strings.HasSuffix(strings.TrimSpace(explicit), "body") {
		t.Error("the chunk must be last so the instructions frame it")
	}
}
