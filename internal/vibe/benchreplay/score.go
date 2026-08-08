package benchreplay

import (
	"sort"

	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
)

// The scorer, and the only type that leaves this package.
//
// Every field below is a count, an instant, a def name, a metric name from
// modelprobe's constants, or a string from an enumerated set declared in
// shape.go. There is no []byte, no map, no interface and no free text, and
// TestReportCannotCarryABody walks the whole graph with an explicit field
// allowlist so that ADDING a field is what turns it red.

// Report is what a replay run produces.
type Report struct {
	Cell      string      `json:"cell"`
	Sample    SampleStats `json:"sample"`
	Incumbent SideScore   `json:"incumbent"`
	Candidate SideScore   `json:"candidate"`
	Paired    PairedScore `json:"paired"`
	// Divergence is the recorded response used correctly: as a noise
	// floor, never as a text-similarity score.
	Divergence DivergenceScore `json:"divergence"`
	// Rows is the per-request table, populated only when n is below the
	// rate floor. A proportion on n = 3 is noise wearing a percent sign;
	// the individual rows are still worth reading.
	Rows []RequestScore `json:"rows,omitempty"`
}

// SampleStats is the arithmetic of the harvest. Every figure carries its
// n, and n is not the operator's to choose — it is whatever survived the
// FIFO buffer.
type SampleStats struct {
	Walked       int `json:"walked"`
	Candidates   int `json:"candidates"`
	SkippedBasis int `json:"skipped_basis"`
	Evicted      int `json:"evicted"`
	Malformed    int `json:"malformed"`
	Replayable   int `json:"replayable"`
	Floor        int `json:"floor"`
}

// SideScore is one model over the whole sample. Counts, plus rates that
// are observed.Values because a rate below the floor is UNKNOWN and never
// zero.
type SideScore struct {
	Model  string `json:"model"`
	Metric string `json:"metric,omitempty"`

	Requests int `json:"requests"`
	Answered int `json:"answered"`
	Failed   int `json:"failed"`

	ToolsOffered     int `json:"tools_offered"`
	ToolCalled       int `json:"tool_called"`
	ToolDeclared     int `json:"tool_declared"`
	ToolUndeclared   int `json:"tool_undeclared"`
	ArgsValidJSON    int `json:"args_valid_json"`
	ArgsHaveRequired int `json:"args_have_required"`

	SchemaAsked     int `json:"schema_asked"`
	SchemaConformed int `json:"schema_conformed"`

	FinishStop      int `json:"finish_stop"`
	FinishLength    int `json:"finish_length"`
	FinishToolCalls int `json:"finish_tool_calls"`
	FinishFilter    int `json:"finish_filter"`
	FinishOther     int `json:"finish_other"`
	FinishNone      int `json:"finish_none"`

	Empty     int `json:"empty"`
	Truncated int `json:"truncated"`
	OutTokens int `json:"out_tokens"`

	// ToolCallRate is calls / offers. Unknown under the floor, and unknown
	// when nothing offered tools — which is a different thing from a model
	// that was offered tools and called none.
	ToolCallRate observed.Value[float64] `json:"tool_call_rate"`
	// ArgsValidRate is valid-JSON arguments / calls made.
	ArgsValidRate observed.Value[float64] `json:"args_valid_rate"`
	// FailureRate is requests that produced no readable answer / requests.
	FailureRate observed.Value[float64] `json:"failure_rate"`
}

// Reasons a paired ratio exists or does not. A closed set: the report
// never carries a sentence somebody wrote at a call site.
const (
	PairedOK           = "paired"
	PairedMixedMetrics = "mixed-metrics"
	PairedNoPairs      = "no-pairs"
	PairedUnderFloor   = "under-floor"
)

// PairedFloor is the smallest n for which a paired SCALAR is printed, and
// it is C8's five rather than DefaultRateFloor's twenty.
//
// The two floors are different on purpose and §5 says why: C8 wants five
// samples before a paired scalar means anything, and a PROPORTION needs
// more than a scalar does. A median of three ratios is a number three
// requests decided; a tool-call rate on three requests is noise wearing a
// percent sign, and the second is worse.
const PairedFloor = 5

