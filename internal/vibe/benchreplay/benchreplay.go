// Package benchreplay replays a cell's own recent requests through a
// candidate model and emits only scores (fleet-control C25).
//
// It is the sampler half of `vibe model try --replay`. C18 measures both
// sides with one canned deterministic prompt and says, in its own report,
// what that cannot answer: "throughput is not quality. A faster model that
// tool-calls worse is a worse model, and no probe in this repo measures
// that." This package measures that, against the workload the box
// actually serves.
//
// # The privacy invariant, as a mechanism
//
// The bytes this package reads are the most private objects in the fleet.
// llama-swap's GET /api/captures/{id} answers the verbatim request and
// response of one metered request — the user's prompt, the system prompt,
// the tool definitions, the whole completion — and its redaction covers
// five HEADER names and nothing in the bodies at all.
//
// "Replay in place, emit only scores" is therefore five mechanisms rather
// than a comment, because this project's repeated lesson is that a prose
// rule is treated as advisory and a mechanical guarantee belongs at the
// boundary:
//
//  1. A result type that cannot carry a body. Report has no []byte, no
//     map, no interface and no string field that is not drawn from a
//     closed set. Gated by a reflection walk with an explicit field
//     allowlist, so ADDING A FIELD is what turns the test red
//     (TestReportCannotCarryABody).
//  2. This package cannot write a file. No os writer is called and no
//     writer is imported, gated by an AST scan
//     (TestPackageWritesNoFile). The leak it prevents is one line long
//     and looks helpful: "cache the sample so a resume doesn't
//     re-harvest".
//  3. Bodies become shapes at the boundary, once. The scorer takes a
//     shape, never a capture, and the []byte is not reachable from
//     anything downstream. "Emit only scores" then holds by the shape of
//     the call graph rather than by discipline at each print site.
//  4. No capture text reaches any output, including the error paths. A
//     failure mentioning a capture names its activity id and its HTTP
//     status and nothing else. Even a tool NAME the request itself
//     declared is not echoed: a name is model-adjacent text, the
//     per-request table reports `none` / `declared` / `<undeclared>`, and
//     an undeclared name — which is model output, i.e. private traffic —
//     never leaves the process at all.
//  5. Nothing crosses a box. No announce field, no HTTP route, no MCP
//     tool, nothing sent to fleetd. The sample never leaves the process;
//     the scores never leave the box except as the operator's own
//     terminal output.
//
// # The sample is never resumable state
//
// A capture buffer is 10 MB of in-process RAM with FIFO eviction, and a
// `-watch-config` reload — which is exactly what C18's apply performs —
// allocates a fresh empty one. So at the moment the candidate first
// exists, the sample is gone: the harvest must precede the apply.
//
// That ordering constraint and the privacy invariant are the same
// constraint. The only place a sample can live across an apply is the
// memory of the one process running the command, and a sample that lives
// only there is a sample that cannot be leaked. Harvest refuses once the
// trial is applied, and Set is the only thing Run accepts, so the ordering
// is a property of the type graph rather than of a comment. If the command
// dies, the trial resumes without a replay and says why.
//
// # What it refuses to claim
//
// It cannot answer "is my incumbent getting worse over time" — each run's
// sample is different traffic, so a score kept across runs compares two
// workloads and calls the difference a model change. This package
// compares two models against one sample in one run and stores no
// baseline. It is not a health check: a bad score changes a display,
// never a config, never an alias, never residency (C8's cardinal rule).
// And it never turns captures on: how much private traffic a box retains
// is a DECLARATION, and a benchmark that quietly widened it would have
// taken an intent decision that belongs to the operator.
package benchreplay

import (
	"errors"
	"net/http"
	"time"
)

// DefaultRateFloor is the smallest n for which this package will print a
// PROPORTION. Below it the per-request table is printed and every rate
// reads unknown.
//
// It is a judgement, not a derived constant, and it is here rather than on
// a flag because n is not the operator's to choose. C8 wants five samples
// before a paired SCALAR means anything, and a proportion needs more than
// a scalar does: on n = 3 a single differing request moves a "tool-call
// rate" by 33 points. Twenty is the smallest number at which one
// disagreement is a visible minority rather than the answer.
const DefaultRateFloor = 20

