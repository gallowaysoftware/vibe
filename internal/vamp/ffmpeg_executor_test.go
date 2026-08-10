package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingFFmpegRunner is a test double for ffmpegRunner. It records every
// Run call (binary, args) and lets the test choose what to return. The
// optional onRun hook can write to the stderr writer so tests can verify the
// stream-to-Log plumbing without spawning a real ffmpeg.
type recordingFFmpegRunner struct {
	mu      sync.Mutex
	calls   []recordedFFmpegCall
	wantErr error
	// stderrBlob, when non-empty, is written to the supplied stderr writer
	// before Run returns. Used to verify the executor pipes ffmpeg's stderr
	// through to in.Log.
	stderrBlob string
	// onRun, when set, is invoked synchronously inside Run after recording
	// the call (and before any wantErr is returned). Tests use it to
	// simulate ffmpeg's side effects (e.g. creating the output file the
	// real binary would).
	onRun func(call recordedFFmpegCall) error
	// emptyOutput reproduces the failure this double previously could not
	// express: ffmpeg exiting 0 having written a 0-byte container. The
	// default (false) writes a few plausible bytes instead, because
	// "exited 0 and produced nothing" is now a stage failure and every
	// pre-existing test here means to describe a SUCCESSFUL run.
	emptyOutput bool
}

// writeFakeOutput creates the file the real ffmpeg would have written.
// The output path is the last argv element by construction — every call
// site appends "-y", outAbs last — which is also what the executor
// stats afterwards.
func (r *recordingFFmpegRunner) writeFakeOutput(args []string) error {
	if len(args) == 0 {
		return nil
	}
	out := args[len(args)-1]
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	var body []byte
	if !r.emptyOutput {
		body = []byte("fake-media-bytes")
	}
	return os.WriteFile(out, body, 0o644)
}

type recordedFFmpegCall struct {
	Binary string
	Args   []string
}

func (r *recordingFFmpegRunner) Run(ctx context.Context, binary string, args []string, stderr io.Writer) error {
	call := recordedFFmpegCall{Binary: binary, Args: append([]string(nil), args...)}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	hook := r.onRun
	wantErr := r.wantErr
	blob := r.stderrBlob
	r.mu.Unlock()
	if blob != "" && stderr != nil {
		_, _ = io.WriteString(stderr, blob)
	}
	if hook != nil {
		if err := hook(call); err != nil {
			return err
		}
	}
	if wantErr != nil {
		return wantErr
	}
	// A zero exit means ffmpeg claims it produced the output. Produce it,
	// so the double describes the real success path rather than a case
	// the executor is now required to reject.
	return r.writeFakeOutput(call.Args)
}

func (r *recordingFFmpegRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingFFmpegRunner) call(i int) recordedFFmpegCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

// TestFFmpegExecutor_RendersArgsAndCallsFFmpeg verifies the executor renders
// each ffmpeg_args entry through the template engine (resolving
// `{{ index .stages.x.outputs 0 }}` against prior-stage outputs), then invokes
// the binary with the rendered args plus the expected `-y <output>` suffix.
func TestFFmpegExecutor_RendersArgsAndCallsFFmpeg(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:     "assemble",
		Type:   StageTypeFFmpeg,
		Inputs: []string{"voiceover", "render"},
		FFmpegArgs: []string{
			"-loop", "1",
			"-i", "{{ index .stages.render.outputs 0 }}",
			"-i", "{{ .stages.voiceover.output }}",
			"-c:v", "libx264",
			"-shortest",
		},
		Output: "final.mp4",
	}
	prior := map[string]*stageResult{
		"voiceover": {Output: "voiceover.wav"},
		"render":    {Output: "assets/img_0.png", Outputs: []string{"assets/img_0.png"}},
	}
	in := StageInput{
		Stage:  stage,
		Prior:  prior,
		RunDir: runDir,
	}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("expected 1 ffmpeg invocation, got %d", runner.callCount())
	}
	call := runner.call(0)
	if call.Binary != "ffmpeg" {
		t.Errorf("binary = %q, want %q", call.Binary, "ffmpeg")
	}
	// User args must come first, then -y, then the path ffmpeg writes to —
	// the SCRATCH file beside the rendered output, which the executor
	// renames into place once ffmpeg has exited and produced something.
	// Argument ordering matters to ffmpeg's positional parser.
	wantArgs := []string{
		"-loop", "1",
		"-i", "assets/img_0.png",
		"-i", "voiceover.wav",
		"-c:v", "libx264",
		"-shortest",
		"-y", partialOutputPath(filepath.Join(runDir, "final.mp4")),
	}
	if !equalStringSlices(call.Args, wantArgs) {
		t.Errorf("args mismatch:\n got:  %v\n want: %v", call.Args, wantArgs)
	}
	// Output reporting: a single ABSOLUTE path so downstream
	// .stages.<id>.output references resolve from the daemon's CWD when
	// passed as argv to subprocesses (ffmpeg/Piper). The relative form
	// would leave subprocesses opening files relative to wherever the
	// daemon was started.
	wantFile := filepath.Join(runDir, "final.mp4")
	if len(out.Files) != 1 || out.Files[0] != wantFile {
		t.Errorf("out.Files = %v, want [%s]", out.Files, wantFile)
	}
}

