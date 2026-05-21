package vamp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
)

// visionExecutor handles text stages with image_dir: reads the prompt, scans
// the image directory for supported image files, and calls the multimodal
// inference endpoint. Falls through to the standard text executor when
// image_dir is empty.
type visionExecutor struct {
	inference  InferenceFunc
	multimodal func(ctx context.Context, baseURL, model, prompt string, images []string, params map[string]any, onToken StreamFunc) (string, error)
}

// Compile-time guarantee that visionExecutor satisfies StageExecutor.
var _ StageExecutor = (*visionExecutor)(nil)

// Execute processes a vision-capable text stage. If ImageDir is set, all
// supported images are collected and sent as multimodal content alongside the
// prompt. Otherwise it delegates to the text executor path.
func (v *visionExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	extra := map[string]any{
		"pipeline_status": in.PipelineStatus,
		"failure_summary": in.FailureSummary,
	}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}
	prompt, err := renderPrompt(st, in.PipelineDir, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("render prompt: %w", err)
	}

	var onToken StreamFunc
	if in.Log != nil {
		onToken = func(delta string) {
			_, _ = in.Log.Write([]byte(delta))
		}
	}

	// Collect images. Two modes:
	//   - image_dir: scan a directory for raster/SVG files.
	//   - image_files: explicit templated list (one rendered path per
	//     entry). Used for per-iteration single-image stages where
	//     directory globbing is too coarse.
	// Mutually exclusive at validate time.
	var imagePaths []string
	if st.ImageDir != "" {
		resolvedDir, renderErr := renderTemplate(st.ID+":image_dir", st.ImageDir, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if renderErr != nil {
			return nil, fmt.Errorf("render image_dir: %w", renderErr)
		}
		imgDir := expandTilde(resolvedDir)
		if !filepath.IsAbs(imgDir) {
			imgDir = filepath.Join(in.PipelineDir, imgDir)
		}
		imagePaths, err = collectImages(imgDir)
		if err != nil {
			return nil, fmt.Errorf("collect images from %s: %w", resolvedDir, err)
		}
	} else if len(st.ImageFiles) > 0 {
		for idx, tmpl := range st.ImageFiles {
			resolved, renderErr := renderTemplate(fmt.Sprintf("%s:image_files[%d]", st.ID, idx), tmpl, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
			if renderErr != nil {
				return nil, fmt.Errorf("render image_files[%d]: %w", idx, renderErr)
			}
			p := expandTilde(strings.TrimSpace(resolved))
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(in.PipelineDir, p)
			}
			// SVGs get the same rsvg-convert path as collectImages does.
			if strings.EqualFold(filepath.Ext(p), ".svg") {
				png, rasterErr := rasterizeSVG(p)
				if rasterErr != nil {
					return nil, fmt.Errorf("rasterize %s: %w", p, rasterErr)
				}
				p = png
			}
			imagePaths = append(imagePaths, p)
		}
	}

	var out string
	if len(imagePaths) > 0 {
		out, err = v.inferenceMultimodal(ctx, in.BaseURL+"/v1", in.ModelID, prompt, imagePaths, st.Params, onToken)
	} else {
		out, err = v.inference(ctx, in.BaseURL+"/v1", in.ModelID, prompt, st.Params, onToken)
	}
	if err != nil {
		return nil, fmt.Errorf("inference: %w", err)
	}
	if in.Log != nil {
		_, _ = in.Log.Write([]byte("\n"))
	}

	if st.OutputFormat == "json" {
		if err := validateJSON(out); err != nil {
			return nil, fmt.Errorf("stage output is not valid JSON: %w", err)
		}
	}
	return &StageOutput{Text: out}, nil
}

// inferenceMultimodal calls the multimodal inference path when available,
// otherwise falls back to text-only (images are silently ignored).
func (v *visionExecutor) inferenceMultimodal(ctx context.Context, baseURL, model, prompt string, images []string, params map[string]any, onToken StreamFunc) (string, error) {
	if v.multimodal != nil {
		return v.multimodal(ctx, baseURL, model, prompt, images, params, onToken)
	}
	return v.inference(ctx, baseURL, model, prompt, params, onToken)
}

