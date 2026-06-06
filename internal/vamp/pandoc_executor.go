package vamp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// pandocExecutor implements StageExecutor for type: pandoc stages. It
// converts a source document (markdown, HTML, etc.) into a target format
// (EPUB, PDF, etc.) by shelling out to pandoc. When the configured binary
// isn't on $PATH the executor falls back to a docker invocation against
// the `pandoc/core` image so a host without a pandoc install still works.
//
// Stage fields consumed:
//   - SourceFile (required) — template rendering the input path
//   - PandocFrom / PandocTo (required) — --from / --to format flags
//   - PandocMetadata — map of --metadata key=value
//   - PandocArgs — appended raw args (e.g. ["--toc", "--toc-depth=3"])
//   - CoverImage — when set on an EPUB output, becomes
//     --epub-cover-image
//   - Binary — overrides "pandoc"; "docker" triggers the docker fallback
//     shape explicitly
type pandocExecutor struct{}

var _ StageExecutor = (*pandocExecutor)(nil)

// pandocDockerImage is the upstream-maintained image used when pandoc
// isn't installed locally. ~200 MB on first pull, then cached.
const pandocDockerImage = "pandoc/core:latest"

func (p *pandocExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("pandoc: missing stage")
	}
	if st.SourceFile == "" {
		return nil, fmt.Errorf("stage %s: source_file is required for type: pandoc", st.ID)
	}
	extra := map[string]any{}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}
	srcRel, err := renderTemplate(st.ID+":source_file", st.SourceFile, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render source_file: %w", st.ID, err)
	}
	srcAbs := srcRel
	if !filepath.IsAbs(srcAbs) {
		srcAbs = filepath.Join(in.RunDir, srcRel)
	}
	if _, err := os.Stat(srcAbs); err != nil {
		return nil, fmt.Errorf("stage %s: source_file %s: %w", st.ID, srcAbs, err)
	}

	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output: %w", st.ID, err)
	}
	outAbs := filepath.Join(in.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: create output dir: %w", st.ID, err)
	}

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

	useDocker := false
	binary := st.Binary
	switch binary {
	case "":
		// Prefer a host pandoc when available so we avoid the docker
		// pull on first use; fall back to docker so the stage works on
		// hosts without pandoc installed.
		if _, err := exec.LookPath("pandoc"); err == nil {
			binary = "pandoc"
		} else {
			useDocker = true
			binary = "docker"
		}
	case "docker":
		useDocker = true
	}

	args := buildPandocArgs(st, srcAbs, outAbs, coverAbs, useDocker, in.RunDir)
	if in.Log != nil {
		fmt.Fprintf(in.Log, "pandoc: %s -> %s (engine=%s)\n", filepath.Base(srcAbs), outRel, binary)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = in.Log
	cmd.Stderr = in.Log
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("stage %s: pandoc: %w", st.ID, err)
	}
	if _, err := os.Stat(outAbs); err != nil {
		return nil, fmt.Errorf("stage %s: pandoc produced no output at %s: %w", st.ID, outAbs, err)
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// buildPandocArgs assembles the argv for the pandoc invocation. The host
// vs docker shapes differ only at the head: docker prepends the
// `run --rm -v RUNDIR:/data -w /data` flags and rewrites paths to be
// relative to /data. The remaining pandoc flags are identical.
func buildPandocArgs(st *Stage, srcAbs, outAbs, coverAbs string, useDocker bool, runDir string) []string {
	src, out, cover := srcAbs, outAbs, coverAbs
	pre := []string{}
	if useDocker {
		// Bind the run dir at /data so every path in the rest of the
		// argv resolves inside the container without smuggling
		// host-specific absolute paths in.
		pre = append(pre,
			"run", "--rm",
			"-v", runDir+":/data",
			"-w", "/data",
			pandocDockerImage,
		)
		src = rebasePath(src, runDir, "/data")
		out = rebasePath(out, runDir, "/data")
		if cover != "" {
			cover = rebasePath(cover, runDir, "/data")
		}
	}
	args := append([]string(nil), pre...)
	if st.PandocFrom != "" {
		args = append(args, "--from", st.PandocFrom)
	}
	if st.PandocTo != "" {
		args = append(args, "--to", st.PandocTo)
	}
	// Deterministic metadata-key order so the cache key (rendered argv)
	// doesn't oscillate run-to-run on Go map iteration.
	if len(st.PandocMetadata) > 0 {
		keys := make([]string, 0, len(st.PandocMetadata))
		for k := range st.PandocMetadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--metadata", k+"="+st.PandocMetadata[k])
		}
	}
	if cover != "" {
		args = append(args, "--epub-cover-image", cover)
	}
	args = append(args, st.PandocArgs...)
	args = append(args, "-o", out, src)
	return args
}

// rebasePath rewrites abs from a host prefix to a container prefix,
// preserving the trailing relative segment. Used only inside the docker
// fallback to convert host paths into container-internal /data paths.
// Inputs already live under runDir by construction (executor ensures
// it), so a path that doesn't match is a programmer error and we panic
// at runtime via an obvious empty-string result — caller validates the
// stat above.
func rebasePath(abs, hostPrefix, containerPrefix string) string {
	rel, err := filepath.Rel(hostPrefix, abs)
	if err != nil {
		return abs
	}
	return filepath.Join(containerPrefix, rel)
}

// pandocOutputExtension reports the file extension the pandoc stage
// produces — derived from PandocTo for the validator. Empty when no
// reliable mapping exists.
func pandocOutputExtension(to string) string {
	switch strings.ToLower(to) {
	case "epub", "epub3":
		return ".epub"
	case "pdf":
		return ".pdf"
	case "html", "html5":
		return ".html"
	case "docx":
		return ".docx"
	}
	return ""
}