// PairedScore is the throughput half. Both sides saw the identical
// sample, so prompt-length composition cancels exactly.
type PairedScore struct {
	// N is the number of requests where BOTH sides produced a rate.
	N int `json:"n"`
	// MedianRatio is the median of per-request ratios, never the ratio of
	// means: one 100k-token request in the sample would otherwise decide
	// the answer on its own.
	MedianRatio observed.Value[float64] `json:"median_ratio"`
	// Why names why there is or is not a ratio, from the closed set above.
	Why string `json:"why"`
}

// DivergenceScore is the one honest use of the recorded production
// response: as a NOISE FLOOR.
//
// The captured request carries the client's own temperature, so replaying
// it through the very model that produced the recorded response yields
// different text — the floor is not zero and is not knowable in advance.
// So the incumbent's structural disagreement with its OWN recorded output
// is measured, and the candidate's is reported only as an excess above it.
// When the candidate is not above the floor there is NO divergence claim.
type DivergenceScore struct {
	// N counts samples where the recorded response could be reduced to a
	// shape at all and both replays produced one. Non-200 rows have no
	// recorded response body at all, and they are the most interesting
	// requests in a sample.
	N                  int `json:"n"`
	IncumbentDisagreed int `json:"incumbent_disagreed"`
	CandidateDisagreed int `json:"candidate_disagreed"`
	// Claimed is false whenever the candidate did not exceed the floor, or
	// n is 0. Excess is unknown in exactly those cases.
	Claimed bool                    `json:"claimed"`
	Excess  observed.Value[float64] `json:"excess"`
}

// RequestScore is one replayed request, for the table printed under the
// rate floor. It carries the activity id — a number — and nothing else
// that came from the capture.
type RequestScore struct {
	ActivityID int64 `json:"activity_id"`

	IncumbentStatus int     `json:"incumbent_status"`
	CandidateStatus int     `json:"candidate_status"`
	IncumbentTokS   float64 `json:"incumbent_tok_s"`
	CandidateTokS   float64 `json:"candidate_tok_s"`

	// Tool and Finish are the closed sets from shape.go. An undeclared
	// tool NAME never appears; the marker does.
	IncumbentTool   string `json:"incumbent_tool"`
	CandidateTool   string `json:"candidate_tool"`
	IncumbentFinish string `json:"incumbent_finish"`
	CandidateFinish string `json:"candidate_finish"`
}

func (s *Set) score(cell string, inc, cand replayResult) *Report {
	r := &Report{
		Cell:      cell,
		Sample:    s.stats,
		Incumbent: sideScore(inc, s.items, s.opt.RateFloor),
		Candidate: sideScore(cand, s.items, s.opt.RateFloor),
	}
	r.Paired = pairedScore(inc, cand)
	r.Divergence = divergenceScore(s.items, inc, cand)
	if len(s.items) < s.opt.RateFloor {
		r.Rows = rows(s.items, inc, cand)
	}
	return r
}

func sideScore(res replayResult, items []sample, floor int) SideScore {
	sc := SideScore{Model: res.model, Metric: res.metric, Requests: len(res.shapes)}
	for i, sh := range res.shapes {
		if !sh.known {
			sc.Failed++
			continue
		}
		sc.Answered++
		sc.OutTokens += sh.outTokens
		if items[i].facts.declaresTools {
			sc.ToolsOffered++
		}
		if sh.hasToolCall {
			sc.ToolCalled++
			switch sh.toolOutcome {
			case ToolDeclared:
				sc.ToolDeclared++
			case ToolUndeclared:
				sc.ToolUndeclared++
			}
			if sh.argsValid {
				sc.ArgsValidJSON++
			}
			if sh.argsRequired {
				sc.ArgsHaveRequired++
			}
		}
		if sh.schemaWanted {
			sc.SchemaAsked++
			if sh.schemaOK {
				sc.SchemaConformed++
			}
		}
		switch sh.finish {
		case FinishStop:
			sc.FinishStop++
		case FinishLength:
			sc.FinishLength++
		case FinishToolCalls:
			sc.FinishToolCalls++
		case FinishFilter:
			sc.FinishFilter++
		case FinishNone:
			sc.FinishNone++
		default:
			sc.FinishOther++
		}
		if sh.empty {
			sc.Empty++
		}
		if sh.truncated {
			sc.Truncated++
		}
	}
	// The rates. Under the floor they stay UNKNOWN, which is the whole
	// reason they are observed.Values: a proportion on n = 3 rendered as
	// "0%" is a claim nobody measured, and this repo's most-repeated
	// defect class is absent evidence read as a healthy value.
	if sc.Requests >= floor {
		if sc.ToolsOffered > 0 {
			sc.ToolCallRate = observed.Known(float64(sc.ToolCalled) / float64(sc.ToolsOffered))
		}
		if sc.ToolCalled > 0 {
			sc.ArgsValidRate = observed.Known(float64(sc.ArgsValidJSON) / float64(sc.ToolCalled))
		}
		if sc.Requests > 0 {
			sc.FailureRate = observed.Known(float64(sc.Failed) / float64(sc.Requests))
		}
	}
	return sc
}

