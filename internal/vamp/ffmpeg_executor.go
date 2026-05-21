package vamp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
	isM4B := st.M4BFrom != ""
	if !st.ConcatWavs && !isM4B && len(st.FFmpegArgs) == 0 {
		return nil, fmt.Errorf("stage %s: ffmpeg_args is required for type: ffmpeg (unless concat_wavs or m4b_from is set)", st.ID)
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
	// m4b mode: build a chapterized audiobook by concatenating per-item
	// MP3s with embedded chapter metadata.
	if isM4B {
		return f.executeM4B(ctx, in, st, extra)
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

// executeConcatWavs walks the run dir for all "*.wav" files, sorts them
// numerically (segment_0.wav, segment_1.wav, ...), creates a concat file
// list, and runs ffmpeg to merge them into the output MP3. The walk is
// recursive so audio stages that write to a per-stage subdirectory (e.g.
// `output: "audio/segment_{{.i}}.wav"`) are still picked up — a flat
// `filepath.Glob` of `*.wav` would miss those.
func (f *ffmpegExecutor) executeConcatWavs(ctx context.Context, in StageInput, st *Stage, extra map[string]any) (*StageOutput, error) {
	// concat_wavs_dir, when set, scopes the walk to a specific subdirectory
	// (template-rendered so a foreach stage can use a per-item subdir like
	// `audio/{{.unit.unit_id}}`). Default — empty — walks the whole run dir
	// the way the original concat_wavs always did.
	walkRoot := in.RunDir
	if st.ConcatWavsDir != "" {
		rel, err := renderTemplate(st.ID+":concat_wavs_dir", st.ConcatWavsDir, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render concat_wavs_dir: %w", st.ID, err)
		}
		if filepath.IsAbs(rel) {
			return nil, fmt.Errorf("stage %s: concat_wavs_dir %q must be relative to the run dir", st.ID, rel)
		}
		walkRoot = filepath.Join(in.RunDir, rel)
	}
	var wavs []string
	err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".wav") {
			wavs = append(wavs, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("stage %s: walk for *.wav: %w", st.ID, err)
	}
	if len(wavs) == 0 {
		return nil, fmt.Errorf("stage %s: no .wav files found under %s", st.ID, walkRoot)
	}
	// Sort by trailing integer in the filename — segment_0.wav before
	// segment_10.wav before segment_100.wav — so `audio/<unit>/segment_<i>.wav`
	// concatenates in the order the script generator emitted them. A plain
	// lexicographic sort puts segment_10 before segment_2; that bug
	// previously concatenated WAVs out of order.
	sort.Slice(wavs, func(i, j int) bool {
		ai, ok1 := trailingInt(wavs[i])
		bj, ok2 := trailingInt(wavs[j])
		if ok1 && ok2 && ai != bj {
			return ai < bj
		}
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

	// Create concat file list. Path is per-iteration so parallel foreach
	// iterations don't race on a shared `.ffmpeg-concat.txt`; the prior
	// shared-path implementation caused per-unit mp3 stages to read each
	// other's lists and produce bit-identical MP3s for the iterations
	// that started ffmpeg after a sibling's overwrite.
	listName := ".ffmpeg-concat.txt"
	if st.Foreach != nil {
		listName = fmt.Sprintf(".ffmpeg-concat.%s-%d.txt", st.ID, in.ItemIdx)
	}
	listPath := filepath.Join(in.RunDir, listName)
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

// executeM4B builds a chapterized M4B audiobook by:
//
//  1. Reading st.M4BFrom's JSON-array output to determine chapter order
//     and binding each item under st.M4BVar.
//  2. For each item: rendering st.M4BFile to an MP3 path (resolved
//     relative to the run dir when not absolute) and st.M4BChapter to
//     a chapter title string.
//  3. Probing each MP3's duration via ffprobe and building cumulative
//     start/end timestamps for the FFMETADATA chapter table.
//  4. Writing a concat list and a metadata file under the run dir.
//  5. Invoking ffmpeg once: `-f concat -safe 0 -i list -i metadata
//     -map_metadata 1 -c:a aac -b:a 96k -movflags +faststart output`.
//
// The result is an Apple-Books / Audiobookshelf / VLC-readable M4B
// with proper chapter navigation. Stage.Output must end in .m4b
// (validator enforces). No cover-art embed in this pass — that's a
// follow-up.
func (f *ffmpegExecutor) executeM4B(ctx context.Context, in StageInput, st *Stage, extra map[string]any) (*StageOutput, error) {
	srcResult, ok := in.Prior[st.M4BFrom]
	if !ok || srcResult == nil {
		return nil, fmt.Errorf("stage %s: m4b_from %q has no upstream output", st.ID, st.M4BFrom)
	}
	var items []any
	srcRaw := strings.TrimSpace(stripModelArtifacts(srcResult.Output))
	if srcRaw == "" {
		return nil, fmt.Errorf("stage %s: m4b_from %q produced empty output", st.ID, st.M4BFrom)
	}
	var asObj map[string]any
	if err := json.Unmarshal([]byte(srcRaw), &asObj); err == nil {
		if arr, isArr := asObj["items"].([]any); isArr {
			items = arr
		} else {
			items = []any{asObj}
		}
	} else if err := json.Unmarshal([]byte(srcRaw), &items); err != nil {
		return nil, fmt.Errorf("stage %s: parse m4b_from %q output: %w", st.ID, st.M4BFrom, err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("stage %s: m4b_from %q produced zero items", st.ID, st.M4BFrom)
	}

	type chapter struct {
		File    string
		Title   string
		Millis  int64
		StartMs int64
		EndMs   int64
	}
	chapters := make([]chapter, 0, len(items))
	var cumMs int64
	for i, raw := range items {
		itemExtra := map[string]any{st.M4BVar: raw, "i": i}
		for k, v := range extra {
			itemExtra[k] = v
		}
		filePath, err := renderTemplate(fmt.Sprintf("%s:m4b_file[%d]", st.ID, i), st.M4BFile, st.Inputs, in.Inputs, in.Prior, in.RunDir, itemExtra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render m4b_file[%d]: %w", st.ID, i, err)
		}
		title, err := renderTemplate(fmt.Sprintf("%s:m4b_chapter[%d]", st.ID, i), st.M4BChapter, st.Inputs, in.Inputs, in.Prior, in.RunDir, itemExtra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render m4b_chapter[%d]: %w", st.ID, i, err)
		}
		absFile := filePath
		if !filepath.IsAbs(absFile) {
			absFile = filepath.Join(in.RunDir, absFile)
		}
		dur, err := probeDurationMs(ctx, absFile)
		if err != nil {
			return nil, fmt.Errorf("stage %s: ffprobe chapter[%d] %s: %w", st.ID, i, absFile, err)
		}
		ch := chapter{
			File:    absFile,
			Title:   title,
			Millis:  dur,
			StartMs: cumMs,
			EndMs:   cumMs + dur,
		}
		chapters = append(chapters, ch)
		cumMs += dur
	}

	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output path: %w", st.ID, err)
	}
	outAbs := filepath.Join(in.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: create output dir: %w", st.ID, err)
	}

	listPath := filepath.Join(in.RunDir, fmt.Sprintf(".ffmpeg-m4b-concat.%s.txt", st.ID))
	var listBuf strings.Builder
	for _, ch := range chapters {
		fmt.Fprintf(&listBuf, "file '%s'\n", ch.File)
	}
	if err := os.WriteFile(listPath, []byte(listBuf.String()), 0o644); err != nil {
		return nil, fmt.Errorf("stage %s: write concat list: %w", st.ID, err)
	}

	metaPath := filepath.Join(in.RunDir, fmt.Sprintf(".ffmpeg-m4b-meta.%s.txt", st.ID))
	var metaBuf strings.Builder
	metaBuf.WriteString(";FFMETADATA1\n")
	for _, ch := range chapters {
		fmt.Fprintf(&metaBuf, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n", ch.StartMs, ch.EndMs, m4bEscapeMeta(ch.Title))
	}
	if err := os.WriteFile(metaPath, []byte(metaBuf.String()), 0o644); err != nil {
		return nil, fmt.Errorf("stage %s: write metadata: %w", st.ID, err)
	}

	binary := st.Binary
	if binary == "" {
		binary = f.defaultBinary
	}
	if binary == "" {
		binary = "ffmpeg"
	}

	// Optional cover-art embed: passes the cover PNG as a second
	// input stream and tags it as the attached-picture so Apple Books,
	// Audiobookshelf, VLC etc. display it as the book cover.
	var coverAbs string
	if st.CoverImage != "" {
		coverRel, err := renderTemplate(st.ID+":cover_image", st.CoverImage, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render cover_image: %w", st.ID, err)
		}
		coverAbs = coverRel
		if !filepath.IsAbs(coverAbs) {
			coverAbs = filepath.Join(in.RunDir, coverRel)
		}
		if _, err := os.Stat(coverAbs); err != nil {
			return nil, fmt.Errorf("stage %s: cover_image %s: %w", st.ID, coverAbs, err)
		}
	}

	args := []string{"-f", "concat", "-safe", "0", "-i", listPath, "-i", metaPath}
	if coverAbs != "" {
		args = append(args, "-i", coverAbs)
		// 0:a is the concatenated audio; 2:v is the cover image
		// (input 1 carries no streams, just FFMETADATA chapters).
		args = append(args,
			"-map", "0:a",
			"-map", "2:v",
			"-map_metadata", "1",
			"-disposition:v:0", "attached_pic",
			"-c:a", "aac", "-b:a", "96k",
			"-c:v", "copy",
		)
	} else {
		args = append(args,
			"-map", "0:a",
			"-map_metadata", "1",
			"-c:a", "aac", "-b:a", "96k",
		)
	}
	args = append(args, "-movflags", "+faststart", "-y", outAbs)
	runner := f.runner
	if runner == nil {
		runner = ffmpegCommandRunner{}
	}
	if in.Log != nil {
		fmt.Fprintf(in.Log, "ffmpeg m4b: %s (%d chapter(s), %s total)\n", outRel, len(chapters), formatM4BDuration(cumMs))
	}
	if err := runner.Run(ctx, binary, args, in.Log); err != nil {
		return nil, fmt.Errorf("stage %s: ffmpeg m4b: %w", st.ID, err)
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// probeDurationMs shells out to ffprobe to read a file's duration in
// milliseconds. ffprobe's -show_entries form returns a float seconds
// value; we round to ms to match the FFMETADATA TIMEBASE=1/1000.
func probeDurationMs(ctx context.Context, path string) (int64, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return 0, fmt.Errorf("%w: %s", err, msg)
		}
		return 0, err
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return 0, fmt.Errorf("ffprobe returned empty duration for %s", path)
	}
	secs, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	if secs < 0 {
		return 0, fmt.Errorf("negative duration %g from %s", secs, path)
	}
	return int64(secs*1000 + 0.5), nil
}

// m4bEscapeMeta escapes characters that have special meaning in FFMETADATA1
// (`=`, `;`, `#`, `\`, newlines) per ffmpeg's metadata-file format. Chapter
// titles routinely include `:` which is fine; the listed characters are
// the ones that break the parser.
func m4bEscapeMeta(s string) string {
	repl := strings.NewReplacer(
		`\`, `\\`,
		`=`, `\=`,
		`;`, `\;`,
		`#`, `\#`,
		"\n", `\n`,
	)
	return repl.Replace(s)
}

// formatM4BDuration renders a millisecond count as H:MM:SS for the M4B
// status line. Distinct from timing.go's formatDuration which formats
// time.Duration values for the per-stage timing report.
func formatM4BDuration(ms int64) string {
	totalSec := ms / 1000
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// trailingInt extracts the integer suffix from p's basename (after the last
// non-digit character, before the extension). Returns (n, true) on a clean
// match and (0, false) when the name has no numeric tail. Used by the
// concat_wavs walker to sort `segment_<i>.wav` files by numeric `i` rather
// than lexicographically (which would put segment_10 before segment_2).
func trailingInt(p string) (int, bool) {
	base := filepath.Base(p)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	end := len(base)
	start := end
	for start > 0 && base[start-1] >= '0' && base[start-1] <= '9' {
		start--
	}
	if start == end {
		return 0, false
	}
	n := 0
	for i := start; i < end; i++ {
		n = n*10 + int(base[i]-'0')
	}
	return n, true
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
