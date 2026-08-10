package vamp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/comfyui"
	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
)

// escapingOutput is the `output:` template every case below declares. The
// leading ".." is the shape a model samples into a filename slot — via a
// foreach item field, or (as the second table here shows) via a prior
// stage's raw text interpolated with {{ .stages.x.output }}.
func escapingOutput(name, ext string) string {
	return "../" + strings.ReplaceAll(name, "/", "-") + ext
}

// escapedPath is where that template lands if nothing refuses it: one
// level ABOVE the run dir, which t.TempDir gives us for free (run dirs are
// .../TestName/001, so "../x" is .../TestName/x).
func escapedPath(runDir, name, ext string) string {
	return filepath.Join(runDir, "..", strings.ReplaceAll(name, "/", "-")+ext)
}

// TestBinaryExecutorsRefuseAnOutputPathOutsideTheRunDir drives every
// file-producing executor with an `output:` template that resolves above
// the run dir and asserts each one refuses before it writes.
//
// These executors report their result in StageOutput.Files, which is
// exactly why they were unguarded: executeStage short-circuits on
// len(out.Files) > 0 BEFORE its own guarded renderOutputPath call, so for
// a non-foreach binary stage the guarded render never ran at all and each
// executor joined its own rendered path to RunDir unchecked.
func TestBinaryExecutorsRefuseAnOutputPathOutsideTheRunDir(t *testing.T) {
	cases := []struct {
		name  string
		ext   string
		build func(t *testing.T, runDir, output string) (StageExecutor, StageInput)
	}{
		{
			name: "ffmpeg-args",
			ext:  ".mp4",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				st := &Stage{ID: "assemble", Type: StageTypeFFmpeg, FFmpegArgs: []string{"-i", "in.wav"}, Output: output}
				return &ffmpegExecutor{runner: &recordingFFmpegRunner{}}, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name: "ffmpeg-args-from-a-prior-stages-text",
			ext:  ".mp4",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				// The model-controlled shape: `pick` is a text stage whose
				// output is interpolated straight into the path.
				st := &Stage{
					ID: "assemble", Type: StageTypeFFmpeg, Inputs: []string{"pick"},
					FFmpegArgs: []string{"-i", "in.wav"},
					Output:     "{{ .stages.pick.output }}" + filepath.Ext(output),
				}
				prior := map[string]*stageResult{"pick": {Output: strings.TrimSuffix(output, filepath.Ext(output))}}
				return &ffmpegExecutor{runner: &recordingFFmpegRunner{}}, StageInput{Stage: st, RunDir: runDir, Prior: prior}
			},
		},
		{
			name: "ffmpeg-concat-wavs",
			ext:  ".mp3",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				for _, n := range []string{"segment_0.wav", "segment_1.wav"} {
					if err := os.WriteFile(filepath.Join(runDir, n), []byte("RIFF....WAVE"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				st := &Stage{ID: "join", Type: StageTypeFFmpeg, ConcatWavs: true, Output: output}
				return &ffmpegExecutor{runner: &recordingFFmpegRunner{}}, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name: "ffmpeg-concat-video",
			ext:  ".mp4",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				for _, n := range []string{"shot_0.mp4", "shot_1.mp4"} {
					if err := os.WriteFile(filepath.Join(runDir, n), []byte("clipdata"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				clips, _ := json.Marshal([]string{"shot_0.mp4", "shot_1.mp4"})
				st := &Stage{
					ID: "stitch", Type: StageTypeFFmpeg, Inputs: []string{"clips"},
					ConcatVideoFrom: "clips", ConcatVideoVar: "item", ConcatVideoFile: "{{ .item }}",
					Output: output,
				}
				prior := map[string]*stageResult{"clips": {Output: string(clips)}}
				return &ffmpegExecutor{runner: &recordingFFmpegRunner{}}, StageInput{Stage: st, RunDir: runDir, Prior: prior}
			},
		},
		{
			name: "ffmpeg-m4b",
			ext:  ".m4b",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				if err := os.WriteFile(filepath.Join(runDir, "ch_0.mp3"), []byte("audio"), 0o644); err != nil {
					t.Fatal(err)
				}
				chapters, _ := json.Marshal([]map[string]string{{"file": "ch_0.mp3", "title": "One"}})
				st := &Stage{
					ID: "book", Type: StageTypeFFmpeg, Inputs: []string{"chapters"},
					M4BFrom: "chapters", M4BVar: "ch", M4BFile: "{{ index .ch \"file\" }}", M4BChapter: "{{ index .ch \"title\" }}",
					Output: output,
				}
				prior := map[string]*stageResult{"chapters": {Output: string(chapters)}}
				return &ffmpegExecutor{runner: &recordingFFmpegRunner{}}, StageInput{Stage: st, RunDir: runDir, Prior: prior}
			},
		},
		{
			name: "audio-piper",
			ext:  ".wav",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				voices := t.TempDir()
				makeVoiceFile(t, voices, "narrator")
				runner := &recordingRunner{onRun: func(call recordedRunCall) error {
					// The real piper writes wherever --output-file points.
					out, _ := argByFlag(call.Args, "--output-file")
					return os.WriteFile(out, []byte("RIFF....WAVEpwned"), 0o644)
				}}
				st := &Stage{
					ID: "say", Type: StageTypeAudio, Text: "hello", Voice: "narrator",
					VoicesDir: voices, Binary: "piper", Output: output,
				}
				return &audioExecutor{runner: runner}, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name: "pandoc",
			ext:  ".epub",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				writePandocSource(t, runDir)
				fake := writeFakeBinary(t, t.TempDir(), "pandoc", pandocWritesRealBytes)
				st := newPandocStage(fake)
				st.Output = output
				return &pandocExecutor{}, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name: "mix",
			ext:  ".m4b",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav"}})
				fake := writeFakeBinary(t, t.TempDir(), "ffmpeg", ffmpegWritesRealBytes)
				return &mixExecutor{}, StageInput{Stage: newMixStage(fake, output), RunDir: runDir}
			},
		},
		{
			name: "short",
			ext:  ".mp4",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				writeShortFixtures(t, runDir, shortScript{Shots: []shortShot{{Video: "clip_0.mp4", Audio: "vo_0.wav"}}})
				exec := &shortExecutor{
					runner: &recordingFFmpegRunner{},
					probe:  func(context.Context, string) (int64, error) { return 1000, nil },
				}
				st := &Stage{ID: "assemble", Type: StageTypeShort, ScriptFile: "assembly.json", Output: output}
				return exec, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name: "comfyui",
			ext:  ".png",
			build: func(t *testing.T, runDir, output string) (StageExecutor, StageInput) {
				_, srv := newFakeComfyServer(t, "p1",
					[]comfyui.OutputFile{{Filename: "out.png", Type: "output"}}, []byte("PNGDATA12"))
				pipelineDir := t.TempDir()
				if err := os.WriteFile(filepath.Join(pipelineDir, "workflow.json"),
					[]byte(`{"6":{"class_type":"CLIPTextEncode","inputs":{"text":""}}}`), 0o644); err != nil {
					t.Fatal(err)
				}
				exec := &comfyuiExecutor{
					pollInterval: time.Millisecond,
					newClient:    func(string) *comfyui.Client { return comfyui.New(srv.URL, srv.Client()) },
				}
				st := &Stage{ID: "render", Type: StageTypeComfyUI, Workflow: "workflow.json", Output: output}
				return exec, StageInput{Stage: st, RunDir: runDir, PipelineDir: pipelineDir, BackendAddr: srv.URL}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runDir := t.TempDir()
			output := escapingOutput(c.name, c.ext)
			landing := escapedPath(runDir, c.name, c.ext)
			exec, in := c.build(t, runDir, output)
			out, err := exec.Execute(context.Background(), in)
			if err == nil {
				t.Fatalf("executor accepted an output path outside the run dir and reported %v", out)
			}
			if !errors.Is(err, errOutputPathEscape) {
				t.Fatalf("error must be the containment refusal, got: %v", err)
			}
			if _, statErr := os.Stat(landing); statErr == nil {
				t.Fatalf("a file was written OUTSIDE the run dir at %s", landing)
			}
		})
	}
}

// TestMaterializeCacheHitRefusesAnOutputPathOutsideTheRunDir covers the
// eleventh site: a cache hit writes the cached bytes to the rendered path
// itself, so a poisoned template escapes without any executor running.
func TestMaterializeCacheHitRefusesAnOutputPathOutsideTheRunDir(t *testing.T) {
	runDir := t.TempDir()
	e := &Executor{RunDir: runDir}
	st := &Stage{ID: "render", Type: StageTypeComfyUI, Output: "../cache-pwned.png"}
	landing := filepath.Join(runDir, "..", "cache-pwned.png")
	_, err := e.materializeCacheHit(context.Background(), st, StageInput{Stage: st, RunDir: runDir},
		&cache.Entry{IsBinary: true, Output: []byte("PNGDATA12")})
	if err == nil {
		t.Fatal("materializeCacheHit accepted an output path outside the run dir")
	}
	if !errors.Is(err, errOutputPathEscape) {
		t.Fatalf("error must be the containment refusal, got: %v", err)
	}
	if _, statErr := os.Stat(landing); statErr == nil {
		t.Fatalf("cached bytes were written OUTSIDE the run dir at %s", landing)
	}
}

// TestComfyUIStageDoesNotWriteOutsideTheRunDir is the same escape driven
// through a full Executor.Run rather than a direct Execute call — the
// path a real pipeline takes, and the one the short-circuit at
// executeStage's len(out.Files) check left unguarded.
func TestComfyUIStageDoesNotWriteOutsideTheRunDir(t *testing.T) {
	workflow := `{"6":{"class_type":"CLIPTextEncode","inputs":{"text":""}}}`
	pipeline := &Pipeline{
		Name: "img",
		Stages: []Stage{{
			ID: "render", Type: StageTypeComfyUI, Capability: "image",
			Output:     "../{{.inputs.slug}}.png",
			Parameters: map[string]string{"6.text": "hello"},
		}},
	}
	exec, runDir, _, fcs := stubComfyRuntime(t, pipeline, workflow)
	exec.Inputs = map[string]string{"slug": "run-pwned"}
	err := exec.Run(context.Background())
	if err == nil {
		t.Fatal("the run succeeded with an output path outside the run dir")
	}
	if !errors.Is(err, errOutputPathEscape) {
		t.Fatalf("error must be the containment refusal, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "..", "run-pwned.png")); statErr == nil {
		t.Fatal("comfyui wrote OUTSIDE the run dir")
	}
	// The refusal belongs before the work: a rejected path must not have
	// cost a GPU minute.
	if fcs.submitCount() != 0 {
		t.Errorf("workflow was submitted %d time(s) for a path that could never be written", fcs.submitCount())
	}
}

// spyStageExecutor records whether it was called at all.
type spyStageExecutor struct {
	calls int
	out   *StageOutput
}

func (s *spyStageExecutor) Execute(context.Context, StageInput) (*StageOutput, error) {
	s.calls++
	return s.out, nil
}

// TestExecuteStageValidatesTheOutputPathBeforeTheExecutorRuns pins the
// centralisation itself: the path is rendered and checked ONCE, before
// dispatch, so a stage type that reports out.Files can no longer reach
// its own unguarded render.
func TestExecuteStageValidatesTheOutputPathBeforeTheExecutorRuns(t *testing.T) {
	runDir := t.TempDir()
	spy := &spyStageExecutor{out: &StageOutput{Files: []string{filepath.Join(runDir, "..", "spy.mp4")}}}
	e := &Executor{RunDir: runDir}
	// Run() builds both in production; this test calls executeStage directly.
	e.registry = map[StageType]StageExecutor{StageTypeFFmpeg: spy}
	e.stageOutputs = map[string]*stageResult{}
	st := &Stage{ID: "assemble", Type: StageTypeFFmpeg, FFmpegArgs: []string{"-i", "x"}, Output: "../spy.mp4"}
	err := e.executeStage(context.Background(), st, "", "", "", 1, nil)
	if err == nil {
		t.Fatal("executeStage ran a stage whose output path leaves the run dir")
	}
	if !errors.Is(err, errOutputPathEscape) {
		t.Fatalf("error must be the containment refusal, got: %v", err)
	}
	if spy.calls != 0 {
		t.Errorf("the executor was invoked %d time(s) before the path was checked", spy.calls)
	}
}

// TestACrashedProducerLeavesNothingAtTheOutputPath is the resume half.
//
// `--resume` treats a present, non-empty file at a binary stage's output
// path as a completed stage — that IS the whole integrity check for these
// types. Every one of these executors used to point its producer straight
// at the final path (`ffmpeg -y <out>`, `piper --output-file <out>`,
// pandoc `-o <out>`), so a SIGKILL / OOM / power cut partway through a
// long encode left a real file, at the real path, with a real non-zero
// size. The next --resume accepts it and a truncated m4b goes to the
// publish stage.
//
// Each case simulates the crash the same way the kernel does: the
// producer writes a plausible partial header and then dies.
func TestACrashedProducerLeavesNothingAtTheOutputPath(t *testing.T) {
	// A genuine partial MP4/M4A header: 21 bytes that any size check
	// passes.
	partial := []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A ")
	cases := []struct {
		name   string
		output string
		build  func(t *testing.T, runDir string) (StageExecutor, StageInput)
	}{
		{
			name:   "ffmpeg",
			output: "book.m4b",
			build: func(t *testing.T, runDir string) (StageExecutor, StageInput) {
				runner := &recordingFFmpegRunner{
					wantErr: errors.New("signal: killed"),
					onRun: func(call recordedFFmpegCall) error {
						return os.WriteFile(call.Args[len(call.Args)-1], partial, 0o644)
					},
				}
				st := &Stage{ID: "book", Type: StageTypeFFmpeg, FFmpegArgs: []string{"-i", "in.wav"}, Output: "book.m4b"}
				return &ffmpegExecutor{runner: runner}, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name:   "audio-piper",
			output: "segment_0.wav",
			build: func(t *testing.T, runDir string) (StageExecutor, StageInput) {
				voices := t.TempDir()
				makeVoiceFile(t, voices, "narrator")
				runner := &recordingRunner{
					wantErr: errors.New("signal: killed"),
					onRun: func(call recordedRunCall) error {
						out, _ := argByFlag(call.Args, "--output-file")
						return os.WriteFile(out, partial, 0o644)
					},
				}
				st := &Stage{
					ID: "say", Type: StageTypeAudio, Text: "hello", Voice: "narrator",
					VoicesDir: voices, Binary: "piper", Output: "segment_0.wav",
				}
				return &audioExecutor{runner: runner}, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name:   "pandoc",
			output: "book.epub",
			build: func(t *testing.T, runDir string) (StageExecutor, StageInput) {
				writePandocSource(t, runDir)
				fake := writeFakeBinary(t, t.TempDir(), "pandoc", `
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf 'PARTIAL-EPUB' > "$out"
exit 137`)
				return &pandocExecutor{}, StageInput{Stage: newPandocStage(fake), RunDir: runDir}
			},
		},
		{
			name:   "short",
			output: "final.mp4",
			build: func(t *testing.T, runDir string) (StageExecutor, StageInput) {
				writeShortFixtures(t, runDir, shortScript{Shots: []shortShot{{Video: "clip_0.mp4", Audio: "vo_0.wav"}}})
				runner := &recordingFFmpegRunner{
					wantErr: errors.New("signal: killed"),
					onRun: func(call recordedFFmpegCall) error {
						return os.WriteFile(call.Args[len(call.Args)-1], partial, 0o644)
					},
				}
				exec := &shortExecutor{runner: runner, probe: func(context.Context, string) (int64, error) { return 1000, nil }}
				st := &Stage{ID: "assemble", Type: StageTypeShort, ScriptFile: "assembly.json", Output: "final.mp4"}
				return exec, StageInput{Stage: st, RunDir: runDir}
			},
		},
		{
			name:   "mix",
			output: "episode.m4b",
			build: func(t *testing.T, runDir string) (StageExecutor, StageInput) {
				writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav"}})
				fake := writeFakeBinary(t, t.TempDir(), "ffmpeg", `
for a in "$@"; do out="$a"; done
printf 'PARTIAL' > "$out"
exit 137`)
				return &mixExecutor{}, StageInput{Stage: newMixStage(fake, "episode.m4b"), RunDir: runDir}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runDir := t.TempDir()
			exec, in := c.build(t, runDir)
			outAbs := filepath.Join(runDir, c.output)
			// The previous run's good artefact. A producer that writes in
			// place destroys it the moment it opens the file.
			if err := os.WriteFile(outAbs, []byte("COMPLETE-FROM-THE-LAST-RUN"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := exec.Execute(context.Background(), in); err == nil {
				t.Fatal("a killed producer must fail the stage")
			}
			body, err := os.ReadFile(outAbs)
			if err != nil {
				t.Fatalf("read %s: %v", c.output, err)
			}
			if string(body) != "COMPLETE-FROM-THE-LAST-RUN" {
				t.Errorf("the crash left %q at the output path; --resume reads a present non-empty file as a COMPLETED stage", body)
			}
		})
	}
}

// TestResumeRefusesWhatAKilledEncodeLeftBehind joins the two halves: run a
// stage whose encoder is killed mid-write, then ask the resume pre-pass
// whether the stage is done. It must say no.
func TestResumeRefusesWhatAKilledEncodeLeftBehind(t *testing.T) {
	runDir := t.TempDir()
	runner := &recordingFFmpegRunner{
		wantErr: errors.New("signal: killed"),
		onRun: func(call recordedFFmpegCall) error {
			return os.WriteFile(call.Args[len(call.Args)-1], []byte("\x00\x00\x00\x20ftypM4A "), 0o644)
		},
	}
	st := &Stage{ID: "book", Type: StageTypeFFmpeg, FFmpegArgs: []string{"-i", "in.wav"}, Output: "book.m4b"}
	if _, err := (&ffmpegExecutor{runner: runner}).Execute(context.Background(), StageInput{Stage: st, RunDir: runDir}); err == nil {
		t.Fatal("a killed encoder must fail the stage")
	}
	e := &Executor{RunDir: runDir, ResumeDir: runDir}
	e.stageOutputs = map[string]*stageResult{}
	resumed, err := e.tryResumeStage(st)
	if err != nil {
		t.Fatalf("tryResumeStage: %v", err)
	}
	if resumed {
		t.Error("--resume accepted a stage whose encode was killed; the next stage publishes a truncated file")
	}
}

// TestKokoroPublishesThroughTheScratchFile is the one file-producing path
// with no subprocess to kill: the TTS response is bytes in memory and
// os.WriteFile truncates the destination before it writes them.
//
// The observable that separates "write in place" from "write then
// rename" without a real crash is a read-only artefact at the output
// path: os.WriteFile needs write permission on the FILE and fails, while
// a rename needs it on the DIRECTORY and succeeds. That is also a real
// situation — a previous run's segment that somebody chmod'd, or a
// restored backup — and the correct answer is that the new audio lands.
func TestKokoroPublishesThroughTheScratchFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this test uses as its observable")
	}
	srv := httptestNewWAVServer(t, "RIFF....WAVEfresh-take")
	runDir := t.TempDir()
	outAbs := filepath.Join(runDir, "segment_0.wav")
	if err := os.WriteFile(outAbs, []byte("RIFF....WAVEprevious"), 0o444); err != nil {
		t.Fatal(err)
	}
	out, err := (&audioExecutor{}).Execute(context.Background(), StageInput{
		Stage: &Stage{
			ID: "say", Type: StageTypeAudio, Voice: "af_bella", Text: "hello",
			Output: "segment_0.wav", Engine: AudioEngineKokoro, EngineURL: srv,
		},
		RunDir: runDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(out.Files) != 1 || out.Files[0] != outAbs {
		t.Fatalf("out.Files = %v, want [%s]", out.Files, outAbs)
	}
	body, err := os.ReadFile(outAbs)
	if err != nil {
		t.Fatal(err)
	}
	// (The RIFF size field is patched on the way through, so compare the
	// payload rather than the whole envelope.)
	if !strings.HasSuffix(string(body), "fresh-take") {
		t.Errorf("output = %q, want the new take", body)
	}
	if _, err := os.Stat(partialOutputPath(outAbs)); err == nil {
		t.Error("the scratch file outlived the stage")
	}
}

// httptestNewWAVServer serves one canned WAV body at the Kokoro endpoint
// and returns its base URL.
func httptestNewWAVServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSuccessfulProducersLeaveNoScratchBehind: the partial file is an
// implementation detail of "not visible until complete", and a run dir
// that accumulates them would feed them back to concat_wavs' walk and to
// the operator's file browser.
func TestSuccessfulProducersLeaveNoScratchBehind(t *testing.T) {
	runDir := t.TempDir()
	st := &Stage{ID: "assemble", Type: StageTypeFFmpeg, FFmpegArgs: []string{"-i", "in.wav"}, Output: "out/final.mp4"}
	out, err := (&ffmpegExecutor{runner: &recordingFFmpegRunner{}}).Execute(context.Background(), StageInput{Stage: st, RunDir: runDir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := filepath.Join(runDir, "out", "final.mp4"); len(out.Files) != 1 || out.Files[0] != want {
		t.Fatalf("out.Files = %v, want [%s]", out.Files, want)
	}
	entries, err := os.ReadDir(filepath.Join(runDir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "final.mp4" {
		t.Errorf("output dir = %v, want exactly [final.mp4]", names)
	}
}

// TestConcatWavsIgnoresScratchFiles: the scratch file a killed piper
// leaves behind still ends in .wav, and concat_wavs walks the run dir for
// exactly that. Gluing a half-written segment into the audiobook is the
// same defect one layer down, and it also fires WITHOUT a crash — a
// sibling foreach item writing while the walk runs.
func TestConcatWavsIgnoresScratchFiles(t *testing.T) {
	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, "segment_0.wav"), []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Whatever the scratch file is named, it is hidden (dot-prefixed):
	// vamp's own run-dir scratch (.ffmpeg-concat.*, .caption-*) already is.
	if err := os.WriteFile(filepath.Join(runDir, ".vamp-partial.segment_1.wav"), []byte("HALF"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingFFmpegRunner{}
	st := &Stage{ID: "join", Type: StageTypeFFmpeg, ConcatWavs: true, Output: "book.mp3"}
	if _, err := (&ffmpegExecutor{runner: runner}).Execute(context.Background(), StageInput{Stage: st, RunDir: runDir}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	list, err := os.ReadFile(filepath.Join(runDir, ".ffmpeg-concat.join-0.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(list), "vamp-partial") {
		t.Errorf("the concat list glued in a scratch file:\n%s", list)
	}
	if !strings.Contains(string(list), "segment_0.wav") {
		t.Errorf("the concat list dropped a real segment:\n%s", list)
	}
}
