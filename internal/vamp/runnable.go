package vamp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

// CheckRunnable validates the cross-cutting prerequisites that Pipeline.Validate
// can't see (because they need either the pipeline's source directory or the
// active capabilities config to evaluate):
//
//   - every stage that requires a capability has one mapped in caps
//   - every prompt_file resolves to an existing, readable file
//   - every prompt_file's contents parse as a Go text/template
//
// Inline `prompt` strings are already try-parsed by Pipeline.Validate, but we
// re-parse them here too so the same error path covers both forms — callers
// that built a Pipeline programmatically (without going through LoadPipeline)
// still get a clean diagnostic.
//
// The function returns a single errors.Join of every problem it finds so a
// user fixing a multi-stage pipeline sees the full punch list rather than one
// error per `vamp validate` re-run. nil caps disables the capability check;
// pass it when the caller is checking a pipeline in a context where
// capabilities aren't yet loaded (e.g. lint).
func CheckRunnable(p *Pipeline, pipelineDir string, caps *Capabilities) error {
	if p == nil {
		return errors.New("nil pipeline")
	}
	var errs []error
	for i := range p.Stages {
		s := &p.Stages[i]
		ctx := fmt.Sprintf("stage %s", s.ID)
		if caps != nil && stageNeedsCapability(s) {
			if _, err := caps.Profiles(s.Capability); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", ctx, err))
			}
		}
		if s.PromptFile != "" {
			path := s.PromptFile
			if !filepath.IsAbs(path) {
				path = filepath.Join(pipelineDir, path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: prompt_file %q: %w", ctx, s.PromptFile, err))
				continue
			}
			if _, err := template.New(s.ID + ":prompt_file").Funcs(templateFuncs()).Parse(string(data)); err != nil {
				errs = append(errs, fmt.Errorf("%s: parse prompt_file %q: %w", ctx, s.PromptFile, err))
			}
		}
		if s.Prompt != "" {
			if _, err := template.New(s.ID + ":prompt").Funcs(templateFuncs()).Parse(s.Prompt); err != nil {
				errs = append(errs, fmt.Errorf("%s: parse prompt template: %w", ctx, err))
			}
		}
	}
	return errors.Join(errs...)
}

// stageNeedsCapability mirrors the capability-required rule in Pipeline.Validate
// so the runnable check stays in lock-step with the shape check. Stage types
// that talk to a third-party API (youtube/webhook) or run a local subprocess
// (audio/ffmpeg) or do pure local I/O (confirm/render) never activate a vibe
// profile, so they don't need a capability mapping even if one is set.
func stageNeedsCapability(s *Stage) bool {
	t := s.Type
	if t == "" {
		t = StageTypeText
	}
	switch t {
	case StageTypeAudio, StageTypeFFmpeg, StageTypeYouTube,
		StageTypeWebhook, StageTypeConfirm, StageTypeRender:
		return false
	}
	return s.Capability != ""
}

// SuggestCapability returns up to three known capability names from caps that
// are closest to the misspelled `want` by edit distance, sorted by distance
// then alphabetically. Returns nil when caps is empty or no candidate scores
// below the threshold. Intended for "did you mean: ..." hints in error
// messages; keeps the suggestion list short so the hint stays scannable.
func SuggestCapability(caps *Capabilities, want string) []string {
	if caps == nil || len(caps.Mapping) == 0 {
		return nil
	}
	type scored struct {
		name string
		d    int
	}
	const threshold = 3
	var hits []scored
	for k := range caps.Mapping {
		d := editDistance(strings.ToLower(want), strings.ToLower(k))
		if d <= threshold {
			hits = append(hits, scored{k, d})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].d != hits[j].d {
			return hits[i].d < hits[j].d
		}
		return hits[i].name < hits[j].name
	})
	if len(hits) > 3 {
		hits = hits[:3]
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.name)
	}
	return out
}

// editDistance returns the Levenshtein distance between a and b. Used to
// power capability-suggestion hints; we keep it local rather than pulling in
// a fuzzy-match library because the call site is one-shot at validation time
// and the matrix is tiny (capability names are short).
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