// TestFFmpegExecutor_FailureSurfacesStderr verifies a non-zero ffmpeg exit
// produces an error whose message includes the runner's stderr context, so a
// failed assembly is diagnosable without rerunning under strace.
func TestFFmpegExecutor_FailureSurfacesStderr(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{
		wantErr: errors.New("exit status 1: ffmpeg: invalid codec"),
	}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:         "assemble",
		Type:       StageTypeFFmpeg,
		FFmpegArgs: []string{"-i", "missing.png"},
		Output:     "final.mp4",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected error from ffmpeg failure")
	}
	if !strings.Contains(err.Error(), "invalid codec") {
		t.Errorf("error does not surface ffmpeg stderr: %v", err)
	}
	// The error should also identify the stage so a multi-stage failure can
	// be diagnosed without inspecting goroutine state.
	if !strings.Contains(err.Error(), "assemble") {
		t.Errorf("error does not mention stage id: %v", err)
	}
}

// TestFFmpegExecutor_CustomBinary verifies a stage-level binary override is
// honored on the argv (ffmpeg lives in a non-default location on some
// systems, and on macOS in particular it's commonly under /usr/local/bin or
// /opt/homebrew/bin).
func TestFFmpegExecutor_CustomBinary(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:         "assemble",
		Type:       StageTypeFFmpeg,
		Binary:     "/usr/local/bin/ffmpeg",
		FFmpegArgs: []string{"-i", "x.wav"},
		Output:     "final.mp4",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := runner.call(0).Binary; got != "/usr/local/bin/ffmpeg" {
		t.Errorf("argv[0] = %q, want /usr/local/bin/ffmpeg", got)
	}
}

// TestFFmpegExecutor_TemplateUsesPriorStageFiles verifies the executor
// resolves `{{ index .stages.render.outputs N }}` against a multi-item slice
// produced by a foreach stage, and that the chosen path lands on the argv at
// the exact position the template was placed in.
func TestFFmpegExecutor_TemplateUsesPriorStageFiles(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:     "assemble",
		Type:   StageTypeFFmpeg,
		Inputs: []string{"render"},
		FFmpegArgs: []string{
			"-i", "{{ index .stages.render.outputs 0 }}",
			"-i", "{{ index .stages.render.outputs 2 }}",
			"-filter_complex", "concat=n=2:v=1:a=0",
		},
		Output: "concat.mp4",
	}
	prior := map[string]*stageResult{
		"render": {
			Output: "assets/img_0.png\nassets/img_1.png\nassets/img_2.png",
			Outputs: []string{
				"assets/img_0.png",
				"assets/img_1.png",
				"assets/img_2.png",
			},
		},
	}
	in := StageInput{Stage: stage, Prior: prior, RunDir: runDir}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	call := runner.call(0)
	wantArgs := []string{
		"-i", "assets/img_0.png",
		"-i", "assets/img_2.png",
		"-filter_complex", "concat=n=2:v=1:a=0",
		"-y", partialOutputPath(filepath.Join(runDir, "concat.mp4")),
	}
	if !equalStringSlices(call.Args, wantArgs) {
		t.Errorf("args mismatch:\n got:  %v\n want: %v", call.Args, wantArgs)
	}
}

