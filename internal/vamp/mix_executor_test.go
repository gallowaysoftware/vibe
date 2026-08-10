package vamp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The /bin/sh bodies that stand in for the real encoder. mix builds its argv
// with the output path LAST, which is also what the executor stats
// afterwards, so the fakes read `$@`'s tail rather than hunting a flag.
const (
	// ffmpegWritesAnEmptyContainer is the failure the exit status does not
	// report: a filtergraph error that leaves a 0-byte m4b behind and
	// still exits 0.
	ffmpegWritesAnEmptyContainer = `
for a in "$@"; do out="$a"; done
: > "$out"
exit 0`
	// ffmpegWritesNothingAtAll is its sibling — exit 0 and no file.
	ffmpegWritesNothingAtAll = `exit 0`
	// ffmpegWritesRealBytes is the success path.
	ffmpegWritesRealBytes = `
for a in "$@"; do out="$a"; done
printf 'M4B-ish bytes' > "$out"
exit 0`
	// ffmpegRecordsArgv appends every argument to $ARGV_LOG before
	// producing output, so a test can assert on the invocation.
	ffmpegRecordsArgv = `
for a in "$@"; do echo "$a" >> "$ARGV_LOG"; out="$a"; done
printf 'M4B-ish bytes' > "$out"
exit 0`
)

// writeMixScript writes a mix script JSON to <runDir>/script.json — the
// name newMixStage points ScriptFile at — and creates each named voice
// segment as a small non-empty file, so the executor's per-segment stat
// passes.
func writeMixScript(t *testing.T, runDir string, script mixScript) {
	t.Helper()
	for _, seg := range script.VoiceSegments {
		p := filepath.Join(runDir, seg)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("RIFF....WAVE"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "script.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newMixStage builds a minimal valid mix stage pointed at binary.
func newMixStage(binary, output string) *Stage {
	return &Stage{
		ID:         "assemble",
		Type:       StageTypeMix,
		ScriptFile: "script.json",
		Output:     output,
		Binary:     binary,
	}
}

// TestMixExecutor_HappyPath is the control every failure case below is
// read against, and it also pins the argv shape the fakes depend on: the
// output path last, every voice segment fed as its own -i, and a
// filter_complex that concats exactly as many inputs as were supplied.
func TestMixExecutor_HappyPath(t *testing.T) {
	runDir := t.TempDir()
	writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav", "seg_1.wav"}})
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.txt")
	fake := writeFakeBinary(t, binDir, "ffmpeg", ffmpegRecordsArgv)
	t.Setenv("ARGV_LOG", argvLog)

	out, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newMixStage(fake, "episode.m4b"),
		RunDir: runDir,
	})
	if err != nil {
		t.Fatalf("mix failed on a working encoder: %v", err)
	}
	want := filepath.Join(runDir, "episode.m4b")
	if len(out.Files) != 1 || out.Files[0] != want {
		t.Fatalf("StageOutput.Files = %v, want [%s]", out.Files, want)
	}

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// The encoder writes the scratch file beside the output; the executor
	// renames it into place after the 0-byte check. The last argument is
	// still the destination — the fakes rely on that — it is just not the
	// path a half-finished encode would be findable at.
	if argv[len(argv)-1] != partialOutputPath(want) {
		t.Errorf("output path must be the last argument (the executor and the fakes both rely on it): %v", argv)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the finished m4b must be published at the stage's output path: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "concat=n=2:v=0:a=1") {
		t.Errorf("filtergraph should concat both voice segments: %v", joined)
	}
	if !strings.Contains(joined, "loudnorm=I=-16") {
		t.Errorf("mix should apply the podcast-standard loudness default: %v", joined)
	}
}

// TestMixExecutor_ZeroByteOutputFailsTheStage pins the guard PR #71 added
// to this executor.
//
// ffmpeg exits 0 having written a 0-byte container when a filtergraph
// error never reaches the exit status. Nothing downstream notices: the
// file exists, the stage is green, and the run "succeeds" with an empty
// audiobook in it. The durable cost is the cache — StageOutput.Files is
// what tryCachePut reads, and os.ReadFile of a 0-byte file returns an
// empty but NON-NIL slice, which the cache's mode select reads as "binary
// output". So the second assertion matters as much as the first: a failed
// stage must return NO output, because an output is what would be
// admitted.
func TestMixExecutor_ZeroByteOutputFailsTheStage(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"an empty container", ffmpegWritesAnEmptyContainer, "0-byte output"},
		{"no file at all", ffmpegWritesNothingAtAll, "stat output"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runDir := t.TempDir()
			writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav"}})
			fake := writeFakeBinary(t, t.TempDir(), "ffmpeg", c.script)

			out, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
				Stage:  newMixStage(fake, "episode.m4b"),
				RunDir: runDir,
			})
			if err == nil {
				t.Fatalf("ffmpeg exited 0 having produced nothing and the stage reported success: %v", out)
			}
			if out != nil {
				t.Errorf("a failed stage must not hand a file to the cache layer: %v", out)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should name the empty output (%q): %v", c.want, err)
			}
		})
	}
}

