package vamp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
)

// recordingInference is a stub InferenceFunc / multimodal func pair that
// records what it was asked for and replies with a canned answer. Both
// paths are recorded on the SAME recorder so a test can assert which one
// the executor chose — the routing decision is the thing, and a stub per
// path would let a wrong choice pass.
type recordingInference struct {
	mu       sync.Mutex
	textCall int
	mmCall   int
	images   []string
	prompt   string
	reply    string
	err      error
}

func (r *recordingInference) text(_ context.Context, _, _, prompt string, _ map[string]any, onToken StreamFunc) (string, error) {
	r.mu.Lock()
	r.textCall++
	r.prompt = prompt
	reply, err := r.reply, r.err
	r.mu.Unlock()
	if onToken != nil {
		onToken(reply)
	}
	return reply, err
}

func (r *recordingInference) multimodal(_ context.Context, _, _, prompt string, images []string, _ map[string]any, onToken StreamFunc) (string, error) {
	r.mu.Lock()
	r.mmCall++
	r.prompt = prompt
	r.images = append([]string(nil), images...)
	reply, err := r.reply, r.err
	r.mu.Unlock()
	if onToken != nil {
		onToken(reply)
	}
	return reply, err
}

func newVisionExecutor(r *recordingInference) *visionExecutor {
	return &visionExecutor{inference: r.text, multimodal: r.multimodal}
}