// DefaultMaxSample bounds one run's cost. A replay is n REAL requests
// through the cell's own llama-swap, twice — once per side — at whatever
// max_tokens the captured requests themselves declared. The report says
// what that cost was; this constant is what stops it being unbounded.
//
// It selects the NEWEST n, deterministically. That is not the operator
// choosing a favourable sample: the ordering is llama-swap's own
// newest-first activity walk, and both the cap and the number available
// are printed.
const DefaultMaxSample = 40

// DefaultMaxPages bounds the activity walk. llama-swap's activity endpoint
// caps `limit` at 999, and the capture buffer holds on the order of 125
// rows, so a handful of pages is already past the end of anything
// fetchable.
const DefaultMaxPages = 6

// DefaultReplayTimeout bounds ONE replayed request. A captured agentic
// request carries the whole prior conversation in its own body and its own
// max_tokens, so this is generous — and a request that exceeds it is
// recorded as a FAILURE, never as a fast one and never as a slow number
// nobody measured.
const DefaultReplayTimeout = 5 * time.Minute

// activityLimit is llama-swap's own cap on the activity endpoint's `limit`
// parameter; asking for more is answered with fewer, silently.
const activityLimit = 999

// runningTimeout bounds the residency check. Same value C8 uses.
const runningTimeout = 5 * time.Second

// captureFetchTimeout bounds one GET /api/captures/{id}. The buffer is in
// RAM and the handler decompresses server-side; a fetch that takes longer
// than this is a cell in trouble, not a big capture.
const captureFetchTimeout = 30 * time.Second

// maxCaptureBytes bounds one decoded capture. A 100k-token agentic prompt
// is roughly 400 KB of JSON; 32 MB is far past any of them and stops a
// malformed length prefix from being an allocation.
const maxCaptureBytes = 32 << 20

// The refusals. Each is a sentinel so the caller can tell them apart, and
// each carries its own instruction: a silent degradation here would be a
// score computed from evidence nobody has.
var (
	// ErrCapturesDisabled: the cell declares captureBuffer: 0. Refused by
	// name, and the config is NOT written — see the package doc.
	ErrCapturesDisabled = errors.New("this cell's llama-swap config sets `captureBuffer: 0`, so it retains no request bodies and there is nothing to replay. That is a declaration about how much private traffic this box keeps, and no vibe verb may change it: set it through the router extras file yourself if you want replay (fleet-control C25 §4)")

	// ErrNoCaptures: the walk found no fetchable capture for the model.
	// n = 0 is a refusal, never a score of 0.
	ErrNoCaptures = errors.New("this cell has served nothing recently that is still in llama-swap's capture buffer, so there is no sample to replay. The buffer is ~10 MB of in-process RAM with FIFO eviction and it is emptied by every restart and every -watch-config reload; use the box for an hour and re-run")

	// ErrAlreadyApplied: the harvest was attempted after the apply. The
	// apply IS the reload that empties the buffer, so this is not a
	// scheduling preference.
	ErrAlreadyApplied = errors.New("the sample must be harvested BEFORE the trial is applied: applying writes the cell's llama-swap config, and a -watch-config reload allocates a fresh, empty capture buffer. There is nothing left to harvest at this point, and a replay of an empty buffer would be a score with no evidence behind it")

	// ErrNotResident: C8's cardinal rule. A replay never loads a model.
	ErrNotResident = errors.New("not resident on this cell; a replay must not load a model. Warm it first, with the verb that exists for warming")
)

// Options wires a harvest and the replay that follows it. Every seam is
// injected so the whole sequence runs against a swaptest double.
type Options struct {
	// LlamaSwapURL is this box's OWN llama-swap. Always loopback: a
	// capture must not cross a box, and neither may a replay.
	LlamaSwapURL string
	// ConfigPath is the cell's llama-swap config. It is READ, once, to
	// answer "does this cell declare captureBuffer: 0". It is never
	// written — this package cannot write a file at all.
	ConfigPath string

	HTTP *http.Client
	Now  func() time.Time

	// MaxSample, MaxPages, RateFloor and ReplayTimeout default to the
	// constants above.
	MaxSample     int
	MaxPages      int
	RateFloor     int
	ReplayTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxSample <= 0 {
		o.MaxSample = DefaultMaxSample
	}
	if o.MaxPages <= 0 {
		o.MaxPages = DefaultMaxPages
	}
	if o.RateFloor <= 0 {
		o.RateFloor = DefaultRateFloor
	}
	if o.ReplayTimeout <= 0 {
		o.ReplayTimeout = DefaultReplayTimeout
	}
	return o
}