// TestMixExecutor_NonZeroExitSurfacesFFmpegsOwnDiagnostic pins the
// stderr tail. A bare "exit status 1" tells an operator nothing about a
// filtergraph they wrote; the retained tail is the whole difference
// between a debuggable failure and a shrug.
//
// The nil-Log case is asserted deliberately: multi-stage capability
// groups pass a nil in.Log, and a tail that only existed when someone was
// watching would be missing exactly when the run was unattended.
//
// It also pins the SINK, which is the half that only shows up under
// -race. os/exec gives Stdout and Stderr separate pipes and separate
// copying goroutines unless the two fields compare EQUAL; the log vamp
// passes here is a per-item *bytes.Buffer that
// executeForeachStage hands out unguarded, so two goroutines writing it
// is a genuine data race on the shape a foreach mix stage takes every
// time. The fake below therefore talks on BOTH streams, at volume, so
// the detector has something to catch if the two fields ever diverge
// again.
func TestMixExecutor_NonZeroExitSurfacesFFmpegsOwnDiagnostic(t *testing.T) {
	const chatty = `
i=0
while [ $i -lt 200 ]; do
  echo "frame=$i fps=30 time=00:00:0$i"
  echo "[concat @ 0x1] Invalid file index 3 in filtergraph" >&2
  i=$((i+1))
done
exit 1`
	for _, withLog := range []bool{true, false} {
		name := "nil log"
		if withLog {
			name = "with log"
		}
		t.Run(name, func(t *testing.T) {
			runDir := t.TempDir()
			writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav"}})
			fake := writeFakeBinary(t, t.TempDir(), "ffmpeg", chatty)

			in := StageInput{Stage: newMixStage(fake, "episode.m4b"), RunDir: runDir}
			var log bytes.Buffer
			if withLog {
				in.Log = &log
			}
			_, err := (&mixExecutor{}).Execute(context.Background(), in)
			if err == nil {
				t.Fatal("a non-zero ffmpeg exit must fail the stage")
			}
			if !strings.Contains(err.Error(), "Invalid file index") {
				t.Errorf("ffmpeg's own diagnostic was dropped from the error: %v", err)
			}
			// The retained tail carries BOTH streams. This is the
			// deterministic shadow of the race above: the divergent form
			// routed stdout to the log only (or, with a nil log, to
			// /dev/null), so anything ffmpeg said there was missing from
			// the one place a failed stage's evidence survives.
			if !strings.Contains(err.Error(), "frame=") {
				t.Errorf("stdout never reached the error's stderr tail: %v", err)
			}
			if !withLog {
				return
			}
			// Both streams reach the operator's log. stdout carries
			// ffmpeg's progress and stderr carries the reason it died;
			// dropping either leaves a failure with half its evidence.
			got := log.String()
			if !strings.Contains(got, "Invalid file index") {
				t.Errorf("stderr never reached the run log:\n%s", got)
			}
			if !strings.Contains(got, "frame=") {
				t.Errorf("stdout never reached the run log:\n%s", got)
			}
		})
	}
}

