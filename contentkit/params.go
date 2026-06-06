package contentkit

import (
	"time"

	"github.com/gallowaysoftware/vibe/vamp"
)

// thinkingOff suppresses Qwen3's chain-of-thought preamble, which otherwise
// lands in the output (and trips JSON gates). Every long-form text stage across
// the apps sets this.
var thinkingOff = map[string]any{"enable_thinking": false}

// DefaultRetry is the standard policy for generative text stages: a few attempts,
// backing off, retrying transient errors and invalid (e.g. non-JSON) output.
func DefaultRetry() *vamp.RetryPolicy {
	return &vamp.RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 5 * time.Second,
		MaxBackoff:     30 * time.Second,
		RetryOn:        []string{"transient", "invalid_output"},
	}
}

// LongFormText starts a long_form text stage with the standard sampler shape
// (the given temperature + token budget, chain-of-thought off) and the default
// retry policy — the boilerplate every generative text stage in worldsmith,
// brainrot, and textbook-to-audiobook repeats. The caller chains the rest
// (.PromptFS / .After / .Output / .OutputFormatJSON / extra .Param).
func LongFormText(p *vamp.Pipeline, name string, temp float64, maxTokens int) *vamp.TextStage {
	return p.Text(name).
		Capability("long_form").
		Param("temperature", temp).
		Param("max_tokens", maxTokens).
		Param("chat_template_kwargs", thinkingOff).
		Retry(DefaultRetry())
}