// TestFFmpegExecutor_ConcatVideo verifies concat_video mode reads the
// upstream JSON array, renders each clip path, and builds the normalizing
// concat filtergraph in order.
func TestFFmpegExecutor_ConcatVideo(t *testing.T) {
	runDir := t.TempDir()
	clips := []string{
		filepath.Join(runDir, "shot_0.mp4"),
		filepath.Join(runDir, "shot_1.mp4"),
	}
	for _, c := range clips {
		if err := os.WriteFile(c, []byte("clipdata"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingFFmpegRunner{}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:              "stitch",
		Type:            StageTypeFFmpeg,
		Inputs:          []string{"clips_list"},
		ConcatVideoFrom: "clips_list",
		ConcatVideoVar:  "item",
		ConcatVideoFile: "{{ .item }}",
		Output:          "final.mp4",
	}
	listJSON, _ := json.Marshal(clips)
	prior := map[string]*stageResult{"clips_list": {Output: string(listJSON)}}
	in := StageInput{Stage: stage, Prior: prior, RunDir: runDir}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := filepath.Join(runDir, "final.mp4"); len(out.Files) != 1 || out.Files[0] != want {
		t.Fatalf("output = %v, want [%s]", out.Files, want)
	}
	call := runner.call(0)
	joined := strings.Join(call.Args, " ")
	// Both clips fed as inputs in order.
	if !strings.Contains(joined, "-i "+clips[0]+" -i "+clips[1]) {
		t.Errorf("inputs not in order: %s", joined)
	}
	// Default vertical normalization + 2-way concat.
	for _, want := range []string{
		"scale=1080:1920:force_original_aspect_ratio=decrease",
		"fps=30[v0]", "fps=30[v1]",
		"concat=n=2:v=1:a=0[outv]",
		"-map [outv]", "-c:v libx264", "-pix_fmt yuv420p",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("filtergraph missing %q in: %s", want, joined)
		}
	}
}

// TestFFmpegExecutor_StderrStreamsToLog verifies the executor wires the
// runner's stderr writer to in.Log so users see ffmpeg's progress lines live.
// Without this plumbing, a long render appears hung between "queued" and
// "mp4 written."
func TestFFmpegExecutor_StderrStreamsToLog(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{
		stderrBlob: "frame=  100 fps= 50 q=28.0 size=    1024kB\n",
	}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:         "assemble",
		Type:       StageTypeFFmpeg,
		FFmpegArgs: []string{"-i", "x.wav"},
		Output:     "final.mp4",
	}
	var log bytes.Buffer
	in := StageInput{Stage: stage, RunDir: runDir, Log: &log}
	if _, err := exec.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := log.String()
	if !strings.Contains(got, "frame=  100 fps= 50") {
		t.Errorf("expected ffmpeg progress line in log, got: %q", got)
	}
}

// TestFFmpegExecutor_TemplateError_PointsAtArgIndex verifies that a template
// error reports which ffmpeg_args index failed so the user can find the
// offending entry in the YAML.
func TestFFmpegExecutor_TemplateError_PointsAtArgIndex(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:   "assemble",
		Type: StageTypeFFmpeg,
		FFmpegArgs: []string{
			"-i", "ok.wav",
			"-i", "{{ .stages.notreal.output }}", // undeclared dep
		},
		Output: "final.mp4",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	_, err := exec.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected template error")
	}
	if !strings.Contains(err.Error(), "ffmpeg_args[3]") {
		t.Errorf("error should point at offending index, got: %v", err)
	}
	if runner.callCount() != 0 {
		t.Errorf("ffmpeg should not be invoked after render failure, got %d call(s)", runner.callCount())
	}
}

