package vamp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ffmpegExecutor implements StageExecutor for type: ffmpeg stages. It performs
// final assembly of media (combining audio + images / video clips into a
// container) by shelling out to ffmpeg
// (https://ffmpeg.org/). Each entry in Stage.FFmpegArgs is rendered as a Go
// template against the standard binding (inputs / stages / runDir + foreach
// item) and passed through verbatim. The executor then appends `-y <output>`
// after the user args so the destination path is always under the run dir and
// users don't manage it themselves. The output path is reported in
// StageOutput.Files so the runner skips its text write-back path (the binary,
// not vamp, produced the file).
type ffmpegExecutor struct {
	// runner is the injectable subprocess driver. Tests stub this so they
	// can record argv + stream stderr without spawning a real ffmpeg. nil
	// means use ffmpegCommandRunner (the os/exec-backed default).
	runner ffmpegRunner
	// defaultBinary, when non-empty, overrides "ffmpeg" as the fallback
	// binary name. Used by tests; production callers leave it empty.
	defaultBinary string
}

// ffmpegRunner is the subprocess driver interface used by ffmpegExecutor. The
// real implementation is ffmpegCommandRunner; tests replace it with a recorder
// so they can assert argv without invoking ffmpeg. stderr (when non-nil) is
// where the runner streams ffmpeg's stderr; ffmpeg writes its progress lines
// there by default, so plumbing it to in.Log keeps the user informed while
// the assembly runs.
type ffmpegRunner interface {
	// Run executes binary with the given args, streaming the subprocess's
	// stderr to the supplied writer. On non-zero exit it should return an
	// error whose message surfaces the captured stderr's last lines.
	Run(ctx context.Context, binary string, args []string, stderr io.Writer) error
}

// Compile-time guarantee that ffmpegExecutor satisfies StageExecutor.
var _ StageExecutor = (*ffmpegExecutor)(nil)

