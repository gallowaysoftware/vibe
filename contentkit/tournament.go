package contentkit

import (
	"fmt"
	"io/fs"

	"github.com/gallowaysoftware/vibe/vamp"
)

// TournamentConfig parameterises Tournament.
type TournamentConfig struct {
	PromptFS fs.FS

	// Drafts: N independent attempts at the same prompt, at varied temperatures
	// (more shots on goal). Stage ids are DraftBaseName + "_a", "_b", ...
	DraftBaseName string
	DraftPrompt   string
	DraftTemps    []float64 // one entry per candidate; varied for diversity
	DraftTokens   int       // default 12288
	DraftJSON     bool
	// DraftDeps are shared by every draft (e.g. the source the drafts work from).
	DraftDeps []vamp.Ref

	// Judge: picks the strongest draft and may graft the best beats from the rest.
	JudgeName   string
	JudgePrompt string
	JudgeTemp   float64 // default 0.6
	JudgeTokens int     // default 12288
	JudgeJSON   bool
	JudgeOut    string // default JudgeName + ".md"
}

// Tournament wires generate-N-pick-best: several independent drafts at varied
// temperatures, then a judge that selects (and optionally merges) the funniest /
// strongest. Comparison is more reliable than single-shot generation for
// subjective quality. This is brainrot's draft_a/b/c→bakeoff, generalised.
// Returns the judge stage (the winner) and the draft stages.
func Tournament(p *vamp.Pipeline, cfg TournamentConfig) (judge *vamp.TextStage, drafts []*vamp.TextStage) {
	if cfg.DraftTokens == 0 {
		cfg.DraftTokens = 12288
	}
	if cfg.JudgeTemp == 0 {
		cfg.JudgeTemp = 0.6
	}
	if cfg.JudgeTokens == 0 {
		cfg.JudgeTokens = 12288
	}
	if cfg.JudgeOut == "" {
		cfg.JudgeOut = cfg.JudgeName + ".md"
	}

	for i, temp := range cfg.DraftTemps {
		name := fmt.Sprintf("%s_%c", cfg.DraftBaseName, 'a'+i)
		d := LongFormText(p, name, temp, cfg.DraftTokens).
			PromptFS(cfg.PromptFS, cfg.DraftPrompt)
		if len(cfg.DraftDeps) > 0 {
			d = d.After(cfg.DraftDeps...)
		}
		if cfg.DraftJSON {
			d = d.OutputFormatJSON()
		}
		d = d.Output(name + ".json")
		drafts = append(drafts, d)
	}

	judge = LongFormText(p, cfg.JudgeName, cfg.JudgeTemp, cfg.JudgeTokens).
		PromptFS(cfg.PromptFS, cfg.JudgePrompt)
	deps := make([]vamp.Ref, len(drafts))
	for i, d := range drafts {
		deps[i] = d
	}
	judge = judge.After(deps...)
	if cfg.JudgeJSON {
		judge = judge.OutputFormatJSON()
	}
	judge = judge.Output(cfg.JudgeOut)
	return judge, drafts
}