// TestVisionExecutor_RoutesToMultimodalOnlyWhenThereAreImages pins the
// routing decision itself.
//
// It matters in both directions and the two failures look nothing alike.
// Sending a text-only stage down the multimodal path costs the vision
// encoder's per-image budget for no images; sending an image stage down
// the TEXT path silently drops the diagrams the lesson author attached
// and produces a confident answer about a picture the model never saw —
// which reads as a model quality problem, not a plumbing bug.
func TestVisionExecutor_RoutesToMultimodalOnlyWhenThereAreImages(t *testing.T) {
	pipelineDir := t.TempDir()
	imgDir := filepath.Join(pipelineDir, "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"b.png", "a.jpg", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(imgDir, n), []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("with images", func(t *testing.T) {
		rec := &recordingInference{reply: "described"}
		out, err := newVisionExecutor(rec).Execute(context.Background(), StageInput{
			Stage:       &Stage{ID: "look", Prompt: "describe", ImageDir: "images"},
			PipelineDir: pipelineDir,
			RunDir:      t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Text != "described" {
			t.Errorf("Text = %q", out.Text)
		}
		if rec.mmCall != 1 || rec.textCall != 0 {
			t.Fatalf("an image stage went down the text path (mm=%d text=%d): the model never saw the images", rec.mmCall, rec.textCall)
		}
		// Sorted, and non-image files excluded: the order is what makes
		// the cache key stable across runs, and notes.txt is not an image.
		want := []string{filepath.Join(imgDir, "a.jpg"), filepath.Join(imgDir, "b.png")}
		if len(rec.images) != len(want) {
			t.Fatalf("images = %v, want %v", rec.images, want)
		}
		for i := range want {
			if rec.images[i] != want[i] {
				t.Fatalf("images = %v, want %v (sorted, raster only)", rec.images, want)
			}
		}
	})

	t.Run("without images", func(t *testing.T) {
		rec := &recordingInference{reply: "plain"}
		if _, err := newVisionExecutor(rec).Execute(context.Background(), StageInput{
			Stage:       &Stage{ID: "think", Prompt: "reason"},
			PipelineDir: pipelineDir,
			RunDir:      t.TempDir(),
		}); err != nil {
			t.Fatal(err)
		}
		if rec.textCall != 1 || rec.mmCall != 0 {
			t.Fatalf("a text-only stage went down the multimodal path (mm=%d text=%d)", rec.mmCall, rec.textCall)
		}
	})
}

// TestCollectImages_MissingDirIsNotAnError pins a DELIBERATE degrade,
// which is the kind a later "add error handling" pass quietly reverses.
//
// Foreach pipelines point image_dir at a per-item path that legitimately
// does not exist on every item — an SAQ-only lesson in a curriculum
// carries no images/ subdir. Failing the stage there would refuse the
// text-only inference the vision-capable backend cleanly degrades to, and
// take the whole overnight run down with it.
func TestCollectImages_MissingDirIsNotAnError(t *testing.T) {
	got, err := collectImages(context.Background(), filepath.Join(t.TempDir(), "no-such-dir"))
	if err != nil {
		t.Fatalf("a missing image_dir must degrade to text-only, not fail: %v", err)
	}
	if got != nil {
		t.Errorf("images = %v, want none", got)
	}
}

// TestVisionExecutor_JSONOutputIsExtracted pins the cleanup step. A model
// asked for JSON routinely emits a sentence of preamble and a ``` fence
// around it; the raw string is what downstream stages read through
// {{ .stages.x.output }}, so without extraction the next stage's
// json.Unmarshal fails on work that was actually correct.
func TestVisionExecutor_JSONOutputIsExtracted(t *testing.T) {
	rec := &recordingInference{reply: "Sure! Here is the JSON:\n```json\n{\"a\":1}\n```\n"}
	out, err := newVisionExecutor(rec).Execute(context.Background(), StageInput{
		Stage:       &Stage{ID: "extract", Prompt: "emit json", OutputFormat: "json"},
		PipelineDir: t.TempDir(),
		RunDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Text, "Sure!") || strings.Contains(out.Text, "```") {
		t.Errorf("the preamble/fence reached the downstream stage: %q", out.Text)
	}
	if strings.TrimSpace(out.Text) != `{"a":1}` {
		t.Errorf("Text = %q", out.Text)
	}
}

// TestVisionExecutor_UnparseableJSONFailsTheStage is the other half: when
// there is no JSON in there at all, the stage must fail rather than write
// prose to a file the next stage will try to parse. A failure here names
// the stage that lied; a failure two stages later names the wrong one.
func TestVisionExecutor_UnparseableJSONFailsTheStage(t *testing.T) {
	rec := &recordingInference{reply: "I'm afraid I can't do that."}
	_, err := newVisionExecutor(rec).Execute(context.Background(), StageInput{
		Stage:       &Stage{ID: "extract", Prompt: "emit json", OutputFormat: "json"},
		PipelineDir: t.TempDir(),
		RunDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("prose returned for output_format: json must fail the stage")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error should say what was wrong: %v", err)
	}
}

// TestVisionExecutor_InferenceErrorIsWrappedNotSwallowed. A transport
// failure that returned ("", nil) would write an empty output file and
// let the pipeline continue on nothing.
func TestVisionExecutor_InferenceErrorIsWrappedNotSwallowed(t *testing.T) {
	rec := &recordingInference{err: errors.New("connection refused")}
	_, err := newVisionExecutor(rec).Execute(context.Background(), StageInput{
		Stage:       &Stage{ID: "think", Prompt: "reason"},
		PipelineDir: t.TempDir(),
		RunDir:      t.TempDir(),
	})
	if err == nil {
		t.Fatal("an inference failure must fail the stage")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the transport's own message was dropped: %v", err)
	}
}

// ── the rasterized-SVG cache ──────────────────────────────────────────

// rsvgScripts stand in for rsvg-convert, whose argv is
// `--width W --height H --keep-aspect-ratio -o <tmp> <svg>`.
const (
	rsvgWritesAnEmptyPNG = `
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
: > "$out"
exit 0`
	rsvgWritesRealPNG = `
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf 'PNG-ish bytes' > "$out"
exit 0`
)

// stubRSVG puts a fake rsvg-convert first on $PATH and redirects the
// rasterizer's content-addressed cache into a temp dir, returning that
// cache dir. Both are required: the executor calls a hard-coded binary
// name, and it writes into $XDG_CACHE_HOME/vamp, which is the developer's
// real cache if left alone.
func stubRSVG(t *testing.T, script string) string {
	t.Helper()
	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "rsvg-convert", script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	return filepath.Join(cache.DefaultRoot(), "svg-rasterized")
}

// writeSVG writes a small SVG and returns its path.
func writeSVG(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRasterizeSVG_ZeroBytePNGNeverEntersTheCache pins the guard PR #71
// added, and the second assertion is the one that matters.
//
// Failing the stage is not the whole job here: this cache is
// CONTENT-ADDRESSED and permanent. If a 0-byte render were renamed into
// place, every later call for the same SVG — every lesson in the course
// that reuses that boilerplate diagram, on every future run — would hit
// the Stat fast path and hand the model an empty image, with no error
// anywhere, until somebody deleted the cache dir by hand.
func TestRasterizeSVG_ZeroBytePNGNeverEntersTheCache(t *testing.T) {
	cacheDir := stubRSVG(t, rsvgWritesAnEmptyPNG)
	svg := writeSVG(t, t.TempDir(), "diagram.svg", `<svg xmlns="http://www.w3.org/2000/svg"/>`)

	_, err := rasterizeSVG(context.Background(), svg)
	if err == nil {
		t.Fatal("a 0-byte raster must fail rather than be handed to the model")
	}
	if !strings.Contains(err.Error(), "0-byte png") {
		t.Errorf("error should name the empty raster: %v", err)
	}

	entries, readErr := os.ReadDir(cacheDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") && !strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("an empty raster was admitted to the content-addressed cache as %q; every later run for this SVG would serve it", e.Name())
		}
	}
	// The scratch file goes too: a cache dir that fills with .tmp files on
	// every failed render is its own slow leak.
	if len(entries) != 0 {
		t.Errorf("failed render left %d file(s) behind in %s", len(entries), cacheDir)
	}
}

// TestRasterizeSVG_CachesByContent pins the fast path the guard above
// protects: a successful render lands at sha256(svg bytes).png and the
// second call for identical bytes does not re-run the converter. That is
// what makes the poisoned-entry failure permanent, so it is worth
// asserting the mechanism exists rather than inferring it.
func TestRasterizeSVG_CachesByContent(t *testing.T) {
	cacheDir := stubRSVG(t, rsvgWritesRealPNG)
	body := `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`
	dir := t.TempDir()
	first := writeSVG(t, dir, "one.svg", body)
	// A different FILE with identical bytes must hit the same entry.
	second := writeSVG(t, dir, "two.svg", body)

	p1, err := rasterizeSVG(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := rasterizeSVG(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Errorf("identical SVG bytes rendered to two cache entries: %q vs %q", p1, p2)
	}
	sum := sha256.Sum256([]byte(body))
	want := filepath.Join(cacheDir, hex.EncodeToString(sum[:])+".png")
	if p1 != want {
		t.Errorf("cache path = %q, want the content address %q", p1, want)
	}
}

// TestRasterizeSVG_ConverterFailureNamesItsOwnDiagnostic. A missing or
// unhappy rsvg-convert must fail the stage rather than silently drop the
// image the lesson author put there for the model to read, and the
// message the converter printed is the only thing that says why.
func TestRasterizeSVG_ConverterFailureNamesItsOwnDiagnostic(t *testing.T) {
	stubRSVG(t, `echo "Error reading SVG: XML parse error" >&2; exit 1`)
	svg := writeSVG(t, t.TempDir(), "broken.svg", "<svg")

	_, err := rasterizeSVG(context.Background(), svg)
	if err == nil {
		t.Fatal("a failing rsvg-convert must fail the stage")
	}
	if !strings.Contains(err.Error(), "XML parse error") {
		t.Errorf("the converter's own message was dropped: %v", err)
	}
}

// TestVisionExecutor_SVGImageFilesAreRasterized ties the rasterizer to
// the executor: image_files entries are template-rendered per iteration
// and a .svg there gets the same treatment collectImages gives a
// directory scan. Vision models see bitmap pixels, not vector markup, so
// an un-rasterized SVG is an image the model cannot read at all.
func TestVisionExecutor_SVGImageFilesAreRasterized(t *testing.T) {
	stubRSVG(t, rsvgWritesRealPNG)
	pipelineDir := t.TempDir()
	writeSVG(t, pipelineDir, "fig1.svg", `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)

	rec := &recordingInference{reply: "ok"}
	if _, err := newVisionExecutor(rec).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID:         "look",
			Prompt:     "describe",
			ImageFiles: []string{"fig1.svg"},
		},
		PipelineDir: pipelineDir,
		RunDir:      t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if rec.mmCall != 1 {
		t.Fatalf("image_files did not reach the multimodal path (mm=%d)", rec.mmCall)
	}
	if len(rec.images) != 1 || !strings.HasSuffix(rec.images[0], ".png") {
		t.Errorf("the SVG reached the model un-rasterized: %v", rec.images)
	}
}

// TestRenderExecutor_EmptyResultIsANewlineNotNothing pins a one-character
// guard whose absence is an infinite loop.
//
// vamp's resume layer treats a zero-byte stage output as "the stage
// crashed mid-write" and re-runs it. A template that legitimately elides
// everything — a conditional block over an empty list — therefore gets
// re-run on every resume, forever, because the correct answer and the
// crash signature are the same file. The extra newline is invisible to
// consumers, which all read-and-trim.
func TestRenderExecutor_EmptyResultIsANewlineNotNothing(t *testing.T) {
	out, err := (&renderExecutor{}).Execute(context.Background(), StageInput{
		Stage:       &Stage{ID: "list", Prompt: "{{ if false }}never{{ end }}"},
		PipelineDir: t.TempDir(),
		RunDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text == "" {
		t.Fatal("an empty render wrote a zero-byte file: every resume will re-run this stage forever")
	}
	if out.Text != "\n" {
		t.Errorf("Text = %q, want a bare newline", out.Text)
	}
}

// TestExpandTilde covers the anchored-prefix rule. "~/" expands; a bare
// "~" or an embedded one does not, because this is a path helper, not a
// shell.
func TestExpandTilde(t *testing.T) {
	// Pinned rather than read: os.UserHomeDir consults $HOME, and a test
	// that skipped when it could not find one would be a green assertion
	// about nothing on any runner that does not set it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := expandTilde("~/x/y"); got != filepath.Join(home, "x", "y") {
		t.Errorf("expandTilde(~/x/y) = %q", got)
	}
	for _, p := range []string{"~x", "/abs/~/x", "relative/path"} {
		if got := expandTilde(p); got != p {
			t.Errorf("expandTilde(%q) = %q, want it untouched", p, got)
		}
	}
}
