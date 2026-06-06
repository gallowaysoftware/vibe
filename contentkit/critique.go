package contentkit

import (
	"io/fs"

	"github.com/gallowaysoftware/vibe/vamp"
)

// CritiqueReviseConfig parameterises CritiqueRevise. Prompts are app-specific
// (each app audits different axes), so the caller supplies its own PromptFS and
// prompt names; contentkit owns only the two-stage wiring.
type CritiqueReviseConfig struct {
	PromptFS fs.FS

	// Critique: the cool, analytical diagnosis. Names problems; does not rewrite.
	CritiqueName   string
	CritiquePrompt string  // name within PromptFS
	CritiqueTemp   float64 // default 0.2
	CritiqueTokens int     // default 8192
	CritiqueJSON   bool    // OutputFormatJSON
	CritiqueOut    string  // default CritiqueName + ".md"
	// CritiqueDeps are the stages the critique reads (the content + any context,
	// e.g. an outline). Their outputs must be referenced in the prompt by name.
	CritiqueDeps []vamp.Ref

	// Revise: the surgical fix. Resolves the critique's notes, nothing else.
	ReviseName   string
	RevisePrompt string
	ReviseTemp   float64 // default 0.5
	ReviseTokens int     // default 12288
	ReviseJSON   bool
	ReviseOut    string // default ReviseName + ".md"
	// ReviseDeps are what the revise reads BESIDES the critique (typically the
	// content stage). The critique stage is appended automatically.
	ReviseDeps []vamp.Ref
}

// CritiqueRevise wires the diagnose-then-fix cycle: a low-temperature stage that
// NAMES a piece of content's problems, feeding a surgical rewrite that resolves
// exactly those notes. This is the pattern a fiction pipeline (prose_critique→edit_story,
// expand_critique→expand_revise, continuity_precheck→edit) and a short-video
// pipeline (critique→punchup, recheck→polish) each hand-wired. Returns both stages so the
// caller can wire further downstream of the reviser.
func CritiqueRevise(p *vamp.Pipeline, cfg CritiqueReviseConfig) (critique, revise *vamp.TextStage) {
	if cfg.CritiqueTemp == 0 {
		cfg.CritiqueTemp = 0.2
	}
	if cfg.CritiqueTokens == 0 {
		cfg.CritiqueTokens = 8192
	}
	if cfg.CritiqueOut == "" {
		cfg.CritiqueOut = cfg.CritiqueName + ".md"
	}
	if cfg.ReviseTemp == 0 {
		cfg.ReviseTemp = 0.5
	}
	if cfg.ReviseTokens == 0 {
		cfg.ReviseTokens = 12288
	}
	if cfg.ReviseOut == "" {
		cfg.ReviseOut = cfg.ReviseName + ".md"
	}

	critique = LongFormText(p, cfg.CritiqueName, cfg.CritiqueTemp, cfg.CritiqueTokens).
		PromptFS(cfg.PromptFS, cfg.CritiquePrompt)
	if len(cfg.CritiqueDeps) > 0 {
		critique = critique.After(cfg.CritiqueDeps...)
	}
	if cfg.CritiqueJSON {
		critique = critique.OutputFormatJSON()
	}
	critique = critique.Output(cfg.CritiqueOut)

	revise = LongFormText(p, cfg.ReviseName, cfg.ReviseTemp, cfg.ReviseTokens).
		PromptFS(cfg.PromptFS, cfg.RevisePrompt).
		After(append(append([]vamp.Ref{}, cfg.ReviseDeps...), critique)...)
	if cfg.ReviseJSON {
		revise = revise.OutputFormatJSON()
	}
	revise = revise.Output(cfg.ReviseOut)

	return critique, revise
}