// TestFFmpegExecutor_OutputUnderRunDir verifies the appended `-y <output>` is
// an absolute path under the run dir, even when the rendered output is
// relative or contains subdirectories.
func TestFFmpegExecutor_OutputUnderRunDir(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{}
	exec := &ffmpegExecutor{runner: runner}
	stage := &Stage{
		ID:         "assemble",
		Type:       StageTypeFFmpeg,
		FFmpegArgs: []string{"-i", "x.wav"},
		Output:     "renders/2024/final.mp4",
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	call := runner.call(0)
	// Verify -y + output is the LAST argv pair (after user args). What
	// ffmpeg writes is the scratch file beside the rendered path; what the
	// stage reports — and what a resumed run looks for — is the published
	// path.
	if len(call.Args) < 2 {
		t.Fatalf("argv too short: %v", call.Args)
	}
	wantFile := filepath.Join(runDir, "renders/2024/final.mp4")
	wantOut := partialOutputPath(wantFile)
	if call.Args[len(call.Args)-2] != "-y" || call.Args[len(call.Args)-1] != wantOut {
		t.Errorf("trailing argv = %v %v, want -y %s", call.Args[len(call.Args)-2], call.Args[len(call.Args)-1], wantOut)
	}
	if len(out.Files) != 1 || out.Files[0] != wantFile {
		t.Errorf("out.Files = %v, want [%s]", out.Files, wantFile)
	}
	if _, err := os.Stat(wantFile); err != nil {
		t.Errorf("the finished container must be published at the stage's output path: %v", err)
	}
}

// equalStringSlices is a small helper for argv comparisons. We avoid pulling
// reflect.DeepEqual for the better failure messages in our t.Errorf above
// (which prints both slices on mismatch).
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLineRingBuffer_TrimsOldLines verifies the stderr tail buffer keeps the
// last N lines and drops earlier ones. This is the mechanism that bounds the
// stderr context in ffmpeg error messages.
func TestLineRingBuffer_TrimsOldLines(t *testing.T) {
	buf := newLineRingBuffer(3)
	for i := 0; i < 10; i++ {
		fmt.Fprintf(buf, "line %d\n", i)
	}
	got := buf.String()
	want := "line 7\nline 8\nline 9"
	if got != want {
		t.Errorf("ring buffer = %q, want %q", got, want)
	}
}

// TestLineRingBuffer_CRSplit verifies the buffer treats '\r' as a line
// separator. ffmpeg uses CR to overwrite its progress line so we'd otherwise
// accumulate one giant line and lose the tail bound.
func TestLineRingBuffer_CRSplit(t *testing.T) {
	buf := newLineRingBuffer(2)
	// ffmpeg's progress pattern: CR-separated status updates ending in a
	// terminating newline.
	_, _ = buf.Write([]byte("frame=  1\rframe=  2\rframe=  3\rframe=  4\n"))
	got := buf.String()
	// Last two CR-bounded segments survive the ring; ffmpeg-style.
	if !strings.Contains(got, "frame=  3") || !strings.Contains(got, "frame=  4") {
		t.Errorf("ring buffer = %q, expected last two frames", got)
	}
}

// TestLineRingBuffer_AlwaysFullWriteCount drives ~10 KB of mixed-delimiter
// stderr through the ring buffer in chunks of varying size and asserts that
// EVERY Write returned the full input count. This is the regression test for
// the "ffmpeg: short write" bug: the ring's Write wraps cmd.Stderr inside an
// io.MultiWriter, and any short count (or any count != len(p)) is translated
// by MultiWriter into io.ErrShortWrite — which os/exec then surfaces as a
// non-zero exit and vamp reports as `stage assemble: ffmpeg: short write`,
// even when the output MP4 is valid. The bound is line count (max), not byte
// count, so the writer MUST consume the whole buffer regardless of size; ring
// storage drops oldest lines silently.
func TestLineRingBuffer_AlwaysFullWriteCount(t *testing.T) {
	buf := newLineRingBuffer(20) // realistic ffmpegStderrTailLines value
	// Build a ~10 KB stream that mixes '\n' lines (banner/codec block) with
	// long '\r'-terminated progress segments — ffmpeg's actual output shape.
	var blob bytes.Buffer
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&blob, "[libx264 @ 0x55] frame I:1 Avg QP:21.55 size: 12345 line %d\n", i)
	}
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&blob, "frame=  %d fps= 60 q=28.0 size=    %dkB time=00:00:%02d.00\r", i, i*64, i)
	}
	// Slice the blob into uneven chunks (matches a real os/exec stderr pump
	// where individual reads don't align with line boundaries).
	data := blob.Bytes()
	if len(data) < 10*1024 {
		t.Fatalf("test blob is %d bytes, want >= 10240", len(data))
	}
	chunkSizes := []int{1, 17, 64, 256, 1024, 4096, 7, 3}
	off := 0
	chunkIdx := 0
	for off < len(data) {
		size := chunkSizes[chunkIdx%len(chunkSizes)]
		chunkIdx++
		if off+size > len(data) {
			size = len(data) - off
		}
		chunk := data[off : off+size]
		n, err := buf.Write(chunk)
		if err != nil {
			t.Fatalf("Write(off=%d, size=%d): unexpected error %v", off, size, err)
		}
		if n != len(chunk) {
			t.Fatalf("Write(off=%d, size=%d): n=%d, want %d (short write would surface as `ffmpeg: short write` to the user)", off, size, n, len(chunk))
		}
		off += size
	}
	// MultiWriter parity check: a real ffmpeg invocation wraps the ring
	// inside io.MultiWriter(ring, stderr). Drive the same blob through a
	// MultiWriter targeting the ring + a discard sink to verify the wrapped
	// path doesn't trip io.ErrShortWrite either.
	ring2 := newLineRingBuffer(20)
	mw := io.MultiWriter(ring2, io.Discard)
	for off := 0; off < len(data); off += 1024 {
		end := off + 1024
		if end > len(data) {
			end = len(data)
		}
		n, err := mw.Write(data[off:end])
		if err != nil {
			t.Fatalf("MultiWriter.Write: unexpected error %v (this is the regression: io.MultiWriter returns ErrShortWrite when an inner writer's n != len(p))", err)
		}
		if n != end-off {
			t.Fatalf("MultiWriter.Write: n=%d, want %d", n, end-off)
		}
	}
}