// Execute performs one ffmpeg invocation. For foreach stages the runner calls
// this once per item with StageInput.Item populated; non-foreach invocations
// have Item=nil and ItemIdx=0.
//
// Side effects: writes the rendered output file to <RunDir>/<rendered output>
// and reports that path in StageOutput.Files.
func (f *ffmpegExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("ffmpeg: missing stage")
	}
	if !st.ConcatWavs && len(st.FFmpegArgs) == 0 {
		return nil, fmt.Errorf("stage %s: ffmpeg_args is required for type: ffmpeg (unless concat_wavs is true)", st.ID)
	}

	// Foreach binding goes into argv-template rendering and the output-path
	// render below so users can template both per item.
	var extra map[string]any
	if st.Foreach != nil {
		extra = map[string]any{st.Foreach.Var: in.Item, "i": in.ItemIdx}
	}

	// concat_wavs mode: auto-glob all WAVs in run dir and concatenate them.
	if st.ConcatWavs {
		return f.executeConcatWavs(ctx, in, st, extra)
	}

	// Render each argv entry independently. We name each template with its
	// index so a parse / render error points the user at the offending arg
	// rather than just "the ffmpeg stage."
	args := make([]string, 0, len(st.FFmpegArgs)+2)
	for i, raw := range st.FFmpegArgs {
		rendered, err := renderTemplate(fmt.Sprintf("%s:arg[%d]", st.ID, i), raw, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render ffmpeg_args[%d]: %w", st.ID, i, err)
		}
		args = append(args, rendered)
	}

	// Render the output path. ffmpeg stages own their own output write (the
	// ffmpeg subprocess does it), so we render here and pass the absolute
	// path on the argv. We ALSO report the absolute form in StageOutput.Files
	// so downstream `.stages.<id>.output` references resolve from the
	// daemon's CWD (subprocesses like ffmpeg/Piper can't open RunDir-relative
	// paths).
	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	outAbs := filepath.Join(in.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: create output dir: %w", st.ID, err)
	}

	// `-y` forces overwrite so reruns into the same run dir don't hang on
	// the interactive "y/n" prompt ffmpeg would otherwise emit on stderr.
	// We append it (along with the output path) AFTER the user args so
	// ffmpeg's positional argv parser treats the rendered path as the
	// output file, not as an input or an option value.
	args = append(args, "-y", outAbs)

	binary := st.Binary
	if binary == "" {
		binary = f.defaultBinary
	}
	if binary == "" {
		binary = "ffmpeg"
	}

	runner := f.runner
	if runner == nil {
		runner = ffmpegCommandRunner{}
	}
	// Stream a status line for the live single-stage path so users see
	// something happen between "queued" and "mp4 written". ffmpeg's own
	// progress lines stream below via in.Log as stderr.
	if in.Log != nil {
		fmt.Fprintf(in.Log, "ffmpeg: %s (%d arg(s))\n", outRel, len(st.FFmpegArgs))
	}
	if err := runner.Run(ctx, binary, args, in.Log); err != nil {
		return nil, fmt.Errorf("stage %s: ffmpeg: %w", st.ID, err)
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// executeConcatWavs globs all "*.wav" files in the run dir, sorts them
// numerically (segment_0.wav, segment_1.wav, ...), creates a concat file
// list, and runs ffmpeg to merge them into the output MP3.
func (f *ffmpegExecutor) executeConcatWavs(ctx context.Context, in StageInput, st *Stage, extra map[string]any) (*StageOutput, error) {
	wavs, err := filepath.Glob(filepath.Join(in.RunDir, "*.wav"))
	if err != nil {
		return nil, fmt.Errorf("stage %s: glob *.wav: %w", st.ID, err)
	}
	if len(wavs) == 0 {
		return nil, fmt.Errorf("stage %s: no .wav files found in run dir", st.ID)
	}
	// Sort numerically: segment_0.wav before segment_10.wav.
	sort.Slice(wavs, func(i, j int) bool {
		return wavs[i] < wavs[j]
	})

	// Render the output path.
	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	outAbs := filepath.Join(in.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: create output dir: %w", st.ID, err)
	}

	// Create concat file list.
	listPath := filepath.Join(in.RunDir, ".ffmpeg-concat.txt")
	listContent := strings.Builder{}
	for _, w := range wavs {
		fmt.Fprintf(&listContent, "file '%s'\n", w)
	}
	if err := os.WriteFile(listPath, []byte(listContent.String()), 0o644); err != nil {
		return nil, fmt.Errorf("stage %s: write concat list: %w", st.ID, err)
	}

	binary := st.Binary
	if binary == "" {
		binary = f.defaultBinary
	}
	if binary == "" {
		binary = "ffmpeg"
	}

	args := []string{
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-c:a", "libmp3lame",
		"-b:a", "128k",
		"-y", outAbs,
	}

	runner := f.runner
	if runner == nil {
		runner = ffmpegCommandRunner{}
	}
	if in.Log != nil {
		fmt.Fprintf(in.Log, "ffmpeg concat: %s (%d wav(s))\n", outRel, len(wavs))
	}
	if err := runner.Run(ctx, binary, args, in.Log); err != nil {
		return nil, fmt.Errorf("stage %s: ffmpeg concat: %w", st.ID, err)
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// ffmpegCommandRunner is the production ffmpegRunner. It spawns ffmpeg via
// os/exec, fans the subprocess's stderr to BOTH the caller-supplied writer
// (for live progress display) and a tail buffer (so a non-zero exit can
// surface the last few lines of stderr in the returned error). stdout is
// discarded — ffmpeg writes its output to the destination path we pass on the
// argv, not to stdout; anything it does emit there is noise.
type ffmpegCommandRunner struct{}

// ffmpegStderrTailLines bounds how many stderr lines we retain for the
// error-context buffer. ffmpeg emits a lot of "frame=  123 fps=..." progress
// lines; capping the tail keeps a failure's error message focused on the
// terminating-error region without becoming a wall of progress noise.
const ffmpegStderrTailLines = 20

func (ffmpegCommandRunner) Run(ctx context.Context, binary string, args []string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	// Tail buffer collects the last N stderr lines so non-zero exits surface
	// a useful slice of ffmpeg's own error message. We also tee live to the
	// caller-supplied writer when one is set; the two consumers are
	// independent — the live writer can be nil (e.g. multi-stage groups
	// already buffer per stage upstream) without losing the tail buffer.
	tail := newLineRingBuffer(ffmpegStderrTailLines)
	var sinks []io.Writer
	sinks = append(sinks, tail)
	if stderr != nil {
		sinks = append(sinks, stderr)
	}
	cmd.Stderr = io.MultiWriter(sinks...)
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(tail.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// lineRingBuffer is a writer that retains the last N newline-delimited lines
// of input. Used to capture a tailing slice of a subprocess's stderr for
// error-context reporting while still letting the live stream flow to the
// caller-supplied writer. Safe for concurrent writes (cmd.Stderr is
// single-goroutine in practice, but io.MultiWriter doesn't promise that and a
// future hook could add a parallel consumer).
type lineRingBuffer struct {
	mu       sync.Mutex
	max      int
	lines    []string
	leftover []byte // bytes of a partial line carried over between Writes
}

func newLineRingBuffer(max int) *lineRingBuffer {
	if max <= 0 {
		max = 1
	}
	return &lineRingBuffer{max: max}
}

func (b *lineRingBuffer) Write(p []byte) (int, error) {
	// CRITICAL: this writer wraps ffmpeg's stderr inside an io.MultiWriter.
	// io.MultiWriter's contract forwards a short count from any inner writer
	// as a short write to the caller; cmd.Run then surfaces it as
	// `short write`. ffmpeg's banner+libx264 stats easily overflow whatever
	// "natural" capacity a line-ring exposes, so we ALWAYS report the full
	// input as consumed — ring storage drops oldest lines silently to keep
	// the bound, but the Write call itself never short-counts. The only
	// way out is an explicit sink error (we don't have one today; if a
	// future variant introduces one, return io.ErrClosedPipe — never a
	// short count).
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	// Stitch any leftover from the previous write onto this one so a line
	// that spans Write boundaries lands as a single ring entry. The stitched
	// buffer is local — `p` (the caller's slice) is not mutated and `n` is
	// captured above so the short-write invariant holds regardless.
	buf := p
	if len(b.leftover) > 0 {
		combined := make([]byte, 0, len(b.leftover)+len(p))
		combined = append(combined, b.leftover...)
		combined = append(combined, p...)
		buf = combined
		b.leftover = nil
	}
	// ffmpeg progress uses CR ('\r') to overwrite a single status line, so
	// split on '\n' OR '\r' — otherwise we'd accumulate one "line" with all
	// the progress in it and lose the tail bound.
	scanner := bufio.NewScanner(bytes.NewReader(buf))
	scanner.Split(scanCRLF)
	for scanner.Scan() {
		b.appendLine(scanner.Text())
	}
	// Anything after the last delimiter is incomplete and held for the next
	// Write so we don't double-count it once the rest arrives.
	if i := lastDelim(buf); i >= 0 {
		b.leftover = append([]byte(nil), buf[i+1:]...)
	} else {
		b.leftover = append([]byte(nil), buf...)
	}
	return n, nil
}

func (b *lineRingBuffer) appendLine(s string) {
	b.lines = append(b.lines, s)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// String returns the retained lines joined by newlines. Includes any
// in-progress partial line (without trailing delimiter) so a stderr stream
// that exited mid-line still shows its last bytes in error context.
func (b *lineRingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	parts := append([]string(nil), b.lines...)
	if len(b.leftover) > 0 {
		parts = append(parts, string(b.leftover))
	}
	return strings.Join(parts, "\n")
}

// scanCRLF is a bufio.SplitFunc that breaks on either '\n' or '\r'. We use it
// in lineRingBuffer because ffmpeg overwrites its progress line with '\r' and
// only emits '\n' on status changes; treating both as line terminators bounds
// the buffer correctly during long renders.
func scanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, c := range data {
		if c == '\n' || c == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// lastDelim returns the index of the last '\n' or '\r' in p, or -1 if neither
// appears. Used by lineRingBuffer.Write to decide where the trailing partial
// line begins.
func lastDelim(p []byte) int {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '\n' || p[i] == '\r' {
			return i
		}
	}
	return -1
}
