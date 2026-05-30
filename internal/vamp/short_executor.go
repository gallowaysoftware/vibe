package vamp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// shortExecutor renders a structured-script JSON into a single vertical
// short-form video — the video analog of mixExecutor. Each shot pairs a video
// clip with a voiceover audio file (whose duration governs the shot) and an
// optional burned-in caption. The executor builds ONE ffmpeg invocation: per
// shot it fits the clip to the voiceover duration (freeze-last-frame via tpad
// when the clip is short, trim when long), scales/crops to a vertical target,
// burns the caption, then concatenates every shot (video + audio), applies
// loudnorm, and optionally ducks a background-music bed under the voice.
//
// v1 mixing is a static music gain (no sidechain ducking) — same v1/v2 split
// mixExecutor documents. The script schema is designed to grow without
// breaking v1.
type shortExecutor struct {
	// runner is the ffmpeg invoker; tests inject a recorder. nil → the
	// production os/exec runner (with an on-PATH preflight).
	runner ffmpegRunner
	// probe returns an audio file's duration in milliseconds; tests stub it.
	// nil → probeDurationMs (shells ffprobe).
	probe func(ctx context.Context, path string) (int64, error)
}

var _ StageExecutor = (*shortExecutor)(nil)

// shortScript is the on-disk schema short stages consume.
type shortScript struct {
	Shots []shortShot `json:"shots"`
	// Width/Height/FPS are the vertical target; stage fields override these,
	// and both fall back to 1080x1920@30.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	FPS    int `json:"fps,omitempty"`
	// BackgroundMusic is an optional global track mixed under the voiceover
	// at MusicGainDB (default -18 dB). Empty → no music.
	BackgroundMusic string  `json:"background_music,omitempty"`
	MusicGainDB     float64 `json:"music_gain_db,omitempty"`
}

type shortShot struct {
	Video   string `json:"video"`             // clip path (rel to run dir or absolute)
	Audio   string `json:"audio"`             // voiceover path; its duration is the shot length
	Caption string `json:"caption,omitempty"` // optional burned-in caption
}