// TestMixExecutor_RejectsAnEmptyScript covers the schema half. An empty
// voice_segments list would otherwise build an ffmpeg concat over zero
// inputs, which fails deep inside the filtergraph with a message about
// stream indices rather than about the script the showrunner wrote.
func TestMixExecutor_RejectsAnEmptyScript(t *testing.T) {
	runDir := t.TempDir()
	writeMixScript(t, runDir, mixScript{})
	_, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newMixStage("/nonexistent/ffmpeg", "episode.m4b"),
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("a script with no voice segments must fail the stage")
	}
	if !strings.Contains(err.Error(), "voice_segments is empty") {
		t.Errorf("error should name the empty field: %v", err)
	}
}

// TestMixExecutor_MissingVoiceSegmentNamesTheIndex pins the message, not
// just the failure. A 200-segment episode whose segment 137 never got
// written produces an ffmpeg error naming an input index that does not
// map back to anything the author can find; the executor's own stat is
// what turns that into a path.
func TestMixExecutor_MissingVoiceSegmentNamesTheIndex(t *testing.T) {
	runDir := t.TempDir()
	writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav", "seg_1.wav"}})
	if err := os.Remove(filepath.Join(runDir, "seg_1.wav")); err != nil {
		t.Fatal(err)
	}
	_, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newMixStage("/nonexistent/ffmpeg", "episode.m4b"),
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("a missing voice segment must fail the stage before ffmpeg runs")
	}
	if !strings.Contains(err.Error(), "voice segment 1") || !strings.Contains(err.Error(), "seg_1.wav") {
		t.Errorf("error should name both the index and the path: %v", err)
	}
}

// TestMixExecutor_RejectsIncoherentChapters pins the two chapter
// invariants. Both are cheap to violate from a generated script and both
// produce silent nonsense rather than an error if unchecked: an
// out-of-range start_segment indexes past the segment list, and
// non-increasing starts produce chapters whose END precedes their START,
// which Audiobookshelf and Apple Books render as an unnavigable file
// rather than refusing.
func TestMixExecutor_RejectsIncoherentChapters(t *testing.T) {
	cases := []struct {
		name     string
		chapters []mixChapter
		want     string
	}{
		{
			name:     "past the end",
			chapters: []mixChapter{{Title: "One", StartSegment: 0}, {Title: "Two", StartSegment: 9}},
			want:     "out of range",
		},
		{
			name:     "negative",
			chapters: []mixChapter{{Title: "One", StartSegment: -1}},
			want:     "out of range",
		},
		{
			name:     "not increasing",
			chapters: []mixChapter{{Title: "One", StartSegment: 1}, {Title: "Two", StartSegment: 1}},
			want:     "strictly greater",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runDir := t.TempDir()
			writeMixScript(t, runDir, mixScript{
				VoiceSegments: []string{"seg_0.wav", "seg_1.wav"},
				Chapters:      c.chapters,
			})
			_, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
				Stage:  newMixStage("/nonexistent/ffmpeg", "episode.m4b"),
				RunDir: runDir,
			})
			if err == nil {
				t.Fatalf("incoherent chapters (%s) must fail the stage", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error should say %q: %v", c.want, err)
			}
		})
	}
}

// TestMixExecutor_MP3OutputAnnouncesTheDroppedChapters pins a guard whose
// entire value is the log line. ffmpeg's mp3 muxer carries no chapter
// atoms, so a chaptered script rendered to .mp3 loses them — silently, if
// nobody says so, and the showrunner that emitted the chapters has no way
// to learn its work was discarded.
func TestMixExecutor_MP3OutputAnnouncesTheDroppedChapters(t *testing.T) {
	runDir := t.TempDir()
	writeMixScript(t, runDir, mixScript{
		VoiceSegments: []string{"seg_0.wav", "seg_1.wav"},
		Chapters:      []mixChapter{{Title: "One", StartSegment: 0}, {Title: "Two", StartSegment: 1}},
	})
	fake := writeFakeBinary(t, t.TempDir(), "ffmpeg", ffmpegWritesRealBytes)

	var log bytes.Buffer
	if _, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newMixStage(fake, "episode.mp3"),
		RunDir: runDir,
		Log:    &log,
	}); err != nil {
		t.Fatalf("an mp3 mix with chapters must still produce audio: %v", err)
	}
	if !strings.Contains(log.String(), "dropping 2 chapter(s)") {
		t.Errorf("the run log must say the chapters were discarded:\n%s", log.String())
	}
	if !strings.Contains(log.String(), ".m4b") {
		t.Errorf("the message should name the container that would have kept them:\n%s", log.String())
	}
}

