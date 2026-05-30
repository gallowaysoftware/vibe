package vamp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeShortFixtures writes a script JSON plus the referenced clip/audio files
// into runDir and returns the script path.
func writeShortFixtures(t *testing.T, runDir string, script shortScript) string {
	t.Helper()
	for i, sh := range script.Shots {
		for _, p := range []string{sh.Video, sh.Audio} {
			if p == "" {
				continue
			}
			ap := p
			if !filepath.IsAbs(ap) {
				ap = filepath.Join(runDir, p)
			}
			if err := os.MkdirAll(filepath.Dir(ap), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ap, []byte("data"+string(rune('0'+i))), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, _ := json.Marshal(script)
	sp := filepath.Join(runDir, "assembly.json")
	if err := os.WriteFile(sp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestShortExecutor_BuildsFiltergraph(t *testing.T) {
	runDir := t.TempDir()
	script := shortScript{
		Shots: []shortShot{
			{Video: "clip_0.mp4", Audio: "vo_0.wav", Caption: "Don't touch: it's alive"},
			{Video: "clip_1.mp4", Audio: "vo_1.wav"},
		},
	}
	writeShortFixtures(t, runDir, script)

	runner := &recordingFFmpegRunner{}
	exec := &shortExecutor{
		runner: runner,
		probe: func(_ context.Context, p string) (int64, error) {
			if strings.Contains(p, "vo_0") {
				return 3000, nil
			}
			return 5000, nil
		},
	}
	stage := &Stage{ID: "assemble", Type: StageTypeShort, ScriptFile: "assembly.json", Output: "final.mp4"}
	// Pre-create the output so the 0-byte guard (which stats after the fake
	// runner "runs" without writing) passes.
	if err := os.WriteFile(filepath.Join(runDir, "final.mp4"), []byte("mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := StageInput{Stage: stage, RunDir: runDir}
	out, err := exec.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := filepath.Join(runDir, "final.mp4"); len(out.Files) != 1 || out.Files[0] != want {
		t.Fatalf("output = %v, want [%s]", out.Files, want)
	}
	if runner.callCount() != 1 {
		t.Fatalf("expected 1 ffmpeg call, got %d", runner.callCount())
	}
	joined := strings.Join(runner.call(0).Args, " ")
	for _, want := range []string{
		"scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920",
		"tpad=stop_mode=clone:stop_duration=3.000,trim=duration=3.000", // shot 0 fit to 3s audio
		"tpad=stop_mode=clone:stop_duration=5.000,trim=duration=5.000", // shot 1 fit to 5s audio
		"concat=n=2:v=1:a=1[cv][ca]",
		"loudnorm=I=-16",
		"-map [cv]", "-map [na]", "-c:v libx264", "-c:a aac",
		"drawtext=", "textfile=", "expansion=none", ".caption-0.txt",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("filtergraph/args missing %q in:\n%s", want, joined)
		}
	}
	// Shot 1 has no caption → only one drawtext in the graph.
	if n := strings.Count(joined, "drawtext="); n != 1 {
		t.Errorf("expected exactly 1 drawtext, got %d", n)
	}
	// The caption (apostrophe-bearing here) is written verbatim to a file —
	// no escaping, the bug that broke apostrophe captions inline.
	capBytes, err := os.ReadFile(filepath.Join(runDir, ".caption-0.txt"))
	if err != nil {
		t.Fatalf("caption file: %v", err)
	}
	if !strings.Contains(string(capBytes), "Don't touch: it's alive") {
		t.Errorf("caption file = %q, want the literal apostrophe-bearing caption", string(capBytes))
	}
}

func TestShortExecutor_BackgroundMusicDucks(t *testing.T) {
	runDir := t.TempDir()
	script := shortScript{
		Shots:           []shortShot{{Video: "c.mp4", Audio: "a.wav"}},
		BackgroundMusic: "bed.mp3",
		MusicGainDB:     -20,
	}
	writeShortFixtures(t, runDir, script)
	if err := os.WriteFile(filepath.Join(runDir, "bed.mp3"), []byte("music"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "out.mp4"), []byte("mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingFFmpegRunner{}
	exec := &shortExecutor{
		runner: runner,
		probe:  func(_ context.Context, _ string) (int64, error) { return 4000, nil },
	}
	stage := &Stage{ID: "assemble", Type: StageTypeShort, ScriptFile: "assembly.json", Output: "out.mp4"}
	if _, err := exec.Execute(context.Background(), StageInput{Stage: stage, RunDir: runDir}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	joined := strings.Join(runner.call(0).Args, " ")
	for _, want := range []string{
		"volume=-20dB", "amix=inputs=2:duration=first:normalize=0[outa]", "-map [outa]",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("music mix missing %q in:\n%s", want, joined)
		}
	}
}
