package vamp

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// readStageAsset reads a pipeline-shipped asset referenced from a stage
// field (PromptFile / BodyTemplateFile / Workflow / similar).
//
// Resolution order:
//
//  1. If the stage has a non-nil AssetFS, the path is read from it. Inside
//     an fs.FS, paths are slash-rooted and never absolute, so any leading
//     slash is stripped (fs.ValidPath rejects "abs" paths).
//  2. Otherwise, if the path is absolute on disk, it's read as-is.
//  3. Otherwise, the path is joined with pipelineDir and read from disk.
//
// The two-tier resolution lets a single pipeline mix asset sources: a
// Go-DSL pipeline can ship most prompts via embed.FS while still pulling a
// large lesson corpus from an absolute on-disk path the user supplied via
// --input. YAML pipelines take the disk paths exclusively, matching the
// pre-fs.FS behaviour.
func readStageAsset(st *Stage, pipelineDir, path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("asset path is empty")
	}
	if st != nil && st.AssetFS != nil {
		// fs.FS requires forward-slash paths and forbids leading slashes;
		// be tolerant of users who templated a leading "/" because the
		// embed.FS root they had in mind starts with one.
		fsPath := path
		for len(fsPath) > 0 && fsPath[0] == '/' {
			fsPath = fsPath[1:]
		}
		data, err := fs.ReadFile(st.AssetFS, fsPath)
		if err != nil {
			return nil, fmt.Errorf("read asset %s from fs.FS: %w", path, err)
		}
		return data, nil
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(pipelineDir, resolved)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	return data, nil
}