func (s *shortExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, fmt.Errorf("short: missing stage")
	}
	extra := map[string]any{}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}

	scriptPath, err := renderTemplate(st.ID+":script_file", st.ScriptFile, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render script_file: %w", st.ID, err)
	}
	scriptPath = resolveInRunDir(scriptPath, in.RunDir)
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("stage %s: read script %s: %w", st.ID, scriptPath, err)
	}
	var script shortScript
	if err := json.Unmarshal(scriptBytes, &script); err != nil {
		return nil, fmt.Errorf("stage %s: parse script %s: %w", st.ID, scriptPath, err)
	}
	if len(script.Shots) == 0 {
		return nil, fmt.Errorf("stage %s: short script %s has no shots", st.ID, scriptPath)
	}

	// Dimensions: stage override wins, then script, then vertical default.
	w := firstNonZero(st.ShortWidth, script.Width)
	h := firstNonZero(st.ShortHeight, script.Height)
	fps := firstNonZero(st.ShortFPS, script.FPS)
	w, h, fps = normalizeVideoDims(w, h, fps)

	probe := s.probe
	if probe == nil {
		probe = probeDurationMs
	}

	outputPath, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output: %w", st.ID, err)
	}
	outputPath = resolveInRunDir(outputPath, in.RunDir)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: mkdir output: %w", st.ID, err)
	}

	fontFile := captionFontFile()

	// Inputs interleaved: shot i → input 2i (video), 2i+1 (audio). Background
	// music, when present, is the trailing input.
	var inputs []string
	var fg strings.Builder
	for i, shot := range script.Shots {
		if shot.Video == "" || shot.Audio == "" {
			return nil, fmt.Errorf("stage %s: shot %d needs both video and audio", st.ID, i)
		}
		vid := resolveInRunDir(shot.Video, in.RunDir)
		aud := resolveInRunDir(shot.Audio, in.RunDir)
		if _, err := os.Stat(vid); err != nil {
			return nil, fmt.Errorf("stage %s: shot %d video %s: %w", st.ID, i, vid, err)
		}
		if _, err := os.Stat(aud); err != nil {
			return nil, fmt.Errorf("stage %s: shot %d audio %s: %w", st.ID, i, aud, err)
		}
		durMs, err := probe(ctx, aud)
		if err != nil {
			return nil, fmt.Errorf("stage %s: probe shot %d audio %s: %w", st.ID, i, aud, err)
		}
		if durMs <= 0 {
			return nil, fmt.Errorf("stage %s: shot %d audio %s has zero duration", st.ID, i, aud)
		}
		secs := float64(durMs) / 1000.0
		vi := len(inputs) // video input index
		inputs = append(inputs, vid)
		ai := len(inputs) // audio input index
		inputs = append(inputs, aud)

		// Video: fill-crop to vertical, freeze-last-frame up to the shot
		// length, then trim to exactly the shot length. tpad's stop_duration
		// is the full shot length so it's always enough padding regardless of
		// the clip's native length; trim caps it either way.
		fmt.Fprintf(&fg, "[%d:v]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1,fps=%d,tpad=stop_mode=clone:stop_duration=%.3f,trim=duration=%.3f,setpts=PTS-STARTPTS",
			vi, w, h, w, h, fps, secs, secs)
		if capt := strings.TrimSpace(shot.Caption); capt != "" {
			// Write the (word-wrapped) caption to a file and reference it via
			// drawtext's textfile= option. Inline text=... can't safely carry
			// apostrophes/colons inside a single-quoted filtergraph value;
			// textfile sidesteps all escaping.
			capPath := filepath.Join(in.RunDir, fmt.Sprintf(".caption-%d.txt", i))
			if err := os.WriteFile(capPath, []byte(wrapText(capt, captionMaxChars(w))), 0o644); err != nil {
				return nil, fmt.Errorf("stage %s: write caption %d: %w", st.ID, i, err)
			}
			fmt.Fprintf(&fg, ",%s", drawtextFilter(capPath, w, fontFile))
		}
		fmt.Fprintf(&fg, "[v%d];", i)
		// Audio: pad (in case the voiceover is a hair short) then trim to the
		// shot length so video and audio stay in lockstep across the concat.
		fmt.Fprintf(&fg, "[%d:a]apad,atrim=duration=%.3f,asetpts=PTS-STARTPTS[a%d];", ai, secs, i)
	}

	n := len(script.Shots)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&fg, "[v%d][a%d]", i, i)
	}
	fmt.Fprintf(&fg, "concat=n=%d:v=1:a=1[cv][ca];", n)

	loud := st.LoudnessTarget
	if loud == 0 {
		loud = -16
	}
	fmt.Fprintf(&fg, "[ca]loudnorm=I=%g:LRA=11:TP=-1.5[na]", loud)

	finalAudio := "[na]"
	music, err := renderOptionalPath(st.ID+":background_music", script.BackgroundMusic, st, in, extra)
	if err != nil {
		return nil, err
	}
	if music != "" {
		if _, err := os.Stat(music); err != nil {
			return nil, fmt.Errorf("stage %s: background_music %s: %w", st.ID, music, err)
		}
		gain := script.MusicGainDB
		if gain == 0 {
			gain = -18
		}
		mi := len(inputs)
		inputs = append(inputs, music)
		// Loop the bed, drop its gain, then mix UNDER the voice. duration=first
		// clamps the result to the voice length; normalize=0 keeps the voice at
		// full level (amix otherwise auto-attenuates every input).
		fmt.Fprintf(&fg, ";[%d:a]volume=%gdB,aloop=loop=-1:size=2147483647[bg];[na][bg]amix=inputs=2:duration=first:normalize=0[outa]", mi, gain)
		finalAudio = "[outa]"
	}

	binary := st.Binary
	if binary == "" {
		binary = "ffmpeg"
	}
	runner := s.runner
	if runner == nil {
		if err := ensureFFmpegOnPath(st.ID, binary); err != nil {
			return nil, err
		}
		runner = ffmpegCommandRunner{}
	}

	args := []string{"-hide_banner", "-loglevel", "warning"}
	for _, p := range inputs {
		args = append(args, "-i", p)
	}
	args = append(args, "-filter_complex", fg.String(),
		"-map", "[cv]", "-map", finalAudio,
		"-r", fmt.Sprintf("%d", fps),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "192k",
		"-movflags", "+faststart")
	// Container metadata, alphabetical for deterministic argv (mirrors mix).
	if len(st.Metadata) > 0 {
		keys := make([]string, 0, len(st.Metadata))
		for k := range st.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			rendered, err := renderTemplate(st.ID+":metadata:"+k, st.Metadata[k], st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
			if err != nil {
				return nil, fmt.Errorf("stage %s: render metadata %s: %w", st.ID, k, err)
			}
			args = append(args, "-metadata", fmt.Sprintf("%s=%s", k, rendered))
		}
	}
	args = append(args, "-y", outputPath)

	if in.Log != nil {
		fmt.Fprintf(in.Log, "short: %s (%d shot(s), %dx%d@%d)\n", filepath.Base(outputPath), n, w, h, fps)
	}
	if err := runner.Run(ctx, binary, args, in.Log); err != nil {
		return nil, fmt.Errorf("stage %s: ffmpeg short: %w", st.ID, err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("stage %s: stat output: %w", st.ID, err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("stage %s: ffmpeg produced 0-byte output at %s", st.ID, outputPath)
	}
	return &StageOutput{Files: []string{outputPath}}, nil
}

// firstNonZero returns the first non-zero of a, b (0 if both zero).
func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// captionFontSize / captionMaxChars derive the caption type size and the
// word-wrap width from the frame width, kept in one place so the filter and
// the wrap agree.
func captionFontSize(width int) int {
	fs := width / 26
	if fs < 16 {
		fs = 16
	}
	return fs
}

func captionMaxChars(width int) int {
	// Conservative chars-per-line for a bold face: ~0.6em average advance,
	// kept to ~85% of the frame width so the box has margins.
	mc := int(float64(width) * 0.85 / (float64(captionFontSize(width)) * 0.6))
	if mc < 12 {
		mc = 12
	}
	return mc
}

// drawtextFilter builds a centered, bottom-third caption drawtext filter that
// reads the (already word-wrapped) caption from captionFile via textfile=, with
// a semi-opaque box for legibility. textfile + expansion=none means the caption
// text needs no escaping at all — apostrophes, colons, percent signs all pass
// through literally. fontFile may be empty (fontconfig default sans).
func drawtextFilter(captionFile string, width int, fontFile string) string {
	var b strings.Builder
	b.WriteString("drawtext=")
	if fontFile != "" {
		fmt.Fprintf(&b, "fontfile='%s':", ffmpegPathEscape(fontFile))
	} else {
		b.WriteString("font='Sans':")
	}
	fmt.Fprintf(&b, "textfile='%s':expansion=none:", ffmpegPathEscape(captionFile))
	fmt.Fprintf(&b, "fontcolor=white:fontsize=%d:borderw=6:bordercolor=black@0.85:", captionFontSize(width))
	b.WriteString("box=1:boxcolor=black@0.5:boxborderw=18:")
	b.WriteString("x=(w-text_w)/2:y=h*0.70:line_spacing=12")
	return b.String()
}

// wrapText greedily wraps s on spaces so no line exceeds maxChars (a single
// word longer than maxChars is left intact rather than split). Lines are
// joined with newlines.
func wrapText(s string, maxChars int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > maxChars {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	lines = append(lines, cur)
	return strings.Join(lines, "\n")
}

// ffmpegPathEscape escapes a filesystem path for use inside a single-quoted
// drawtext option value (fontfile= / textfile=). Our paths live under the run
// dir and contain no quotes; we escape backslashes and colons defensively.
func ffmpegPathEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	return s
}

// captionFontFile returns the first common sans-serif TTF present on the host,
// or "" to fall back to fontconfig's default. We resolve a concrete file so
// drawtext doesn't depend on fontconfig being configured.
func captionFontFile() string {
	for _, p := range []string{
		"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/TTF/DejaVuSans.ttf",
		"/usr/share/fonts/liberation/LiberationSans-Bold.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
		"/System/Library/Fonts/Supplemental/Arial Bold.ttf",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
