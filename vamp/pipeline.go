// Package vamp is the public Go API for authoring vamp pipelines as code
// instead of YAML. A pipeline-as-a-Go-program imports this package, builds a
// *Pipeline via the fluent DSL in dsl.go, and hands it to Main to mount the
// same cobra subcommand tree (run / validate / viz / render / list / logs /
// …) the standalone vamp binary exposes.
//
// YAML pipelines continue to work unchanged through the standalone vamp
// binary; both authoring styles produce the same Pipeline value and feed the
// same orchestrator.
package vamp

import (
	internalvamp "github.com/gallowaysoftware/vibe/internal/vamp"
)

// Stage is a single step in a pipeline. The struct is a tagged union by
// Stage.Type plus a "field is meaningful for this type" convention enforced
// by Pipeline.Validate. The fluent DSL (TextStage, WebhookStage, …) hides
// that shape behind type-specific builders so YAML's per-type field-rejection
// rules become impossible-to-violate by construction.
type Stage = internalvamp.Stage

// StageType selects which executor handles a stage at run time.
type StageType = internalvamp.StageType

// InputSpec declares a pipeline-level input passed on the CLI as --input.
type InputSpec = internalvamp.InputSpec

// ForeachSpec is the structured fan-out descriptor for a stage.
type ForeachSpec = internalvamp.ForeachSpec

// RetryPolicy controls per-stage retry behaviour for transient executor
// failures.
type RetryPolicy = internalvamp.RetryPolicy

// AssertSpec turns a webhook stage into an assertion: the executor checks
// the response against these expectations and fails the stage with a
// descriptive error when any check fails.
type AssertSpec = internalvamp.AssertSpec

// Requirements is the runtime contract a pipeline declares: capabilities,
// services, GPU/disk hints, and notes. Pipeline binaries surface this via
// the `requirements` subcommand so tooling (vibe doctor) can preflight
// the host before kicking off a multi-hour run.
type Requirements = internalvamp.Requirements

// CapabilityRequirement names a vibe capability slot, the stages that
// depend on it, and an optional model hint.
type CapabilityRequirement = internalvamp.CapabilityRequirement

// ModelHint advertises what the prompts behind a capability assume of
// the underlying model.
type ModelHint = internalvamp.ModelHint

// ServiceRequirement names a non-vibe HTTP service the pipeline expects
// reachable on the host.
type ServiceRequirement = internalvamp.ServiceRequirement

// InputRequirement is the JSON-friendly view of an InputSpec emitted by
// the `requirements` subcommand.
type InputRequirement = internalvamp.InputRequirement

const (
	StageTypeText    = internalvamp.StageTypeText
	StageTypeComfyUI = internalvamp.StageTypeComfyUI
	StageTypeAudio   = internalvamp.StageTypeAudio
	StageTypeFFmpeg  = internalvamp.StageTypeFFmpeg
	StageTypeYouTube = internalvamp.StageTypeYouTube
	StageTypeWebhook = internalvamp.StageTypeWebhook
	StageTypeConfirm = internalvamp.StageTypeConfirm
	StageTypeRender  = internalvamp.StageTypeRender
	StageTypeCompact = internalvamp.StageTypeCompact
	StageTypePandoc  = internalvamp.StageTypePandoc
	StageTypeMix     = internalvamp.StageTypeMix
)

// AudioEngine* names the TTS backends an audio stage can select. Default
// is AudioEnginePiper (subprocess + .onnx voices on disk). Kokoro routes
// through an OpenAI-compatible HTTP endpoint — markedly more natural
// long-form narration; activated as a vibe profile so the daemon
// supervises the container.
const (
	AudioEnginePiper  = internalvamp.AudioEnginePiper
	AudioEngineKokoro = internalvamp.AudioEngineKokoro
)

const (
	RunWhenSuccess = internalvamp.RunWhenSuccess
	RunWhenFailure = internalvamp.RunWhenFailure
	RunWhenAlways  = internalvamp.RunWhenAlways
)

// DefaultForeachVar is the template variable name bound to each item of a
// foreach stage's array when ForeachSpec.Var is unset.
const DefaultForeachVar = internalvamp.DefaultForeachVar

// LoadPipeline reads, parses, and validates a pipeline YAML file and wraps
// the result in the public *Pipeline so the same value type drives both the
// YAML authoring path and the Go-DSL authoring path.
func LoadPipeline(path string) (*Pipeline, error) {
	inner, err := internalvamp.LoadPipeline(path)
	if err != nil {
		return nil, err
	}
	return &Pipeline{inner: inner}, nil
}
