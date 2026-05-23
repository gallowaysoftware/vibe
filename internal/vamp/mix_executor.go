package vamp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mixExecutor renders a structured-script JSON file into a single m4b
// audiobook / podcast episode. The script is the showrunner's output:
// an ordered list of voice segments (paths to WAV / MP3), an optional
// intro and outro music file, and an optional list of cover-image
// + metadata fields. The executor concatenates voices in order,
// crossfades intro/outro music, applies loudnorm to podcast-standard
// -16 LUFS, and encodes the result as an .m4b with embedded cover.
//
// v1 mix shape (this implementation): linear concat. v2 (future)
// adds per-segment ambient beds, sidechain ducking of music under
// voice, SFX punch-ins, multi-scene crossfades. The script-JSON
// schema is designed to grow to v2 without breaking v1 — the v1
// executor just ignores fields it doesn't understand.
type mixExecutor struct{}

// Compile-time guarantee that mixExecutor satisfies StageExecutor.
var _ StageExecutor = (*mixExecutor)(nil)

// mixScript is the on-disk schema mix stages consume.
type mixScript struct {
	// VoiceSegments is the ordered list of voice audio file paths
	// (relative to RunDir or absolute). At least one segment is
	// required; the m4b is built from these in order.
	VoiceSegments []string `json:"voice_segments"`
	// Chapters, when set, drives the m4b chapter metadata. Each
	// entry pairs a chapter title with the index in VoiceSegments
	// where that chapter starts. The executor ffprobes durations
	// to compute chapter end times.
	Chapters []mixChapter `json:"chapters,omitempty"`
	// MusicCues / SFXCues / AmbientBeds are recorded by the
	// showrunner but ignored in v1. Present so the schema is stable
	// across the v1 → v2 transition.
	MusicCues   []mixCue `json:"music_cues,omitempty"`
	SFXCues     []mixCue `json:"sfx_cues,omitempty"`
	AmbientBeds []mixCue `json:"ambient_beds,omitempty"`
}

type mixChapter struct {
	Title        string `json:"title"`
	StartSegment int    `json:"start_segment"`
}