// TestMixExecutor_MetadataIsOrderedAndRendered pins two properties that
// only look cosmetic. The values are templates, so `{{ .inputs.topic }}`
// must reach the file; and the KEYS are sorted, so two runs with the same
// metadata produce byte-identical ffmpeg invocations — which is what lets
// the mp4 muxer's output be compared across runs at all.
func TestMixExecutor_MetadataIsOrderedAndRendered(t *testing.T) {
	runDir := t.TempDir()
	writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav"}})
	binDir := t.TempDir()
	argvLog := filepath.Join(binDir, "argv.txt")
	fake := writeFakeBinary(t, binDir, "ffmpeg", ffmpegRecordsArgv)
	t.Setenv("ARGV_LOG", argvLog)

	st := newMixStage(fake, "episode.m4b")
	st.Metadata = map[string]string{
		"title":  "{{ .inputs.topic }}",
		"artist": "Reference Fleet",
		"album":  "Season 1",
	}
	if _, err := (&mixExecutor{}).Execute(context.Background(), StageInput{
		Stage:  st,
		RunDir: runDir,
		Inputs: map[string]string{"topic": "Goblin Economics"},
	}); err != nil {
		t.Fatalf("mix failed: %v", err)
	}

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var tags []string
	for i, a := range argv {
		if a == "-metadata" && i+1 < len(argv) {
			tags = append(tags, argv[i+1])
		}
	}
	want := []string{"album=Season 1", "artist=Reference Fleet", "title=Goblin Economics"}
	if len(tags) != len(want) {
		t.Fatalf("metadata tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("metadata tags = %v, want %v (sorted keys keep the argv reproducible)", tags, want)
		}
	}
}

// TestMixExecutor_CancelledContextEndsTheStage pins the bound. A mix is
// the last stage of a long pipeline and encodes for minutes; a stage that
// ignored its cancellation would keep a CPU busy after the run it belongs
// to was abandoned.
//
// This case exercises the HARD half of that bound rather than the easy
// one. mix always attaches a stderr tail buffer, so cmd.Stderr is a pipe;
// the killed child's descendant keeps that pipe open, and Cmd.Wait does
// not return until the pipe closes — with a WaitDelay of zero, documented
// to mean "wait indefinitely", this call would sit here for the full
// sixty seconds with the deadline having fired exactly on time. Observed
// runtime is ~2s, which is subprocessKillGrace, which is the point.
func TestMixExecutor_CancelledContextEndsTheStage(t *testing.T) {
	runDir := t.TempDir()
	writeMixScript(t, runDir, mixScript{VoiceSegments: []string{"seg_0.wav"}})
	fake := writeFakeBinary(t, t.TempDir(), "ffmpeg", `sleep 60`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := (&mixExecutor{}).Execute(ctx, StageInput{
		Stage:  newMixStage(fake, "episode.m4b"),
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("a cancelled mix stage must fail")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s; the stage outlived its context", elapsed)
	}
}

// TestResolveInRunDir keeps the path rule explicit. Every mix path field
// goes through it, and the rule is what lets a script store run-relative
// segment names while an upstream stage's absolute output path also
// works.
func TestResolveInRunDir(t *testing.T) {
	if got := resolveInRunDir("seg_0.wav", "/run"); got != filepath.Join("/run", "seg_0.wav") {
		t.Errorf("relative path = %q", got)
	}
	abs := filepath.Join(string(filepath.Separator), "elsewhere", "seg_0.wav")
	if got := resolveInRunDir(abs, "/run"); got != abs {
		t.Errorf("absolute path was rebased under the run dir: %q", got)
	}
}