func pairedScore(inc, cand replayResult) PairedScore {
	p := PairedScore{Why: PairedNoPairs}
	// A side that measured NOTHING is not a side whose metric disagreed —
	// it is a side with no evidence, and saying "the metrics differed"
	// would describe a comparison nobody could attempt as a comparison
	// that was attempted and refused.
	if inc.metric == "" || cand.metric == "" {
		return p
	}
	// A side that fell back to wall-clock timing on some requests and read
	// llama.cpp's own timings on others is not one measurement, and two
	// sides that used DIFFERENT metrics are not comparable at all. C8
	// keeps the metric in the baseline key for exactly this reason; the
	// same rule has to hold on the surface a human reads.
	if inc.metric == metricMixed || cand.metric == metricMixed || inc.metric != cand.metric {
		p.Why = PairedMixedMetrics
		return p
	}
	var ratios []float64
	n := min(len(inc.tokS), len(cand.tokS))
	for i := range n {
		if inc.tokS[i] > 0 && cand.tokS[i] > 0 {
			ratios = append(ratios, cand.tokS[i]/inc.tokS[i])
		}
	}
	p.N = len(ratios)
	if len(ratios) == 0 {
		return p
	}
	if len(ratios) < PairedFloor {
		// The ratio exists and is not printed. C8's five-sample minimum is
		// the reason, and it applies here for C8's reason: below it the
		// number is what two or three requests happened to do.
		p.Why = PairedUnderFloor
		return p
	}
	p.Why = PairedOK
	p.MedianRatio = observed.Known(median(ratios))
	return p
}

func divergenceScore(items []sample, inc, cand replayResult) DivergenceScore {
	var d DivergenceScore
	n := min(len(inc.shapes), len(cand.shapes))
	for i := range n {
		rec := items[i].recorded
		// Three-way requirement. A recorded shape nobody could reduce, or
		// a replay that produced no answer, is not evidence of agreement
		// OR of disagreement — it is counted out of n and out of both
		// numerators.
		if !rec.known || !inc.shapes[i].known || !cand.shapes[i].known {
			continue
		}
		d.N++
		if disagree(rec, inc.shapes[i]) {
			d.IncumbentDisagreed++
		}
		if disagree(rec, cand.shapes[i]) {
			d.CandidateDisagreed++
		}
	}
	if d.N == 0 {
		return d
	}
	floor := float64(d.IncumbentDisagreed) / float64(d.N)
	got := float64(d.CandidateDisagreed) / float64(d.N)
	if got <= floor {
		// The candidate is inside the noise the incumbent makes against
		// its own recorded output. There is no divergence claim to make,
		// and the report prints none.
		return d
	}
	d.Claimed = true
	d.Excess = observed.Known(got - floor)
	return d
}

func rows(items []sample, inc, cand replayResult) []RequestScore {
	n := min(len(inc.shapes), len(cand.shapes))
	out := make([]RequestScore, 0, n)
	for i := range n {
		out = append(out, RequestScore{
			ActivityID:      items[i].activityID,
			IncumbentStatus: inc.shapes[i].status,
			CandidateStatus: cand.shapes[i].status,
			IncumbentTokS:   inc.tokS[i],
			CandidateTokS:   cand.tokS[i],
			IncumbentTool:   inc.shapes[i].toolOutcome,
			CandidateTool:   cand.shapes[i].toolOutcome,
			IncumbentFinish: inc.shapes[i].finish,
			CandidateFinish: cand.shapes[i].finish,
		})
	}
	return out
}

// median is the middle value, averaging the two middles on an even count.
func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
