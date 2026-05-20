package vamp

// This file exposes the executor's prompt / output-path rendering paths
// as package-public helpers so the `vamp render` CLI (and any future
// non-runtime callers — linters, IDE integrations, the dry-run validator)
// can render a single stage's templates without standing up a full
// Executor + vibe client. Everything here is a thin wrapper around the
// existing executor internals; the heavy lifting still lives in exec.go's
// renderTemplate / renderPrompt.

// StageResult is the public, render-only view of a stage's recorded
// outputs. It mirrors the package-private stageResult one-for-one so
// callers that need to seed prior outputs (for example: load a prior
// run's `.stages.<id>.output` from disk to drive `vamp render`) don't
// have to depend on the executor's internal type.
//
// Output is the canonical text exposed to dependents as
// `.stages.<id>.output`. Outputs is the per-item slice for foreach
// stages; non-foreach stages leave it nil and range-over is a no-op.
type StageResult struct {
	Output  string
	Outputs []string
}

// RenderStagePrompt renders the given stage's prompt (inline Prompt or
// PromptFile) against the supplied CLI inputs and prior-stage outputs.
// It is the same routine the executor's text/vision stages use; the
// only difference is no foreach injection — render is for one-shot
// prompt iteration, not per-item rendering.
//
// pipelineDir resolves PromptFile relative paths the same way the
// executor does. runDir is exposed to templates as `.runDir`; pass ""
// when the caller has no real run dir to advertise.
func RenderStagePrompt(st *Stage, pipelineDir string, cliInputs map[string]string, prior map[string]*StageResult, runDir string) (string, error) {
	return renderPrompt(st, pipelineDir, cliInputs, publicToPrivatePrior(prior), runDir, nil)
}

// RenderStageOutputPath renders the stage's Output template (the path,
// relative to RunDir, where the runner writes the stage's content)
// against the supplied CLI inputs and prior outputs. Useful for tooling
// that needs to know where the runner would have placed a stage's
// output without invoking the runner itself — e.g. the `vamp render`
// command resolving a dependency's prior output by reading the file at
// the rendered path.
func RenderStageOutputPath(st *Stage, cliInputs map[string]string, prior map[string]*StageResult, runDir string) (string, error) {
	return renderTemplate(st.ID+":output", st.Output, st.Inputs, cliInputs, publicToPrivatePrior(prior), runDir, nil)
}

// publicToPrivatePrior converts the public StageResult map into the
// executor's internal stageResult map so RenderStagePrompt /
// RenderStageOutputPath can call into renderTemplate without leaking
// the unexported type. The shapes are identical so this is a pure copy.
func publicToPrivatePrior(in map[string]*StageResult) map[string]*stageResult {
	if in == nil {
		return nil
	}
	out := make(map[string]*stageResult, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = &stageResult{}
			continue
		}
		out[k] = &stageResult{Output: v.Output, Outputs: v.Outputs}
	}
	return out
}
