package contentkit

import "context"

// Scorer evaluates a piece of generated content on a domain-specific rubric and
// returns structured findings. It is the shared seam for MEASUREMENT: a
// fiction pipeline's prose metrics (slop density, not-x-but-y, repeated
// trigrams) and a short-video pipeline's laugh audit both conform to this
// shape, so a per-installment scorecard and
// quality-over-time tracking can be built once, over any app's scorers.
//
// Implementations live in each app (the rubrics are domain-specific); contentkit
// only owns the contract.
type Scorer interface {
	// Name identifies the scorer in a scorecard (e.g. "prose", "continuity", "laugh").
	Name() string
	// Score evaluates content and returns its result. A returned error means the
	// score could not be computed (not that the content failed) — callers treat
	// scoring as non-gating telemetry.
	Score(ctx context.Context, content []byte) (ScoreResult, error)
}

// ScoreResult is one scorer's verdict on one piece of content.
type ScoreResult struct {
	// Score is an optional normalised 0–100 quality number for trend tracking;
	// 0 when the scorer only reports violations/metrics.
	Score int `json:"score"`
	// Metrics are arbitrary named measurements (counts, rates) for the scorecard.
	Metrics map[string]float64 `json:"metrics,omitempty"`
	// Violations are specific, located problems found in the content.
	Violations []Violation `json:"violations,omitempty"`
	// Summary is a one-line human verdict.
	Summary string `json:"summary,omitempty"`
}

// Violation is a single located finding.
type Violation struct {
	Severity string `json:"severity"` // "breaking" | "minor" | "watch" | "info"
	Message  string `json:"message"`
	Excerpt  string `json:"excerpt,omitempty"` // quoted offending text, when applicable
}

// Scorecard aggregates several scorers' results for one installment — the unit a
// quality-over-time view (goal: measurement, not vibes) is built from.
type Scorecard struct {
	Item    string        `json:"item"` // e.g. "quick-and-spar/002"
	Results []ScoreResult `json:"results"`
}