// TestFFmpegExecutor_ZeroByteOutputFailsTheStage covers the failure the
// exit code does not report.
//
// ffmpeg can exit 0 having written a 0-byte container — a filtergraph
// error that never reaches the exit status. Nothing downstream notices:
// the file exists, the stage is green, and the run "succeeds" with
// silence in it. The cost is not one bad run. StageOutput.Files feeds
// tryCachePut, os.ReadFile of a 0-byte file returns an empty NON-NIL
// slice, and the cache's mode select reads non-nil Bytes as "binary
// output" — so the empty artefact becomes a content-addressed entry that
// replays on every later run with the same inputs.
//
// One case per output-producing mode, because the check has to be at
// each of them: a guard added to whichever executor was written last is
// how this got to four call sites with two of them covered.
func TestFFmpegExecutor_ZeroByteOutputFailsTheStage(t *testing.T) {
	cases := []struct {
		name  string
		stage func(runDir string) *Stage
		setup func(t *testing.T, runDir string)
		prior map[string]*stageResult
	}{
		{
			name: "args",
			stage: func(string) *Stage {
				return &Stage{
					ID:         "assemble",
					Type:       StageTypeFFmpeg,
					FFmpegArgs: []string{"-i", "in.png"},
					Output:     "final.mp4",
				}
			},
		},
		{
			name: "concat_wavs",
			setup: func(t *testing.T, runDir string) {
				for _, n := range []string{"segment_0.wav", "segment_1.wav"} {
					if err := os.WriteFile(filepath.Join(runDir, n), []byte("RIFF....WAVE"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			stage: func(string) *Stage {
				return &Stage{
					ID:         "join",
					Type:       StageTypeFFmpeg,
					ConcatWavs: true,
					Output:     "book.mp3",
				}
			},
		},
		{
			name: "concat_video",
			setup: func(t *testing.T, runDir string) {
				for _, n := range []string{"shot_0.mp4", "shot_1.mp4"} {
					if err := os.WriteFile(filepath.Join(runDir, n), []byte("clipdata"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			stage: func(runDir string) *Stage {
				return &Stage{
					ID:              "stitch",
					Type:            StageTypeFFmpeg,
					Inputs:          []string{"clips_list"},
					ConcatVideoFrom: "clips_list",
					ConcatVideoVar:  "item",
					ConcatVideoFile: "{{ .item }}",
					Output:          "final.mp4",
				}
			},
			prior: map[string]*stageResult{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runDir := t.TempDir()
			if c.setup != nil {
				c.setup(t, runDir)
			}
			prior := c.prior
			if c.name == "concat_video" {
				clips, _ := json.Marshal([]string{
					filepath.Join(runDir, "shot_0.mp4"),
					filepath.Join(runDir, "shot_1.mp4"),
				})
				prior = map[string]*stageResult{"clips_list": {Output: string(clips)}}
			}
			st := c.stage(runDir)
			runner := &recordingFFmpegRunner{emptyOutput: true}
			exec := &ffmpegExecutor{runner: runner}
			out, err := exec.Execute(context.Background(), StageInput{
				Stage: st, Prior: prior, RunDir: runDir,
			})
			if err == nil {
				t.Fatalf("ffmpeg exited 0 with a 0-byte output and the stage reported success: %v", out)
			}
			if !strings.Contains(err.Error(), "0-byte output") {
				t.Errorf("error should name the empty output: %v", err)
			}
			if runner.callCount() != 1 {
				t.Errorf("expected exactly one ffmpeg invocation, got %d", runner.callCount())
			}
		})
	}
}

// TestRequireNonEmptyOutput covers the shared check directly, including
// the case a Size()==0 test alone would miss: ffmpeg exiting 0 without
// creating the file at all.
func TestRequireNonEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "full.mp4")
	if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireNonEmptyOutput("s", "ffmpeg", empty); err == nil {
		t.Error("a 0-byte output must fail the stage")
	}
	if err := requireNonEmptyOutput("s", "ffmpeg", filepath.Join(dir, "absent.mp4")); err == nil {
		t.Error("a missing output must fail the stage")
	}
	if err := requireNonEmptyOutput("s", "ffmpeg", full); err != nil {
		t.Errorf("a non-empty output must pass: %v", err)
	}
}
