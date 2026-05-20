package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// renderCmd implements `vamp render <pipeline.yaml> <stage_id>`: render a
// single stage's prompt template against the pipeline's inputs and any
// supplied prior-stage outputs, and print the result. The point is fast
// prompt iteration: edit the prompt, render, eyeball the rendered text, no
// LLM call, no vibe contact, no run dir written.
//
// Inputs are supplied via repeated `--input KEY=VALUE` (same shape as
// `vamp run`). Prior-stage outputs for the target stage's declared
// dependencies can be sourced from an existing run dir via `--run-dir`;
// when set, the command reads each dependency's `output:` path from disk
// and exposes it under `.stages.<id>.output` exactly as the runtime would.
// When `--run-dir` is unset every declared dependency is bound to a
// placeholder string of the form `<stage>-output-placeholder` so a fresh
// prompt-iteration loop (no prior outputs yet) still renders cleanly.
//
// Output goes to stdout verbatim, with no trailing newline beyond whatever
// the template itself produced. That keeps the command's output suitable
// for piping into a paginator / pbcopy without surprising formatting.
func renderCmd() *cobra.Command {
	var (
		inputFlags []string
		runDirFlag string
	)
	cmd := &cobra.Command{
		Use:               "render <pipeline.yaml> <stage_id>",
		Short:             "Render a single stage's prompt template against inputs and prior outputs (no LLM call).",
		Long:              renderCmdLongDescription,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completePipelineFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			pipelinePath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			stageID := args[1]
			p, err := vamp.LoadPipeline(pipelinePath)
			if err != nil {
				return err
			}
			// Find the requested stage. Validation has already ensured ids
			// are unique, so a linear scan is fine and the error message
			// can enumerate the valid ids for users who typo'd.
			var stage *vamp.Stage
			for i := range p.Stages {
				if p.Stages[i].ID == stageID {
					stage = &p.Stages[i]
					break
				}
			}
			if stage == nil {
				ids := make([]string, 0, len(p.Stages))
				for i := range p.Stages {
					ids = append(ids, p.Stages[i].ID)
				}
				return fmt.Errorf("stage %q not found in %s (have: %s)", stageID, pipelinePath, strings.Join(ids, ", "))
			}
			inputs, err := parseInputs(inputFlags, p)
			if err != nil {
				return err
			}
			absRunDir := ""
			if runDirFlag != "" {
				absRunDir, err = filepath.Abs(runDirFlag)
				if err != nil {
					return err
				}
				if info, err := os.Stat(absRunDir); err != nil {
					return fmt.Errorf("run-dir %s: %w", absRunDir, err)
				} else if !info.IsDir() {
					return fmt.Errorf("run-dir %s: not a directory", absRunDir)
				}
			}
			// Build the prior-stage output map the prompt template will see.
			// Two paths:
			//   - --run-dir set: load each declared dep's output from disk
			//     by rendering its own Output template against the same
			//     inputs (no foreach support; render's whole point is
			//     non-foreach prompt iteration).
			//   - --run-dir unset: synthesise placeholder strings so an
			//     iteration loop with no prior outputs still produces a
			//     fully-rendered prompt the user can eyeball.
			prior, err := buildRenderPrior(p, stage, inputs, absRunDir)
			if err != nil {
				return err
			}
			pipelineDir := filepath.Dir(pipelinePath)
			rendered, err := vamp.RenderStagePrompt(stage, pipelineDir, inputs, prior, absRunDir)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), rendered)
			return err
		},
	}
	cmd.Flags().StringArrayVar(&inputFlags, "input", nil, "Pipeline input as KEY=VALUE; can repeat. Same shape as `vamp run --input`.")
	cmd.Flags().StringVar(&runDirFlag, "run-dir", "", "Optional: prior run directory whose stage outputs (`.stages.<id>.output`) should seed the template. Without this, prior outputs render as `<stage>-output-placeholder` strings.")
	return cmd
}

const renderCmdLongDescription = `Render a single stage's prompt template against the pipeline's inputs and
any available prior-stage outputs, then print the result and exit. No LLM
call, no vibe contact, no run dir created.

The intended workflow is prompt iteration: edit prompts/<file>.md (or the
inline prompt: field), run vamp render <pipeline> <stage>, eyeball the
rendered text, repeat. The render is byte-identical to what the same
stage would produce at the start of a real run, modulo foreach injection
(render does not iterate; it renders the prompt as-is).

Use --input KEY=VALUE (repeatable) to supply pipeline inputs, exactly as
with vamp run. Use --run-dir <dir> to source the target stage's
declared-dependency outputs from a previous run directory; without it,
dependencies are bound to placeholder strings so a fresh iteration loop
still produces a complete prompt.`

// buildRenderPrior constructs the prior-stage output map exposed to a
// stage's prompt template, given the stage's declared Inputs.
//
// When runDir is non-empty we resolve each dep's output path (by rendering
// the dep's Output template against the same CLI inputs) and read the file
// off disk. Missing files / unrenderable Output templates surface as a
// clear error so the user knows which dep is unavailable. When runDir is
// empty we synthesise a deterministic placeholder string per dep so the
// final render still proceeds — placeholders are intentionally distinct
// from any plausible real output (suffix "-output-placeholder") so a user
// who copy-pastes the rendered prompt into an LLM notices the placeholder.
func buildRenderPrior(p *vamp.Pipeline, target *vamp.Stage, inputs map[string]string, runDir string) (map[string]*vamp.StageResult, error) {
	prior := make(map[string]*vamp.StageResult, len(target.Inputs))
	if runDir == "" {
		for _, dep := range target.Inputs {
			prior[dep] = &vamp.StageResult{Output: dep + "-output-placeholder"}
		}
		return prior, nil
	}
	// Build a quick stage lookup so we can resolve each dep's Output
	// template. Validate has already guaranteed every declared dep id
	// matches a real stage, so a miss here would indicate the pipeline
	// drifted between resolve and now — flag it loudly.
	byID := make(map[string]*vamp.Stage, len(p.Stages))
	for i := range p.Stages {
		byID[p.Stages[i].ID] = &p.Stages[i]
	}
	for _, dep := range target.Inputs {
		depStage, ok := byID[dep]
		if !ok {
			return nil, fmt.Errorf("dependency %q referenced by stage %q does not exist (pipeline drift)", dep, target.ID)
		}
		// Render the dep's output path against the same CLI inputs. The
		// dep's *own* deps render as empty placeholders because we only
		// need the path, not the nested template's resolved content; the
		// dep's Output template only references inputs / its own id in
		// practice. If a dep's Output references a transitive stage that
		// hasn't been resolved we surface that as a clear error rather
		// than silently substituting empty.
		depPriorEmpty := make(map[string]*vamp.StageResult, len(depStage.Inputs))
		for _, inner := range depStage.Inputs {
			depPriorEmpty[inner] = &vamp.StageResult{Output: ""}
		}
		path, err := vamp.RenderStageOutputPath(depStage, inputs, depPriorEmpty, runDir)
		if err != nil {
			return nil, fmt.Errorf("resolve output path for dep %q: %w", dep, err)
		}
		abs := filepath.Join(runDir, path)
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read prior output for dep %q (%s): %w", dep, abs, err)
		}
		prior[dep] = &vamp.StageResult{Output: string(data)}
	}
	return prior, nil
}