// expandTilde expands a leading "~/" against the user's home directory.
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// collectImages reads all supported image files from dir, sorted by name.
// SVGs are rasterized to PNG via rsvg-convert into a content-addressed cache
// under $XDG_CACHE_HOME/vamp/svg-rasterized/ so vision models (Gemma 3,
// Qwen-VL, etc.) — which see only bitmap pixel data, not vector markup —
// can read diagrams that were authored as SVG.
//
// A missing dir is treated as "no images here" (returns nil, nil) rather than
// an error. Foreach pipelines routinely point image_dir at a per-item path
// that legitimately doesn't exist on every item (e.g. SAQ-only lessons in
// the cibd curriculum carry no images/ subdir); failing the stage on the
// missing-dir case would refuse to do text-only inference on those items,
// which is what the vision-capable backend cleanly degrades to.
func collectImages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	raster := map[string]bool{
		".png":  true,
		".jpg":  true,
		".jpeg": true,
		".gif":  true,
		".webp": true,
		".bmp":  true,
	}
	var out []string
	var svgs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		p := filepath.Join(dir, e.Name())
		switch {
		case ext == ".svg":
			svgs = append(svgs, p)
		case raster[ext]:
			out = append(out, p)
		}
	}
	for _, svg := range svgs {
		png, err := rasterizeSVG(svg)
		if err != nil {
			return nil, fmt.Errorf("rasterize %s: %w", svg, err)
		}
		out = append(out, png)
	}
	slices.Sort(out)
	return out, nil
}

// svgRasterMaxDim is the bounding-box dimension passed to rsvg-convert.
// Gemma 3's vision encoder normalizes inputs to a single 896x896 tile;
// images that exceed that in either dimension are split into multiple
// pan-and-scan tiles, multiplying the per-image token cost (256 tokens
// per tile). We fit-within 896x896 so every rasterized SVG lands as
// exactly one tile, keeping the per-image cost predictable. Aspect
// ratio is preserved, so the actual PNG is at most 896×896 with the
// other dimension scaled down.
const svgRasterMaxDim = 896

// rasterizeSVG runs rsvg-convert on svgPath and returns the cached PNG path.
// Cache keys are sha256(svg bytes) so identical SVGs across lessons (common
// for boilerplate diagrams) share one render. Errors propagate — a missing
// rsvg-convert or a malformed SVG should fail the stage rather than silently
// drop the image the lesson author put there for the model to read.
func rasterizeSVG(svgPath string) (string, error) {
	data, err := os.ReadFile(svgPath)
	if err != nil {
		return "", fmt.Errorf("read svg: %w", err)
	}
	sum := sha256.Sum256(data)
	cacheDir := filepath.Join(cache.DefaultRoot(), "svg-rasterized")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}
	pngPath := filepath.Join(cacheDir, hex.EncodeToString(sum[:])+".png")
	if info, err := os.Stat(pngPath); err == nil && info.Size() > 0 {
		return pngPath, nil
	}
	dim := fmt.Sprintf("%d", svgRasterMaxDim)
	cmd := exec.Command("rsvg-convert", "--width", dim, "--height", dim, "--keep-aspect-ratio", "-o", pngPath, svgPath)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		_ = os.Remove(pngPath)
		return "", fmt.Errorf("rsvg-convert: %w: %s", runErr, strings.TrimSpace(string(out)))
	}
	return pngPath, nil
}

// renderExecutor renders a template (prompt or prompt_file) against the
// current stage binding and returns the rendered text. It does not invoke
// an LLM, so it skips profile activation. Useful for deterministic data
// transformation stages: enumerating directories, building JSON arrays,
// concatenating intermediate outputs, etc.
type renderExecutor struct{}

// Compile-time guarantee that renderExecutor satisfies StageExecutor.
var _ StageExecutor = (*renderExecutor)(nil)

func (r *renderExecutor) Execute(_ context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, fmt.Errorf("render: missing stage")
	}

	extra := map[string]any{
		"pipeline_status": in.PipelineStatus,
		"failure_summary": in.FailureSummary,
	}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}

	text, err := renderPrompt(st, in.PipelineDir, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("render prompt: %w", err)
	}
	return &StageOutput{Text: text}, nil
}