type mixCue struct {
	ID       string  `json:"id"`
	File     string  `json:"file,omitempty"`
	At       float64 `json:"at,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

func (m *mixExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	extra := map[string]any{
		"pipeline_status": in.PipelineStatus,
		"failure_summary": in.FailureSummary,
	}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}

	scriptPath, err := renderTemplate(st.ID+":script_file", st.ScriptFile, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("render script_file: %w", err)
	}
	scriptPath = resolveInRunDir(scriptPath, in.RunDir)
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("read script %s: %w", scriptPath, err)
	}
	var script mixScript
	if err := json.Unmarshal(scriptBytes, &script); err != nil {
		return nil, fmt.Errorf("parse script %s: %w", scriptPath, err)
	}
	if len(script.VoiceSegments) == 0 {
		return nil, fmt.Errorf("mix script %s: voice_segments is empty", scriptPath)
	}

	intro, err := renderOptionalPath(st.ID+":intro_music", st.IntroMusic, st, in, extra)
	if err != nil {
		return nil, err
	}
	outro, err := renderOptionalPath(st.ID+":outro_music", st.OutroMusic, st, in, extra)
	if err != nil {
		return nil, err
	}
	coverImage, err := renderOptionalPath(st.ID+":cover_image", st.CoverImage, st, in, extra)
	if err != nil {
		return nil, err
	}

	outputPath, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("render output: %w", err)
	}
	outputPath = resolveInRunDir(outputPath, in.RunDir)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir output: %w", err)
	}

	// Build the input file list. Intro first, then every voice
	// segment in order, then outro. Paths are resolved against the
	// run dir so the script can store run-relative paths.
	var inputs []string
	if intro != "" {
		inputs = append(inputs, intro)
	}
	for i, seg := range script.VoiceSegments {
		p := resolveInRunDir(seg, in.RunDir)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("voice segment %d (%s): %w", i, p, err)
		}
		inputs = append(inputs, p)
	}
	if outro != "" {
		inputs = append(inputs, outro)
	}

	binary := st.Binary
	if binary == "" {
		binary = "ffmpeg"
	}

	loudness := st.LoudnessTarget
	if loudness == 0 {
		loudness = -16 // podcast standard
	}

	// Build the ffmpeg invocation:
	//
	//   ffmpeg -y -i <input_0> -i <input_1> ... -i <input_N>
	//          [-i <cover>]
	//          -filter_complex "[0:a][1:a]...[N:a]concat=n=N:v=0:a=1[concat];
	//                           [concat]loudnorm=I=...:LRA=11:TP=-1.5[out]"
	//          -map "[out]" -map <cover>:v (if cover)
	//          -c:a aac -b:a 96k -c:v mjpeg -disposition:v:0 attached_pic
	//          <output>
	args := []string{"-y", "-hide_banner", "-loglevel", "warning"}
	for _, p := range inputs {
		args = append(args, "-i", p)
	}
	coverIdx := -1
	if coverImage != "" {
		coverIdx = len(inputs)
		args = append(args, "-i", coverImage)
	}

	// Filter graph: concat all N audio inputs, then loudnorm.
	var fg strings.Builder
	for i := range inputs {
		fmt.Fprintf(&fg, "[%d:a]", i)
	}
	fmt.Fprintf(&fg, "concat=n=%d:v=0:a=1[concat];", len(inputs))
	fmt.Fprintf(&fg, "[concat]loudnorm=I=%g:LRA=11:TP=-1.5[out]", loudness)

	args = append(args, "-filter_complex", fg.String())
	args = append(args, "-map", "[out]")

	// Codec choice + cover handling differ by container:
	//
	//   .m4b / .m4a → AAC + attached_pic + faststart (Apple
	//                  Books / Audiobookshelf treat this as a true
	//                  audiobook with chapters + embedded cover).
	//   .mp3        → libmp3lame; ffmpeg's mp3 muxer accepts only
	//                  a single audio stream, so we drop the
	//                  cover here (mp3 ID3 cover is doable but
	//                  needs a different filtergraph shape; v2).
	ext := strings.ToLower(filepath.Ext(outputPath))
	switch ext {
	case ".mp3":
		args = append(args, "-c:a", "libmp3lame", "-b:a", "96k")
	default: // .m4b / .m4a
		if coverIdx >= 0 {
			args = append(args,
				"-map", fmt.Sprintf("%d:v", coverIdx),
				"-c:v", "mjpeg",
				"-disposition:v:0", "attached_pic",
			)
		}
		args = append(args, "-c:a", "aac", "-b:a", "96k")
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, outputPath)

	cmd := exec.CommandContext(ctx, binary, args...)
	if in.Log != nil {
		cmd.Stdout = in.Log
		cmd.Stderr = in.Log
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg mix: %w", err)
	}
	// Verify the m4b/mp3 actually has content. ffmpeg occasionally
	// exits 0 with a 0-byte output (filtergraph errors that don't
	// propagate to exit status); without this check vamp would cache
	// the empty result and replay it forever.
	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("mix: stat output: %w", err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("mix: ffmpeg produced 0-byte output at %s (likely a filtergraph error swallowed by ffmpeg exit code)", outputPath)
	}
	return &StageOutput{Files: []string{outputPath}}, nil
}

// renderOptionalPath template-renders a possibly-empty path field
// and resolves it against the run dir. Empty templates pass through
// as "" so callers can omit the field entirely.
func renderOptionalPath(name, tmpl string, st *Stage, in StageInput, extra map[string]any) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	rendered, err := renderTemplate(name, tmpl, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return "", fmt.Errorf("render %s: %w", name, err)
	}
	rendered = strings.TrimSpace(rendered)
	if rendered == "" {
		return "", nil
	}
	return resolveInRunDir(rendered, in.RunDir), nil
}

// resolveInRunDir returns p verbatim if absolute, else joined to
// runDir. Centralised so every mix-stage path lookup behaves
// consistently.
func resolveInRunDir(p, runDir string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(runDir, p)
}
